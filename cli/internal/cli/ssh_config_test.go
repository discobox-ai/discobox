package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSHConfigEmitsHostStanzasAndKnownHostsLine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/projects/project-1/sandboxes":
			_, _ = w.Write([]byte(`{"sandboxes":[
				{"id":"sbx_devbox00000001","projectId":"project-1","createdByUserId":"user-1",
				 "config":{"name":"devbox","image":"discobox-sandbox-agent:local","cpuVcpus":1,"memoryBytes":1,"storageBytes":1},
				 "runtime":{"desiredState":"present","state":"running","generation":1,"observedGeneration":1},
				 "createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}
			]}`))
		case "/ssh/host-key":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ssh-ed25519 AAAAfakehostkey==\n"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cmd := NewRootCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "box", "ssh-config", "--host", "ssh.example.com", "--port", "3222"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"Host sbx_devbox00000001.discobox.internal\n",
		"    HostName ssh.example.com\n",
		"    Port 3222\n",
		"    User sbx_devbox00000001\n",
		"ssh-ed25519 AAAAfakehostkey==",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q; got:\n%s", want, got)
		}
	}
}

func TestSSHConfigMissingHostKeyRouteWarnsButStillEmitsStanzas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/projects/project-1/sandboxes":
			_, _ = w.Write([]byte(`{"sandboxes":[]}`))
		case "/ssh/host-key":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cmd := NewRootCommand()
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "box", "ssh-config"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}
	if !strings.Contains(errOut.String(), "SSH host key") {
		t.Fatalf("expected a warning about the missing host key route, got stderr: %q", errOut.String())
	}
}
