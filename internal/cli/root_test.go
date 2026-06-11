package cli

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRootCommandHelp(t *testing.T) {
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute help: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("sandbox")) || !bytes.Contains(out.Bytes(), []byte("events")) {
		t.Fatalf("help output = %q, want sandbox and events commands", out.String())
	}
}

func TestProjectUUIDDefaultsToLocal(t *testing.T) {
	app := &App{projectID: defaultProjectAlias}

	projectID, err := app.projectUUID()
	if err != nil {
		t.Fatalf("projectUUID: %v", err)
	}
	if projectID.String() != "00000000-0000-0000-0000-000000000002" {
		t.Fatalf("projectID = %s", projectID)
	}
}

func TestRootCommandRejectsInvalidOutputFormat(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"--output", "yaml", "sandbox", "list"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("execute error = nil, want invalid output error")
	}
}

func TestProjectUUIDRejectsEmptyExplicitProject(t *testing.T) {
	app := &App{projectID: " "}
	if _, err := app.projectUUID(); !errors.Is(err, errMissingProject) {
		t.Fatalf("projectUUID error = %v, want errMissingProject", err)
	}
}

func TestHTTPClientAddsTenantAndAuthorizationHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Disco2-Tenant-ID"); got != "tenant-1" {
			t.Fatalf("tenant header = %q, want tenant-1", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
	}))
	t.Cleanup(server.Close)

	app := &App{tenantID: "tenant-1", token: "token-1"}
	resp, err := app.httpClient().Get(server.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
}
