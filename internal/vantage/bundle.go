package vantage

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"time"

	"gopkg.in/yaml.v3"
)

// agentYAMLDoc is the on-disk shape of a vantage agent's config file, in the mTLS model: the
// legacy shared `key` field is gone, replaced by a per-vantage client cert/key plus the hub's
// CA cert. Field order here is emission order (yaml.v3 marshals struct fields in declaration
// order), chosen to read top-to-bottom as "where" then "who" then "trust" then "state".
//
// This struct — not hand-built YAML text — is the contract: a later task's smoke-agent
// fileConfig must decode exactly these keys. Marshaling a struct (rather than concatenating
// strings) is what guarantees the multi-line PEM fields come out as correctly indented YAML
// literal block scalars; yaml.v3 chooses the block style automatically for strings containing
// newlines.
type agentYAMLDoc struct {
	Hub        string `yaml:"hub"`
	Vantage    string `yaml:"vantage"`
	ClientCert string `yaml:"client_cert"`
	ClientKey  string `yaml:"client_key"`
	CACert     string `yaml:"ca_cert"`
	SpoolDir   string `yaml:"spool_dir"`
}

// RenderAgentYAML renders the smoke-agent config for a freshly minted vantage under the mTLS
// federation model: hub URL, vantage name, the vantage's client certificate + private key, and
// the hub's CA cert (so the agent can verify the hub in turn), plus the durable spool path the
// matching docker-compose.yml volume-mounts. This is the sole source of agent.yaml: the dashboard
// no longer builds it client-side, it just downloads the server-built tar.gz bundle this feeds
// (see WriteBundleTarGz below and the download handler in internal/api/api.go).
func RenderAgentYAML(hub, name string, certPEM, keyPEM, caPEM []byte) []byte {
	doc := agentYAMLDoc{
		Hub:        hub,
		Vantage:    name,
		ClientCert: string(certPEM),
		ClientKey:  string(keyPEM),
		CACert:     string(caPEM),
		SpoolDir:   "/var/lib/smoke-agent/spool",
	}

	body, err := yaml.Marshal(&doc)
	if err != nil {
		// yaml.Marshal only fails on unsupported types (channels, funcs, cyclic
		// structures); agentYAMLDoc is plain strings, so this is unreachable in
		// practice. Panicking (rather than threading an error through a function
		// this package's callers treat as pure) keeps the signature simple without
		// silently shipping a broken bundle.
		panic(fmt.Sprintf("vantage: marshal agent.yaml for %q: %v", name, err))
	}

	header := fmt.Sprintf("# smoke-agent config for vantage %q\n", name)
	return append([]byte(header), body...)
}

// RenderAgentCompose renders a ready-to-run docker-compose.yml for a vantage agent. It is
// identical for every vantage — the per-vantage data and secret live only in the mounted
// agent.yaml — so name is used solely for a header comment. Content mirrors the browser-side
// agentCompose() in web/dashboard.js:agentCompose (~line 804) verbatim; keep the two in sync if
// either changes.
func RenderAgentCompose(name string) []byte {
	return []byte(fmt.Sprintf(`# docker-compose.yaml — heliograph vantage agent (%s)
# Save next to agent.yaml (the other tab), then:  docker compose up -d
services:
  smoke-agent:
    image: ghcr.io/seitzbg/heliograph:latest
    entrypoint: ["smoke-agent"]
    command: ["-config", "/etc/heliograph/agent.yaml"]
    volumes:
      - ./agent.yaml:/etc/heliograph/agent.yaml:ro
      - agent-spool:/var/lib/smoke-agent/spool
    cap_add: [NET_RAW]
    sysctls:
      net.ipv4.ping_group_range: "0 10001"
    restart: unless-stopped
volumes:
  agent-spool: {}
`, name))
}

// renderReadme renders the plain-text onboarding README bundled alongside agent.yaml and
// docker-compose.yml.
func renderReadme(hub, name string) []byte {
	return []byte(fmt.Sprintf(`heliograph vantage: %s
========================================

This bundle configures a smoke-agent probe vantage named %q, reporting
measurements back to the hub at:

    %s

Contents:
  agent.yaml           vantage config — hub URL, vantage name, mTLS client
                        certificate/key, and the hub's CA cert
  docker-compose.yml   the agent service definition
  README.txt           this file

agent.yaml carries the vantage's private client key. Treat it like any other
credential: keep it out of version control, and make sure only the container
(and whoever administers this host) can read it, e.g.:

    chmod 600 agent.yaml

To run the agent:

  1. Unpack this bundle and cd into the directory containing agent.yaml and
     docker-compose.yml.
  2. Start the agent:

         docker compose up -d

  3. Check it's reporting:

         docker compose logs -f smoke-agent

The vantage will appear on the hub's dashboard once the agent has completed
its first successful push.
`, name, name, hub))
}

// bundleModTime is the fixed modification time stamped on every tar entry, so
// WriteBundleTarGz's output is byte-for-byte deterministic given the same inputs (no
// time.Now() dependency).
var bundleModTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// WriteBundleTarGz writes a gzip-compressed tar archive to w containing a vantage's complete
// onboarding bundle: agent.yaml, docker-compose.yml, and README.txt, in that order. It is the
// one-click download the hub's UI offers when a new vantage is registered.
func WriteBundleTarGz(w io.Writer, hub, name string, certPEM, keyPEM, caPEM []byte) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	files := []struct {
		name    string
		mode    int64
		content []byte
	}{
		// agent.yaml embeds the vantage's client PRIVATE KEY, so it extracts owner-only (0600); the
		// compose + README carry no secret and stay world-readable.
		{"agent.yaml", 0o600, RenderAgentYAML(hub, name, certPEM, keyPEM, caPEM)},
		{"docker-compose.yml", 0o644, RenderAgentCompose(name)},
		{"README.txt", 0o644, renderReadme(hub, name)},
	}

	for _, f := range files {
		hdr := &tar.Header{
			Name:     f.name,
			Mode:     f.mode,
			Size:     int64(len(f.content)),
			Typeflag: tar.TypeReg,
			ModTime:  bundleModTime,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("vantage: write bundle: tar header for %s: %w", f.name, err)
		}
		if _, err := tw.Write(f.content); err != nil {
			return fmt.Errorf("vantage: write bundle: tar content for %s: %w", f.name, err)
		}
	}

	// Close the tar writer first (flushes its trailer into the gzip stream), then the gzip
	// writer (flushes the compressed bytes to w) — closing in the other order would leave a
	// truncated, unreadable archive.
	if err := tw.Close(); err != nil {
		return fmt.Errorf("vantage: write bundle: close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("vantage: write bundle: close gzip: %w", err)
	}
	return nil
}
