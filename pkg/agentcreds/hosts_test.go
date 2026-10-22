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
// the field dropped: dropped, an ask by ID would widen from the host it named
// to the ID's whole site (ADR 26-10-02-393 §4). An empty host — what a client
// that predates hosts sends when it named none — is no destination, and the
// ask goes through.
func TestARequestNamingHostIsRefused(t *testing.T) {
	server := httptest.NewServer(agentcreds.NewHandler(&fakeService{}))
	t.Cleanup(server.Close)
	for body, want := range map[string]int{
		`{"id":"com.github.api","host":"api.github.com","uses":[{"description":"open a PR"}]}`:                  http.StatusBadRequest,
		`{"id":"com.github.api","host":"","uses":[{"description":"open a PR"}]}`:                                http.StatusAccepted,
		`{"name":"github","envVar":"GH_TOKEN","hosts":["api.github.com"],"uses":[{"description":"open a PR"}]}`: http.StatusAccepted,
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
