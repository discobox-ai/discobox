package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-faster/jx"
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/internal/apiclient/gen"
)

func TestWriteProviderTableIncludesConfig(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := app.writeProvider(cmd, &apiclientgen.SandboxProviderInstance{
		ID:       "provider-1",
		Name:     "Docker",
		Type:     "docker",
		Config:   jx.Raw(`{"poolSize":1,"socketPath":"/var/run/docker.sock","token":"do-secret","nested":{"apiKey":"api-secret"},"items":[{"password":"password-secret"}]}`),
		Disabled: true,
		Status: apiclientgen.NewOptSandboxProviderInstanceStatus(apiclientgen.SandboxProviderInstanceStatus{
			WorkerCount:        1,
			ReadyWorkers:       1,
			SchedulableWorkers: 1,
			Workers: apiclientgen.NewOptNilProviderWorkerStatusArray([]apiclientgen.ProviderWorkerStatus{{
				ID:                  "worker-1",
				Phase:               "registering",
				LastOperationStatus: "success",
				RuntimeId:           apiclientgen.NewOptString("container-1"),
			}}),
		}),
	})
	if err != nil {
		t.Fatalf("writeProvider: %v", err)
	}

	output := out.String()
	for _, want := range []string{
		"FIELD",
		"CONFIG",
		`"poolSize":1`,
		`"socketPath":"/var/run/docker.sock"`,
		`"token":"[REDACTED]"`,
		`"apiKey":"[REDACTED]"`,
		`"password":"[REDACTED]"`,
		"STATUS",
		`"workerCount":1`,
		`"id":"worker-1"`,
		`"runtimeId":"container-1"`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("provider output = %q, want %q", output, want)
		}
	}
	for _, leaked := range []string{"do-secret", "api-secret", "password-secret", "bootstrap-secret"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("provider output leaked secret %q: %q", leaked, output)
		}
	}
}

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

func TestProjectIDDefaultsToDefaultAlias(t *testing.T) {
	app := &App{projectID: defaultProjectAlias}

	projectID, err := app.projectIDValue()
	if err != nil {
		t.Fatalf("projectIDValue: %v", err)
	}
	if projectID != "default" {
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

func TestProjectIDRejectsEmptyExplicitProject(t *testing.T) {
	app := &App{projectID: " "}
	if _, err := app.projectIDValue(); !errors.Is(err, errMissingProject) {
		t.Fatalf("projectIDValue error = %v, want errMissingProject", err)
	}
}

func TestHTTPClientAddsTenantAndAuthorizationHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Discobox-Tenant-ID"); got != "tenant-1" {
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

func TestDebugTransportPrintsRequestAndRedactsAuthorization(t *testing.T) {
	var log bytes.Buffer
	transport := debugTransport{
		out: &log,
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer token-1" {
				t.Fatalf("authorization header = %q, want bearer token", got)
			}
			return &http.Response{
				Status:     "204 No Content",
				StatusCode: http.StatusNoContent,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}
	req, err := http.NewRequest(http.MethodPost, "http://user:pass@example.test/path?x=1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token-1")
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	output := log.String()
	for _, want := range []string{
		"> POST http://user:xxxxx@example.test/path?x=1",
		"> Authorization: [REDACTED]",
		"> Content-Type: application/json",
		"< 204 No Content",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("debug output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "token-1") || strings.Contains(output, "pass@example") {
		t.Fatalf("debug output leaked secret: %q", output)
	}
}

func TestHTTPClientDebugLogsAddedHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Discobox-Tenant-ID"); got != "tenant-1" {
			t.Fatalf("tenant header = %q, want tenant-1", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
	}))
	t.Cleanup(server.Close)

	var log bytes.Buffer
	app := &App{tenantID: "tenant-1", token: "token-1", debug: true, errOut: &log}
	resp, err := app.httpClient().Get(server.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	output := log.String()
	for _, want := range []string{
		"> GET " + server.URL,
		"> X-Discobox-Tenant-Id: tenant-1",
		"> Authorization: [REDACTED]",
		"< 200 OK",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("debug output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "token-1") {
		t.Fatalf("debug output leaked token: %q", output)
	}
}

func TestDebugTransportPrintsRequestAndResponseBodies(t *testing.T) {
	var log bytes.Buffer
	transport := debugTransport{
		out: &log,
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			if got, want := string(body), `{"name":"sandbox-1"}`; got != want {
				t.Fatalf("request body = %q, want %q", got, want)
			}
			return &http.Response{
				Status:     "201 Created",
				StatusCode: http.StatusCreated,
				Body:       io.NopCloser(strings.NewReader(`{"id":"sandbox-1"}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}
	req, err := http.NewRequest(http.MethodPost, "http://example.test/sandboxes", strings.NewReader(`{"name":"sandbox-1"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	if got, want := string(respBody), `{"id":"sandbox-1"}`; got != want {
		t.Fatalf("response body = %q, want %q", got, want)
	}

	output := log.String()
	for _, want := range []string{
		"> body:\n{\"name\":\"sandbox-1\"}\n",
		"< body:\n{\"id\":\"sandbox-1\"}",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("debug output = %q, want %q", output, want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
