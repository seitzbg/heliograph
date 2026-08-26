// Command smoke-agent is the federation agent: it polls a hub for its
// per-vantage assignment, measures the assigned targets with the same
// pluggable probes as smoked, and pushes results back to the hub with a
// bounded store-and-forward buffer.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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

	"github.com/seitzbg/heliograph/internal/agent"
	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/probe/ntpprobe"

	// Register every probe plugin the hub might assign this agent, via the shared list both
	// binaries use — so the hub and agent can't drift apart (CODE_REVIEW M2).
	_ "github.com/seitzbg/heliograph/internal/probe/allprobes"
)

// version is the smoke-agent release version. Unset in an unversioned build (a plain `go build`
// must not claim a release); overridable at build time with
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
var version = "dev"

// agentConfig is the resolved smoke-agent configuration: the merge of an
// optional YAML file and CLI flag overrides, plus defaults.
type agentConfig struct {
	Hub string
	// ClientCert, ClientKey, and CACert hold inline PEM contents (not file paths) — either
	// decoded straight from the YAML config's client_cert/client_key/ca_cert fields, or read
	// from a file when the corresponding -client-cert/-client-key/-ca-cert flag is set.
	ClientCert string
	ClientKey  string
	CACert     string
	Vantage    string
	Interval   time.Duration
	Timeout    time.Duration
	Insecure   bool
	Workers    int
	Buffer     int
	FlushMax   int
	SpoolDir   string
}

// fileConfig mirrors agentConfig for YAML decoding. Interval/Timeout are
// strings here because yaml.v3 does not parse "30s" into a time.Duration
// natively; resolveConfig parses them with time.ParseDuration after decode.
// ClientCert/ClientKey/CACert carry inline PEM — these exact yaml tags match
// RenderAgentYAML's onboarding-bundle contract byte for byte, so a downloaded
// agent.yaml just works.
type fileConfig struct {
	Hub        string `yaml:"hub"`
	ClientCert string `yaml:"client_cert"`
	ClientKey  string `yaml:"client_key"`
	CACert     string `yaml:"ca_cert"`
	Vantage    string `yaml:"vantage"`
	Interval   string `yaml:"interval"`
	Timeout    string `yaml:"timeout"`
	Insecure   bool   `yaml:"insecure"`
	Workers    int    `yaml:"workers"`
	Buffer     int    `yaml:"buffer"`
	FlushMax   int    `yaml:"flush_max"`
	SpoolDir   string `yaml:"spool_dir"`
}

// cliFlags carries the CLI flag overrides for resolveConfig. A zero value for a field
// means "flag not passed" and never overrides a value the config file provided. insecure
// is a *bool (nil = flag omitted) so an explicit `-insecure=false` can override a file's
// `insecure: true` — a plain bool couldn't distinguish "false" from "not passed"
// (CodeRabbit #5). spoolDir is a *string for the same reason: an explicit `-spool-dir=`
// must be able to disable a file's `spool_dir` (select in-memory mode), which a plain
// string treated as "not passed" could not (CODE_REVIEW #3). clientCertPath/clientKeyPath/
// caCertPath are FILE PATHS (unlike the yaml config's inline PEM) — for operators who keep
// their mTLS material as separate files; resolveConfig reads them and overrides the
// corresponding PEM string on cfg.
type cliFlags struct {
	hub, vantage                              string
	clientCertPath, clientKeyPath, caCertPath string
	interval, timeout                         time.Duration
	insecure                                  *bool
	workers, buffer                           int
	flushMax                                  int
	spoolDir                                  *string
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
		Hub:        fc.Hub,
		ClientCert: fc.ClientCert,
		ClientKey:  fc.ClientKey,
		CACert:     fc.CACert,
		Vantage:    fc.Vantage,
		Insecure:   fc.Insecure,
		Workers:    fc.Workers,
		Buffer:     fc.Buffer,
		FlushMax:   fc.FlushMax,
		SpoolDir:   fc.SpoolDir,
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
	if f.clientCertPath != "" {
		data, err := os.ReadFile(f.clientCertPath)
		if err != nil {
			return agentConfig{}, fmt.Errorf("reading -client-cert %s: %w", f.clientCertPath, err)
		}
		cfg.ClientCert = string(data)
	}
	if f.clientKeyPath != "" {
		data, err := os.ReadFile(f.clientKeyPath)
		if err != nil {
			return agentConfig{}, fmt.Errorf("reading -client-key %s: %w", f.clientKeyPath, err)
		}
		cfg.ClientKey = string(data)
	}
	if f.caCertPath != "" {
		data, err := os.ReadFile(f.caCertPath)
		if err != nil {
			return agentConfig{}, fmt.Errorf("reading -ca-cert %s: %w", f.caCertPath, err)
		}
		cfg.CACert = string(data)
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
	if cfg.ClientCert == "" || cfg.ClientKey == "" {
		return agentConfig{}, fmt.Errorf("client_cert and client_key are required (mTLS)")
	}
	if cfg.CACert == "" && !cfg.Insecure {
		return agentConfig{}, fmt.Errorf("ca_cert is required unless -insecure")
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
	clientCertPath := flag.String("client-cert", "", "path to this vantage's mTLS client certificate PEM file (overrides config file's inline client_cert)")
	clientKeyPath := flag.String("client-key", "", "path to this vantage's mTLS client private key PEM file (overrides config file's inline client_key)")
	caCertPath := flag.String("ca-cert", "", "path to the hub CA certificate PEM file used to verify the hub (overrides config file's inline ca_cert)")
	vantage := flag.String("vantage", "", "vantage name this agent measures as (overrides config file)")
	interval := flag.Duration("interval", 0, "assignment poll interval (overrides config file; default 60s)")
	timeout := flag.Duration("timeout", 0, "per-target probe timeout (overrides config file; default 4s)")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification when talking to the hub (dev / self-signed only)")
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
		hub: *hub, vantage: *vantage,
		clientCertPath: *clientCertPath, clientKeyPath: *clientKeyPath, caCertPath: *caCertPath,
		interval: *interval, timeout: *timeout, insecure: insecureOverride,
		workers: *workers, buffer: *buffer, flushMax: *flushMax, spoolDir: spoolDirOverride,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "smoke-agent: %v\n", err)
		os.Exit(1)
	}

	slog.Info("smoke-agent starting", "hub", cfg.Hub, "vantage", cfg.Vantage, "probes", strings.Join(probe.Registered(), ", "))

	// Build the mTLS client identity: this vantage's client certificate always, plus
	// either the hub's CA pool (normal operation) or InsecureSkipVerify (opt-in dev /
	// self-signed setups only — never logs cfg.ClientCert/ClientKey/CACert).
	cert, err := tls.X509KeyPair([]byte(cfg.ClientCert), []byte(cfg.ClientKey))
	if err != nil {
		slog.Error("smoke-agent: invalid client_cert/client_key", "err", err)
		os.Exit(1)
	}
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if cfg.Insecure {
		tlsCfg.InsecureSkipVerify = true // #nosec G402 — opt-in dev/self-signed
	} else {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(cfg.CACert)) {
			slog.Error("smoke-agent: ca_cert: no valid certificate found")
			os.Exit(1)
		}
		tlsCfg.RootCAs = pool
	}

	opts := agent.Options{
		Hub:       cfg.Hub,
		TLSConfig: tlsCfg,
		Vantage:   cfg.Vantage,
		Interval:  cfg.Interval,
		Timeout:   cfg.Timeout,
		Workers:   cfg.Workers,
		BufferCap: cfg.Buffer,
		FlushMax:  cfg.FlushMax,
		SpoolDir:  cfg.SpoolDir,
		// Carry each NTP round's companion clock stat (offset/stratum) to the hub, so a remote
		// vantage's NTP server shows a real clock reading there instead of the hub's own (M2).
		NTPStat: ntpprobe.LatestFor,
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
