package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

func TestGeneratedServerClientUsesLegacyRoutes(t *testing.T) {
	const basePath = "/registered/server"
	requests := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case basePath + "/peer", basePath + "/server":
			_, _ = w.Write([]byte("{}"))
		case basePath + "/api/projects/p/sandboxes/s/execs/my%2Fexec":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"exec not found"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := newServerAPIClient(server.URL+basePath+"/", server.Client())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.GetServerPeer(t.Context()); err != nil {
		t.Fatalf("peer lookup: %v", err)
	}
	if _, err := client.GetSandboxExec(t.Context(), apiclientgen.GetSandboxExecParams{
		ProjectId: "p", SandboxId: "s", ExecId: "my/exec",
	}); err != nil {
		t.Fatalf("exec lookup: %v", err)
	}
	if _, err := client.GetServerInfo(t.Context()); err != nil {
		t.Fatalf("server lookup: %v", err)
	}
	for _, want := range []string{
		"GET " + basePath + "/peer",
		"GET " + basePath + "/api/projects/p/sandboxes/s/execs/my%2Fexec",
		"GET " + basePath + "/server",
	} {
		select {
		case got := <-requests:
			if got != want {
				t.Errorf("request = %q, want %q", got, want)
			}
		default:
			t.Errorf("missing request %q", want)
		}
	}
}
