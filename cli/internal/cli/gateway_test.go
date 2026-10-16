package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `admin gateway set` sends the gateway whole, a header from the server's
// environment by its variable; get shows the variable's name, ls the
// gateways, and rm takes one away.
func TestGatewayCommands(t *testing.T) {
	var stored map[string]any
	deleted := ""
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/projects/project-1/gateways/anthropic":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			stored = map[string]any{
				"name": "anthropic", "projectId": "project-1", "match": body["match"], "target": body["target"],
				"requestHeaders": body["requestHeaders"], "createdAt": "2026-10-16T00:00:00Z", "updatedAt": "2026-10-16T00:00:00Z",
			}
			_ = json.NewEncoder(w).Encode(stored)
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/gateways/anthropic":
			_ = json.NewEncoder(w).Encode(stored)
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/gateways":
			_ = json.NewEncoder(w).Encode(map[string]any{"gateways": []any{stored}})
		case r.Method == http.MethodDelete && r.URL.Path == "/projects/project-1/gateways/anthropic":
			deleted = "anthropic"
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	runCLI(t, server.URL, "admin", "gateway", "set", "anthropic",
		"--match", "https://api.anthropic.com/v1",
		"--target", "https://gateway.example.com/anthropic/v1",
		"--remove-header", "x-api-key",
		"--set-header-env", "cf-aig-authorization=DISCOBOX_GATEWAY_CF_TOKEN",
		"--set-header", "x-team=platform",
		"--add-header", "x-tag=a=b",
	)
	headers, _ := stored["requestHeaders"].(map[string]any)
	sent, err := json.Marshal(headers)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"add":[{"name":"x-tag","value":"a=b"}],"remove":["x-api-key"],"set":[{"name":"x-team","value":"platform"},{"name":"cf-aig-authorization","valueFrom":{"env":"DISCOBOX_GATEWAY_CF_TOKEN"}}]}`
	if string(sent) != want {
		t.Fatalf("sent headers %s\nwant %s", sent, want)
	}

	got := runCLI(t, server.URL, "admin", "gateway", "get", "anthropic")
	for _, line := range []string{"https://gateway.example.com/anthropic/v1", "cf-aig-authorization: env:DISCOBOX_GATEWAY_CF_TOKEN", "x-team: platform", "x-api-key"} {
		if !strings.Contains(got, line) {
			t.Fatalf("get is missing %q:\n%s", line, got)
		}
	}
	if listed := runCLI(t, server.URL, "admin", "gateway", "ls"); !strings.Contains(listed, "anthropic") || !strings.Contains(listed, "https://api.anthropic.com/v1") {
		t.Fatalf("ls:\n%s", listed)
	}
	runCLI(t, server.URL, "admin", "gateway", "rm", "anthropic")
	if deleted != "anthropic" {
		t.Fatal("rm did not delete the gateway")
	}
}

func TestGatewaySetRefusesAHeaderWithoutAName(t *testing.T) {
	if _, err := gatewayHeaderFlags([]string{"=value"}, nil); err == nil {
		t.Fatal("accepted a header with no name")
	}
	if _, err := gatewayHeaderFlags(nil, []string{"x-key"}); err == nil {
		t.Fatal("accepted a header with no =")
	}
}
