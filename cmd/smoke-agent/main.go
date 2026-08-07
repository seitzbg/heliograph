// Command smoke-agent is the federation agent: it polls a hub for its
// per-vantage assignment, measures the assigned targets with the same
// pluggable probes as smoked, and pushes results back to the hub with a
// bounded store-and-forward buffer.
package main

import (
	"context"
	"flag"
	"fmt"
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
	_ "smokeping-modern/internal/probe/sshprobe"
	_ "smokeping-modern/internal/probe/tcpconnect"
)

// version is the smoke-agent release version. Overridable at build time with
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
var version = "0.1.0"

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
}

// resolveConfig builds the effective agentConfig: it starts from the YAML
// file at path (if non-empty), applies any non-zero flag overrides on top,
// fills in defaults for anything still unset, and validates the result. A
// zero-value flag argument (e.g. hubFlag == "") means "not passed" and never
// overrides a file value.
func resolveConfig(path, hubFlag, keyFlag string, intervalFlag, timeoutFlag time.Duration, insecureFlag bool) (agentConfig, error) {
	var fc fileConfig
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return agentConfig{}, fmt.Errorf("reading config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &fc); err != nil {
			return agentConfig{}, fmt.Errorf("parsing config %s: %w", path, err)
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

	// Non-zero flag values override the file. A flag left at its zero value
	// (unset) never clobbers a value the file provided.
	if hubFlag != "" {
		cfg.Hub = hubFlag
	}
	if keyFlag != "" {
		cfg.Key = keyFlag
	}
	if intervalFlag != 0 {
		cfg.Interval = intervalFlag
	}
	if timeoutFlag != 0 {
		cfg.Timeout = timeoutFlag
	}
	if insecureFlag {
		cfg.Insecure = true
	}

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

	if u, err := url.Parse(cfg.Hub); err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return agentConfig{}, fmt.Errorf("hub must be an absolute http(s) URL, got %q", cfg.Hub)
	}
	if cfg.Key == "" {
		return agentConfig{}, fmt.Errorf("key is required")
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
	logFormat := flag.String("log-format", "text", "operational log format: text or json")
	logLevel := flag.String("log-level", "info", "operational log level: debug, info, warn, error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("smoke-agent %s\n", version)
		return
	}

	setupLogger(*logFormat, *logLevel)

	cfg, err := resolveConfig(*configPath, *hub, *key, *interval, *timeout, *insecure)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoke-agent: %v\n", err)
		os.Exit(1)
	}
	if *vantage != "" {
		cfg.Vantage = *vantage
	}
	if *workers != 0 {
		cfg.Workers = *workers
	}
	if *buffer != 0 {
		cfg.Buffer = *buffer
	}
	if *flushMax != 0 {
		cfg.FlushMax = *flushMax
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
