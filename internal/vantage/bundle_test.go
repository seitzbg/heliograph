package vantage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	sampleCertPEM = "-----BEGIN CERTIFICATE-----\n" +
		"MIIBpTCCAUqgAwIBAgIQZmFrZS1jZXJ0LWJ5dGVzMAoGCCqGSM49BAMCMBAxDjAM\n" +
		"BgNVBAMTBWZha2UxMB4XDTI2MDgyNTAwMDAwMFoXDTM2MDgyNTAwMDAwMFowEDEO\n" +
		"MAwGA1UEAxMFZmFrZTIwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAATestCertFa\n" +
		"keFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFake\n" +
		"-----END CERTIFICATE-----\n"

	sampleKeyPEM = "-----BEGIN EC PRIVATE KEY-----\n" +
		"MHcCAQEEIFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeoAo\n" +
		"GCCqGSM49AwEHoUQDQgAETestKeyFakeFakeFakeFakeFakeFakeFakeFakeFake\n" +
		"FakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFake==\n" +
		"-----END EC PRIVATE KEY-----\n"

	sampleCAPEM = "-----BEGIN CERTIFICATE-----\n" +
		"MIIBQTCCAUUCCQDCaFakeCAFakeCAFakeCAFakeCAFakeCAFakeCAFakeCAFake\n" +
		"CAFakeCAFakeCAFakeCAFakeCAFakeCAFakeCAFakeCAFakeCAFakeCAFakeCA\n" +
		"-----END CERTIFICATE-----\n"
)

// agentFileConfig mirrors the smoke-agent's on-disk YAML fields (see
// cmd/smoke-agent/main.go fileConfig), minus the legacy `key` field which the
// mTLS bundle must NOT emit. Kept local to the test so this file is a
// standalone contract guard against RenderAgentYAML drifting from the shape
// a later task's agent config struct must parse.
type agentFileConfig struct {
	Hub        string `yaml:"hub"`
	Vantage    string `yaml:"vantage"`
	ClientCert string `yaml:"client_cert"`
	ClientKey  string `yaml:"client_key"`
	CACert     string `yaml:"ca_cert"`
	SpoolDir   string `yaml:"spool_dir"`
}

type legacyKeyConfig struct {
	Key string `yaml:"key"`
}

func TestRenderAgentYAMLRoundTrips(t *testing.T) {
	const hub = "https://heliograph.bsd-unix.net:8443"
	const name = "munro-comcast"

	out := RenderAgentYAML(hub, name, []byte(sampleCertPEM), []byte(sampleKeyPEM), []byte(sampleCAPEM))

	var got agentFileConfig
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("yaml.Unmarshal: %v\n--- output ---\n%s", err, out)
	}

	if got.Hub != hub {
		t.Errorf("Hub = %q, want %q", got.Hub, hub)
	}
	if got.Vantage != name {
		t.Errorf("Vantage = %q, want %q", got.Vantage, name)
	}
	if got.ClientCert != sampleCertPEM {
		t.Errorf("ClientCert round-trip mismatch:\ngot:  %q\nwant: %q", got.ClientCert, sampleCertPEM)
	}
	if got.ClientKey != sampleKeyPEM {
		t.Errorf("ClientKey round-trip mismatch:\ngot:  %q\nwant: %q", got.ClientKey, sampleKeyPEM)
	}
	if got.CACert != sampleCAPEM {
		t.Errorf("CACert round-trip mismatch:\ngot:  %q\nwant: %q", got.CACert, sampleCAPEM)
	}
	if got.SpoolDir != "/var/lib/smoke-agent/spool" {
		t.Errorf("SpoolDir = %q, want /var/lib/smoke-agent/spool", got.SpoolDir)
	}

	var legacy legacyKeyConfig
	if err := yaml.Unmarshal(out, &legacy); err != nil {
		t.Fatalf("yaml.Unmarshal (legacy check): %v", err)
	}
	if legacy.Key != "" {
		t.Errorf("output must not contain a `key:` field, got %q", legacy.Key)
	}
	if strings.Contains(string(out), "\nkey:") || strings.HasPrefix(string(out), "key:") {
		t.Errorf("output contains a literal `key:` line:\n%s", out)
	}
}

func TestRenderAgentCompose(t *testing.T) {
	out := string(RenderAgentCompose("munro-comcast"))

	for _, want := range []string{
		"entrypoint",
		"smoke-agent",
		"./agent.yaml:/etc/heliograph/agent.yaml:ro",
		"docker compose up -d",
		"agent-spool:/var/lib/smoke-agent/spool",
		"cap_add: [NET_RAW]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderAgentCompose output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteBundleTarGz(t *testing.T) {
	const hub = "https://heliograph.bsd-unix.net:8443"
	const name = "munro-comcast"

	var buf bytes.Buffer
	if err := WriteBundleTarGz(&buf, hub, name, []byte(sampleCertPEM), []byte(sampleKeyPEM), []byte(sampleCAPEM)); err != nil {
		t.Fatalf("WriteBundleTarGz: %v", err)
	}

	entries := untar(t, &buf)

	wantNames := []string{"agent.yaml", "docker-compose.yml", "README.txt"}
	for _, name := range wantNames {
		if _, ok := entries[name]; !ok {
			t.Errorf("bundle missing entry %q; got entries: %v", name, entryNames(entries))
		}
	}

	agentYAML := entries["agent.yaml"]
	if !strings.Contains(agentYAML, "hub:") {
		t.Errorf("agent.yaml missing `hub:`:\n%s", agentYAML)
	}
	if !strings.Contains(agentYAML, "BEGIN CERTIFICATE") {
		t.Errorf("agent.yaml missing cert PEM:\n%s", agentYAML)
	}

	compose := entries["docker-compose.yml"]
	if !strings.Contains(compose, "entrypoint") || !strings.Contains(compose, "smoke-agent") {
		t.Errorf("docker-compose.yml missing entrypoint/smoke-agent:\n%s", compose)
	}
	if !strings.Contains(compose, "./agent.yaml:") {
		t.Errorf("docker-compose.yml missing agent.yaml mount:\n%s", compose)
	}

	readme := entries["README.txt"]
	if !strings.Contains(readme, "docker compose up -d") {
		t.Errorf("README.txt missing run instructions:\n%s", readme)
	}
}

// untar gunzips and untars r, returning a map of entry name to file content.
func untar(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()

	entries := make(map[string]string)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		var content bytes.Buffer
		if _, err := io.Copy(&content, tr); err != nil {
			t.Fatalf("io.Copy: %v", err)
		}
		entries[hdr.Name] = content.String()
	}
	return entries
}

func entryNames(entries map[string]string) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	return names
}
