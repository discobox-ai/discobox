package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The wait on the ports is read with the status's own scope and nothing else's:
// it is the pool's, like the status (ADR 26-11-03-815 §2). A call that names no
// version is answered at once.
func TestTheStatusWaitIsReadWithTheStatusScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	token := func(scopes ...string) string { return signToken("project-1", "sandbox-1", "worker-1", scopes...) }
	const path = "/api/projects/project-1/sandboxes/sandbox-1/status/wait"

	resp := getInventory(t, router, path, token(ScopeStatusRead))
	if resp.Code != http.StatusOK {
		t.Fatalf("GET status/wait with status:read = %d, body = %s", resp.Code, resp.Body.String())
	}
	var body struct {
		PortsVersion *string `json:"portsVersion"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil || body.PortsVersion == nil {
		t.Fatalf("body = %s, want a portsVersion (%v)", resp.Body.String(), err)
	}
	for name, scopes := range map[string][]string{
		"inventory read": {ScopeInventoryRead},
		"exec read":      {ScopeExecRead, ScopeExecWrite},
	} {
		if resp := getInventory(t, router, path, token(scopes...)); resp.Code != http.StatusForbidden {
			t.Errorf("GET status/wait with %s = %d, want 403", name, resp.Code)
		}
	}
}
