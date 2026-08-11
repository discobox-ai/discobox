package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sshConfigTestServer serves the two routes ssh-config reads: the project's
// sandboxes and the SSH ingress discovery document.
func sshConfigTestServer(t *testing.T, sandboxes, ingress string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/projects/project-1/sandboxes":
			_, _ = w.Write([]byte(sandboxes))
		case "/ssh":
			_, _ = w.Write([]byte(ingress))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

const sshConfigTestSandboxes = `{"sandboxes":[
	{"id":"sbx_devbox00000001","projectId":"project-1","createdByUserId":"user-1",
	 "config":{"name":"devbox","image":"discobox-sandbox-agent:local","cpuVcpus":1,"memoryBytes":1,"storageBytes":1},
	 "runtime":{"desiredState":"present","state":"running","generation":1,"observedGeneration":1},
	 "createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}
]}`

// TestSSHConfigUsesTheAdvertisedAddress is the point of the discovery
// document: the client emits the endpoint the server named, having hard-coded
// neither host nor port.
func TestSSHConfigUsesTheAdvertisedAddress(t *testing.T) {
	server := sshConfigTestServer(t, sshConfigTestSandboxes,
		`{"enabled":true,"address":"ssh.example.com:3222","hostKey":"ssh-ed25519 AAAAfakehostkey=="}`)

	cmd := NewRootCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "box", "ssh-config"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"Host sbx_devbox00000001.discobox.internal\n",
		"    HostName ssh.example.com\n",
		"    Port 3222\n",
		"    User sbx_devbox00000001\n",
		"[ssh.example.com]:3222 ssh-ed25519 AAAAfakehostkey==",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q; got:\n%s", want, got)
		}
	}
}

// TestSSHConfigFlagsOverrideTheAdvertisedAddress covers the case the server
// cannot know about, such as reaching it through a local port forward.
func TestSSHConfigFlagsOverrideTheAdvertisedAddress(t *testing.T) {
	server := sshConfigTestServer(t, sshConfigTestSandboxes,
		`{"enabled":true,"address":"ssh.example.com:3222","hostKey":"ssh-ed25519 AAAAfakehostkey=="}`)

	cmd := NewRootCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "box", "ssh-config",
		"--host", "127.0.0.1", "--port", "22022"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"    HostName 127.0.0.1\n",
		"    Port 22022\n",
		"[127.0.0.1]:22022 ssh-ed25519 AAAAfakehostkey==",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ssh.example.com") {
		t.Fatalf("advertised address leaked past the overrides:\n%s", got)
	}
}

// TestSSHConfigReportsDisabledIngress: SSH is opt-in, so "not enabled" is an
// ordinary answer the document carries, and the client should say so rather
// than emit a config pointing nowhere.
func TestSSHConfigReportsDisabledIngress(t *testing.T) {
	server := sshConfigTestServer(t, sshConfigTestSandboxes, `{"enabled":false}`)

	cmd := NewRootCommand()
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "box", "ssh-config"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected ssh-config to fail when the server has no SSH ingress")
	}
	if !strings.Contains(err.Error(), "DISCOBOX_SSH_LISTEN") {
		t.Fatalf("error should name the setting that enables SSH, got: %v", err)
	}
}

func TestKnownHostsHost(t *testing.T) {
	for _, tc := range []struct {
		host string
		port int
		want string
	}{
		{host: "ssh.example.com", port: 3222, want: "[ssh.example.com]:3222"},
		{host: "::1", port: 3222, want: "[::1]:3222"},
		// ssh looks a port-22 host up under its bare name, so bracketing it
		// would write an entry that never matches.
		{host: "ssh.example.com", port: 22, want: "ssh.example.com"},
	} {
		if got := knownHostsHost(tc.host, tc.port); got != tc.want {
			t.Errorf("knownHostsHost(%q, %d) = %q, want %q", tc.host, tc.port, got, tc.want)
		}
	}
}
