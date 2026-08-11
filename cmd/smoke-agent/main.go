// Command smoke-agent is the federation agent: it polls a hub for its
// per-vantage assignment, measures the assigned targets with the same
// pluggable probes as smoked, and pushes results back to the hub with a
// bounded store-and-forward buffer.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"smokeping-modern/internal/agent"
	"smokeping-modern/internal/probe"

	// Register probe plugins (blank imports run their init() -> probe.Register),
	// so probe.New resolves every probe kind the hub might assign this agent.
	_ "smokeping-modern/internal/probe/dns"
	_ "smokeping-modern/internal/probe/fping"
	_ "smokeping-modern/internal/probe/httpprobe"
	_ "smokeping-modern/internal/probe/irttprobe"
	_ "smokeping-modern/internal/probe/pingprobe"
	_ "smokeping-modern/internal/probe/sshprobe"
	_ "smokeping-modern/internal/probe/tcpconnect"
)

// version is the smoke-agent release version. Overridable at build time with
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
var version = "1.0.0"

// agentConfig is the resolved smoke-agent configuration: the merge of an
// optional YAML file and CLI flag overrides, plus defaults.
type agentConfig struct {
	Hub      string
	Key      string
	Vantage  string
	Interval time.Duration
	Timeout  time.Duration
	Insecure bool
	Workers  int
	Buffer   int
	FlushMax int
	SpoolDir string
}

// fileConfig mirrors agentConfig for YAML decoding. Interval/Timeout are
// strings here because yaml.v3 does not parse "30s" into a time.Duration
// natively; resolveConfig parses them with time.ParseDuration after decode.
type fileConfig struct {
	Hub      string `yaml:"hub"`
	Key      string `yaml:"key"`
	Vantage  string `yaml:"vantage"`
	Interval string `yaml:"interval"`
	Timeout  string `yaml:"timeout"`
	Insecure bool   `yaml:"insecure"`
	Workers  int    `yaml:"workers"`
	Buffer   int    `yaml:"buffer"`
	FlushMax int    `yaml:"flush_max"`
	SpoolDir string `yaml:"spool_dir"`
}

// cliFlags carries the CLI flag overrides for resolveConfig. A zero value for a field
// means "flag not passed" and never overrides a value the config file provided. insecure
// is a *bool (nil = flag omitted) so an explicit `-insecure=false` can override a file's
// `insecure: true` — a plain bool couldn't distinguish "false" from "not passed"
// (CodeRabbit #5). spoolDir is a *string for the same reason: an explicit `-spool-dir=`
// must be able to disable a file's `spool_dir` (select in-memory mode), which a plain
// string treated as "not passed" could not (CODE_REVIEW #3).
type cliFlags struct {
	hub, key, vantage string
	interval, timeout time.Duration
	insecure          *bool
	workers, buffer   int
	flushMax          int
	spoolDir          *string
}

// resolveConfig builds the effective agentConfig: it starts from the YAML file at path
// (if non-empty), applies any non-zero flag overrides on top, fills in defaults for
// anything still unset, and then runs ONE final validation over the fully-merged result.
// Merging before validating is what makes the check authoritative: previously numeric
// flag overrides were applied in main() AFTER validation, so an invalid value passed by
// flag slipped through — including flush_max: -1, which panics peekBatch's make([], -1)
// (CODE_REVIEW #9 / P2-9). The YAML decode is strict (KnownFields), so a misspelled key
// is a startup error, not a silently-ignored setting.
func resolveConfig(path string, f cliFlags) (agentConfig, error) {
	var fc fileConfig
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return agentConfig{}, fmt.Errorf("reading config %s: %w", path, err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&fc); err != nil && !errors.Is(err, io.EOF) { // EOF = empty file, treat as no settings
			return agentConfig{}, fmt.Errorf("parsing config %s: %w", path, err)
		}
		// Reject a trailing YAML document: KnownFields only guards the first document, so a
		// second `---` doc (e.g. one holding a misspelled key) would otherwise be silently
		// ignored, making a typo'd setting look applied when it isn't (CodeRabbit #6).
		if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
			if err != nil {
				return agentConfig{}, fmt.Errorf("parsing config %s: %w", path, err)
			}
			return agentConfig{}, fmt.Errorf("parsing config %s: multiple YAML documents are not supported", path)
		}
	}

	cfg := agentConfig{
		Hub:      fc.Hub,
		Key:      fc.Key,
		Vantage:  fc.Vantage,
		Insecure: fc.Insecure,
		Workers:  fc.Workers,
		Buffer:   fc.Buffer,
		FlushMax: fc.FlushMax,
		SpoolDir: fc.SpoolDir,
	}
	if fc.Interval != "" {
		d, err := time.ParseDuration(fc.Interval)
		if err != nil {
			return agentConfig{}, fmt.Errorf("config %s: invalid interval %q: %w", path, fc.Interval, err)
		}
		cfg.Interval = d
	}
	if fc.Timeout != "" {
		d, err := time.ParseDuration(fc.Timeout)
		if err != nil {
			return agentConfig{}, fmt.Errorf("config %s: invalid timeout %q: %w", path, fc.Timeout, err)
		}
		cfg.Timeout = d
	}

	// Merge ALL non-zero flag values over the file, before any validation.
	if f.hub != "" {
		cfg.Hub = f.hub
	}
	if f.key != "" {
		cfg.Key = f.key
	}
	if f.vantage != "" {
		cfg.Vantage = f.vantage
	}
	if f.interval != 0 {
		cfg.Interval = f.interval
	}
	if f.timeout != 0 {
		cfg.Timeout = f.timeout
	}
	if f.insecure != nil {
		cfg.Insecure = *f.insecure
	}
	if f.workers != 0 {
		cfg.Workers = f.workers
	}
	if f.buffer != 0 {
		cfg.Buffer = f.buffer
	}
	if f.flushMax != 0 {
		cfg.FlushMax = f.flushMax
	}
	if f.spoolDir != nil {
		cfg.SpoolDir = *f.spoolDir
	}

	// Defaults for anything still unset (zero). A negative value is NOT zero, so it skips
	// the default and is caught by validation below rather than being silently replaced.
	if cfg.Interval == 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 4 * time.Second
	}
	if cfg.Workers == 0 {
		cfg.Workers = 50
	}
	if cfg.Buffer == 0 {
		cfg.Buffer = 100000
	}
	if cfg.FlushMax == 0 {
		cfg.FlushMax = 5000
	}

	// Single authoritative validation over the fully-merged config.
	if u, err := url.Parse(cfg.Hub); err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return agentConfig{}, fmt.Errorf("hub must be an absolute http(s) URL, got %q", cfg.Hub)
	}
	if cfg.Key == "" {
		return agentConfig{}, fmt.Errorf("key is required")
	}
	if cfg.Interval <= 0 {
		return agentConfig{}, fmt.Errorf("interval must be positive, got %s", cfg.Interval)
	}
	if cfg.Timeout <= 0 {
		return agentConfig{}, fmt.Errorf("timeout must be positive, got %s", cfg.Timeout)
	}
	if cfg.Workers < 1 {
		return agentConfig{}, fmt.Errorf("workers must be at least 1, got %d", cfg.Workers)
	}
	if cfg.Buffer < 1 {
		return agentConfig{}, fmt.Errorf("buffer must be at least 1, got %d", cfg.Buffer)
	}
	if cfg.FlushMax < 1 {
		return agentConfig{}, fmt.Errorf("flush_max must be at least 1, got %d", cfg.FlushMax)
	}

	return cfg, nil
}

func main() {
	configPath := flag.String("config", "", "path to a YAML config file")
	hub := flag.String("hub", "", "hub base URL, e.g. https://hub.example (overrides config file)")
	key := flag.String("key", "", "vantage API key (overrides config file)")
	vantage := flag.String("vantage", "", "vantage name this agent measures as (overrides config file)")
	interval := flag.Duration("interval", 0, "assignment poll interval (overrides config file; default 60s)")
	timeout := flag.Duration("timeout", 0, "per-target probe timeout (overrides config file; default 4s)")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification when talking to the hub")
	workers := flag.Int("workers", 0, "max concurrent probes (overrides config file; default 50)")
	buffer := flag.Int("buffer", 0, "bounded store-and-forward buffer capacity in rounds (overrides config file; default 100000)")
	flushMax := flag.Int("flush-max", 0, "max rounds per push to the hub (overrides config file; default 5000)")
	spoolDir := flag.String("spool-dir", "", "on-disk store-and-forward directory (overrides config file; empty = in-memory only)")
	logFormat := flag.String("log-format", "text", "operational log format: text or json")
	logLevel := flag.String("log-level", "info", "operational log level: debug, info, warn, error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("smoke-agent %s\n", version)
		return
	}

	setupLogger(*logFormat, *logLevel)

	// Pass insecure and spool-dir only when the flag was explicitly set, so `-insecure=false`
	// / `-spool-dir=` override a file's value while an omitted flag leaves the file value alone
	// (CodeRabbit #5; CODE_REVIEW #3).
	var insecureOverride *bool
	var spoolDirOverride *string
	flag.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "insecure":
			insecureOverride = insecure
		case "spool-dir":
			spoolDirOverride = spoolDir
		}
	})

	cfg, err := resolveConfig(*configPath, cliFlags{
		hub: *hub, key: *key, vantage: *vantage,
		interval: *interval, timeout: *timeout, insecure: insecureOverride,
		workers: *workers, buffer: *buffer, flushMax: *flushMax, spoolDir: spoolDirOverride,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoke-agent: %v\n", err)
		os.Exit(1)
	}

	slog.Info("smoke-agent starting", "hub", cfg.Hub, "vantage", cfg.Vantage, "probes", strings.Join(probe.Registered(), ", "))

	opts := agent.Options{
		Hub:       cfg.Hub,
		Key:       cfg.Key,
		Vantage:   cfg.Vantage,
		Insecure:  cfg.Insecure,
		Interval:  cfg.Interval,
		Timeout:   cfg.Timeout,
		Workers:   cfg.Workers,
		BufferCap: cfg.Buffer,
		FlushMax:  cfg.FlushMax,
		SpoolDir:  cfg.SpoolDir,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := agent.New(opts).Run(ctx); err != nil {
		slog.Error("smoke-agent exited with error", "err", err)
		os.Exit(1)
	}
}

// setupLogger installs the process-wide structured logger for operational
// events (lifecycle, poll/flush errors, per-batch summaries). json is for log
// aggregators; text for a console. Mirrors cmd/smoked's helper.
func setupLogger(format, level string) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.ToLower(format) == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}
