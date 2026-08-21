package proxyenv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/nestedbridge"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

func writeManifest(t *testing.T, env map[string]string, proxyEnvs []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sandbox.json")
	body, err := json.Marshal(map[string]any{"env": env, "proxyEnvs": proxyEnvs})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func TestRenderResolvesLocalSubnetsToken(t *testing.T) {
	path := writeManifest(t, map[string]string{
		"NO_PROXY":   "127.0.0.1,localhost,::1," + sandboxconfig.LocalSubnetsToken,
		"HTTP_PROXY": "http://127.0.0.1:17008",
	}, []string{"NO_PROXY", "HTTP_PROXY"})

	out, err := Render(path)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := string(out)

	if strings.Contains(got, sandboxconfig.LocalSubnetsToken) {
		t.Fatalf("token left unresolved: %q", got)
	}
	if want := `HTTP_PROXY="http://127.0.0.1:17008"` + "\n"; !strings.Contains(got, want) {
		t.Fatalf("missing %q\ngot:\n%s", want, got)
	}
	for _, cidr := range nestedbridge.LocalSubnets() {
		if !strings.Contains(got, cidr) {
			t.Fatalf("local subnet %s missing from %q", cidr, got)
		}
	}
	if !strings.Contains(got, "127.0.0.1") {
		t.Fatalf("literal exemptions were lost: %q", got)
	}
}

func TestRenderOnlyIncludesProxyEnvs(t *testing.T) {
	path := writeManifest(t, map[string]string{
		"NO_PROXY": "127.0.0.1",
		"SECRET":   "should-not-appear",
	}, []string{"NO_PROXY"})

	out, err := Render(path)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := string(out)
	if strings.Contains(got, "SECRET") {
		t.Fatalf("non-proxy env leaked into rendering: %q", got)
	}
	if want := `NO_PROXY="127.0.0.1"` + "\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderIsSorted(t *testing.T) {
	path := writeManifest(t, map[string]string{
		"NO_PROXY":   "127.0.0.1",
		"HTTP_PROXY": "http://127.0.0.1:17008",
	}, []string{"NO_PROXY", "HTTP_PROXY"})

	out, err := Render(path)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, line := range lines {
		name, _, _ := strings.Cut(line, "=")
		if i > 0 {
			prev, _, _ := strings.Cut(lines[i-1], "=")
			if prev > name {
				t.Fatalf("lines not sorted: %v", lines)
			}
		}
	}
}

func TestRenderNoProxyEnvsProducesNothing(t *testing.T) {
	path := writeManifest(t, map[string]string{"NO_PROXY": "127.0.0.1"}, nil)
	out, err := Render(path)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil for a sandbox with no proxy-trust vars, got %q", out)
	}
}

func TestRenderMissingSandboxJSONProducesNothing(t *testing.T) {
	out, err := Render(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil for a missing manifest, got %q", out)
	}
}

func TestWriteFileWritesQuotedValues(t *testing.T) {
	path := writeManifest(t, map[string]string{"NO_PROXY": "127.0.0.1"}, []string{"NO_PROXY"})
	out := filepath.Join(t.TempDir(), "nested", "proxy.env")

	if err := WriteFile(path, out); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if want := "NO_PROXY=" + strconv.Quote("127.0.0.1") + "\n"; string(data) != want {
		t.Fatalf("got %q, want %q", data, want)
	}
}

func TestWriteFileRemovesStaleOutputWhenNothingToRender(t *testing.T) {
	out := filepath.Join(t.TempDir(), "proxy.env")
	if err := os.WriteFile(out, []byte("STALE=\"leftover\"\n"), 0o644); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}
	path := writeManifest(t, map[string]string{"NO_PROXY": "127.0.0.1"}, nil)

	if err := WriteFile(path, out); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("expected stale output to be removed, stat err = %v", err)
	}
}
