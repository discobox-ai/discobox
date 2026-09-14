package poolagent

import (
	"crypto/ed25519"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/wire"
	"github.com/discobox-ai/x/shorttmp"
)

func TestJudgeRuntimeUsesResolvedControlPlaneTransport(t *testing.T) {
	path := filepath.Join(shorttmp.Dir(t), "cp.sock")
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() == "discobox-sandbox-agent (port probe)" {
			return
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("missing pool assertion")
		}
		if r.Host != "pool-agent.local" {
			t.Errorf("host = %q", r.Host)
		}
		if r.URL.Path != "/api/pools/pool-test/judge-runtime" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandboxId":"judge-test","revision":"revision-test","token":"test-token","secretEnv":{},"harness":{"id":"harness-test","projectId":"project-test","slug":"codex","name":"Codex","builtIn":false,"configured":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}}`))
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	base, transport, err := wire.HTTPClient("unix://"+path, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := NewHTTPClient(base, WithHTTPClient(transport))
	spec, err := client.getJudgeRuntime(t.Context(), "project-test", "pool-test", key)
	if err != nil {
		t.Fatal(err)
	}
	if spec.SandboxId != "judge-test" || !strings.HasPrefix(spec.Revision, "revision-") {
		t.Fatalf("wrong recipe: %#v", spec)
	}
}
