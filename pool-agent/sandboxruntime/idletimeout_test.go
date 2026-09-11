package sandboxruntime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

func TestConfiguredSandboxIdleTimeout(t *testing.T) {
	t.Setenv(EnvSandboxIdleTimeout, "")
	if got, err := ConfiguredSandboxIdleTimeout(); err != nil || got != 0 {
		t.Fatalf("unset = %s, %v; want zero so the sandbox-agent default applies", got, err)
	}
	t.Setenv(EnvSandboxIdleTimeout, "2m0s")
	if got, err := ConfiguredSandboxIdleTimeout(); err != nil || got != 2*time.Minute {
		t.Fatalf("2m0s = %s, %v", got, err)
	}
	for _, bad := range []string{"soon", "0s", "-1m"} {
		t.Setenv(EnvSandboxIdleTimeout, bad)
		if _, err := ConfiguredSandboxIdleTimeout(); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}

// The pool writes its idle timeout into every sandbox.json it renders, and
// writes nothing when it has none, so the sandbox-agent's own default applies
// (ADR 0108 §3).
func TestSandboxDocumentCarriesThePoolIdleTimeout(t *testing.T) {
	req := &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}
	doc := buildSandboxDocument("project-1", "sandbox-1", "pool-1", "public-key", "sha256:image", 2*time.Minute, req, nil, nil)
	cfg, _ := sandboxconfig.Effective(doc)
	if cfg.AgentRuntime.IdleTimeout != "2m0s" {
		t.Fatalf("idle timeout = %q, want 2m0s", cfg.AgentRuntime.IdleTimeout)
	}
	doc = buildSandboxDocument("project-1", "sandbox-1", "pool-1", "public-key", "sha256:image", 0, req, nil, nil)
	cfg, _ = sandboxconfig.Effective(doc)
	if cfg.AgentRuntime.IdleTimeout != "" {
		t.Fatalf("idle timeout = %q with none configured, want empty", cfg.AgentRuntime.IdleTimeout)
	}
}

// The start path brings a sandbox created under an older timeout onto the
// pool's current one (ADR 0108 §3), touching nothing else in its document and
// rewriting nothing when the value already matches.
func TestApplyIdleTimeoutRewritesOnlyTheTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), sandboxDocumentName)
	req := &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}
	data, err := marshalSandboxDocument(buildSandboxDocument("project-1", "sandbox-1", "pool-1", "public-key", "sha256:image", 0, req, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	read := func() sandboxDocumentFile {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var file sandboxDocumentFile
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatal(err)
		}
		return file
	}

	if err := applyIdleTimeout(path, 2*time.Minute); err != nil {
		t.Fatalf("apply: %v", err)
	}
	file := read()
	if file.AgentRuntime.IdleTimeout != "2m0s" || file.Provenance.Runtime.AgentRuntime.IdleTimeout != "2m0s" {
		t.Fatalf("idle timeout = %q (provenance %q), want 2m0s", file.AgentRuntime.IdleTimeout, file.Provenance.Runtime.AgentRuntime.IdleTimeout)
	}
	if file.SandboxID != "sandbox-1" || file.AgentRuntime.ListenAddress == "" || file.Provider.PoolID != "pool-1" {
		t.Fatalf("the rest of the document changed: %+v", file.Config)
	}

	before, _ := os.ReadFile(path)
	if err := applyIdleTimeout(path, 2*time.Minute); err != nil {
		t.Fatalf("reapply: %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("an unchanged timeout rewrote the document")
	}

	if err := applyIdleTimeout(path, 0); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := read().AgentRuntime.IdleTimeout; got != "" {
		t.Fatalf("idle timeout = %q after the pool dropped it, want empty", got)
	}

	if err := applyIdleTimeout(filepath.Join(t.TempDir(), sandboxDocumentName), time.Minute); err != nil {
		t.Fatalf("a missing document is an error: %v", err)
	}
}
