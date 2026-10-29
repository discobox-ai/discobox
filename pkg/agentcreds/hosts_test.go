package agentcreds_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// A request that still names a destination as "host" is refused, not read with
// the field dropped: dropped, the ask would name no destination it meant
// (ADR 26-10-02-393 §4). An empty host — what a client
// that predates hosts sends when it named none — is no destination, and the
// ask goes through.
func TestARequestNamingHostIsRefused(t *testing.T) {
	server := httptest.NewServer(agentcreds.NewHandler(&fakeService{}))
	t.Cleanup(server.Close)
	for body, want := range map[string]int{
		`{"name":"github","envVar":"GH_TOKEN","host":"api.github.com"}`:          http.StatusBadRequest,
		`{"name":"github","envVar":"GH_TOKEN","host":"","hosts":["github.com"]}`: http.StatusAccepted,
		`{"name":"github","envVar":"GH_TOKEN","hosts":["api.github.com"]}`:       http.StatusAccepted,
	} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+agentcreds.PathRequests, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		var out agentcreds.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: status = %d, want %d", body, resp.StatusCode, want)
		}
		if want == http.StatusBadRequest && (out.Code != agentcreds.CodeInvalid || !strings.Contains(out.Error, `"hosts"`) || !strings.Contains(out.Error, "recreate")) {
			t.Fatalf("%s: error = %+v, want invalid, naming hosts and what an old client can do", body, out)
		}
	}
}

// Routes travel beside hosts as the typed strings the agent wrote; the
// protocol carries them unread, and an ask naming only routes names no hosts.
func TestARequestCarriesRoutes(t *testing.T) {
	svc := &fakeService{}
	server := httptest.NewServer(agentcreds.NewHandler(svc))
	t.Cleanup(server.Close)
	client := agentcreds.NewClient(server.URL)
	routes := []string{"GET api.github.com/repos/o/r/**", "api.github.com/graphql graphql=query"}
	if _, err := client.Request(context.Background(), agentcreds.RequestBody{Name: "github", EnvVar: "GH_TOKEN", Routes: routes}); err != nil {
		t.Fatalf("request: %v", err)
	}
	if got := svc.gotRequest.Routes; len(got) != 2 || got[0] != routes[0] || got[1] != routes[1] {
		t.Fatalf("routes = %q, want %q", got, routes)
	}
	if svc.gotRequest.Hosts != nil {
		t.Fatalf("hosts = %q, want none", svc.gotRequest.Hosts)
	}
	encoded, err := json.Marshal(agentcreds.RequestBody{Name: "github", Routes: routes})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), `"hosts"`) {
		t.Fatalf("encoded = %s, want no hosts field", encoded)
	}
}
