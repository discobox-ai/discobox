package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
)

const inventoryTestPath = "/api/projects/project-1/sandboxes/sandbox-1/inventory"

func getInventory(t *testing.T, router http.Handler, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(resp, req)
	return resp
}

// The inventory holds terminal content and exec records, so the scope that
// reads the status does not read it, and the scope that reads it reads nothing
// else (ADR 26-10-11-657 §1).
func TestInventoryIsReadWithItsOwnScopeOnly(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	token := func(scopes ...string) string { return signToken("project-1", "sandbox-1", "worker-1", scopes...) }

	if resp := getInventory(t, router, inventoryTestPath, token(ScopeInventoryRead)); resp.Code != http.StatusOK {
		t.Fatalf("GET inventory with inventory:read = %d, body = %s", resp.Code, resp.Body.String())
	}
	for name, scopes := range map[string][]string{
		"status read": {ScopeStatusRead},
		"exec read":   {ScopeExecRead, ScopeExecWrite},
	} {
		if resp := getInventory(t, router, inventoryTestPath, token(scopes...)); resp.Code != http.StatusForbidden {
			t.Errorf("GET inventory with %s = %d, want 403", name, resp.Code)
		}
	}
	// Told as the generated router reads it: an escaped letter needs the
	// scope as well (sandbox-agent/REVIEW.md).
	if resp := getInventory(t, router, strings.Replace(inventoryTestPath, "/inventory", "/%69nventory", 1), token(ScopeStatusRead)); resp.Code != http.StatusForbidden {
		t.Errorf("GET an escaped inventory path with status:read = %d, want 403", resp.Code)
	}
	base := "/api/projects/project-1/sandboxes/sandbox-1"
	for _, path := range []string{"/execs", "/services", "/tools", "/status", "/execs/inventory", "/services/inventory", "/execs/inventory/screen"} {
		if resp := getInventory(t, router, base+path, token(ScopeInventoryRead)); resp.Code != http.StatusForbidden {
			t.Errorf("GET %s with inventory:read = %d, want 403", path, resp.Code)
		}
	}
}

// An inventory that has not changed must keep its digest, or the pool would
// post it on every poll; and naming that digest gets an answer with no parts.
func TestInventoryDigestIsStableAndAKnownOneIsUnchanged(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	token := signToken("project-1", "sandbox-1", "worker-1", ScopeInventoryRead)

	read := func(target string) map[string]json.RawMessage {
		t.Helper()
		resp := getInventory(t, router, target, token)
		if resp.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, body = %s", target, resp.Code, resp.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v, body = %s", err, resp.Body.String())
		}
		return body
	}
	first := read(inventoryTestPath)
	if string(first["execs"]) != "[]" {
		t.Fatalf("execs = %s, want an empty list: a sandbox with no execs has that to post", first["execs"])
	}
	if _, ok := first["screens"]; !ok {
		t.Fatalf("inventory has no screens part: %v", first)
	}
	var digest string
	if err := json.Unmarshal(first["digest"], &digest); err != nil || digest == "" {
		t.Fatalf("digest = %s (%v)", first["digest"], err)
	}
	second := read(inventoryTestPath)
	if string(second["digest"]) != string(first["digest"]) {
		t.Fatalf("digest changed between two reads of an unchanged sandbox: %s then %s", first["digest"], second["digest"])
	}

	known := read(inventoryTestPath + "?known=" + digest)
	if string(known["unchanged"]) != "true" {
		t.Fatalf("unchanged = %s, want true", known["unchanged"])
	}
	for _, part := range []string{"execs", "services", "tools", "screens"} {
		if _, ok := known[part]; ok {
			t.Errorf("an unchanged answer carries %s", part)
		}
	}
	if stale := read(inventoryTestPath + "?known=sha256:other"); string(stale["unchanged"]) == "true" {
		t.Fatal("a digest the inventory does not have was answered unchanged")
	}
}

// The digest covers the parts and not their order of encoding: the generated
// encoder writes a map in iteration order.
func TestInventoryDigestIgnoresMapOrderAndObservationTime(t *testing.T) {
	inventory := func() sandboxapi.SandboxAgentInventory {
		screens := sandboxapi.SandboxAgentInventoryScreens{}
		for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
			screens[id] = sandboxapi.SandboxExecScreen{Lines: []string{id}}
		}
		return sandboxapi.SandboxAgentInventory{
			Execs:   []sandboxapi.SandboxExec{{ID: "a", Env: sandboxapi.NewOptSandboxExecEnv(sandboxapi.SandboxExecEnv{"A": "1", "B": "2", "C": "3", "D": "4"})}},
			Screens: sandboxapi.NewOptSandboxAgentInventoryScreens(screens),
		}
	}
	want, err := inventoryDigest(inventory())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := inventoryDigest(inventory())
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("digest differs between two encodings of one inventory: %s, %s", got, want)
		}
	}
	changed := inventory()
	changed.Screens.Value["a"] = sandboxapi.SandboxExecScreen{Lines: []string{"other"}}
	if got, _ := inventoryDigest(changed); got == want {
		t.Fatal("a changed screen kept the digest")
	}
}

// A service is an exec and the tool list says how to start one, so neither
// answers a token that could not read or write an exec — the status poll's
// above all, which the pool holds for every sandbox it hosts.
func TestServicesAndToolsAreGatedLikeExecs(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	base := "/api/projects/project-1/sandboxes/sandbox-1"
	do := func(method, path string, scopes ...string) int {
		t.Helper()
		resp := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), method, base+path, nil)
		req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", scopes...))
		router.ServeHTTP(resp, req)
		return resp.Code
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/services"},
		{http.MethodGet, "/services/web"},
		{http.MethodGet, "/services/web/logs"},
		{http.MethodGet, "/tools"},
	} {
		if code := do(tc.method, tc.path, ScopeStatusRead, ScopeExecWrite); code != http.StatusForbidden {
			t.Errorf("%s %s without exec:read = %d, want 403", tc.method, tc.path, code)
		}
		if code := do(tc.method, tc.path, ScopeExecRead); code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Errorf("%s %s with exec:read = %d, want it let through", tc.method, tc.path, code)
		}
	}
	for _, action := range []string{"start", "stop", "restart"} {
		if code := do(http.MethodPost, "/services/web/"+action, ScopeStatusRead, ScopeExecRead); code != http.StatusForbidden {
			t.Errorf("POST service %s without exec:write = %d, want 403", action, code)
		}
	}
	// An exec named like one of these routes is still an exec.
	for _, id := range []string{"tools", "services"} {
		if code := do(http.MethodDelete, "/execs/"+id, ScopeStatusRead, ScopeExecRead); code != http.StatusForbidden {
			t.Errorf("DELETE exec %q without exec:write = %d, want 403", id, code)
		}
	}
}

// A sandbox with no tool directories declares no tools, which is known rather
// than unknown: it posts an empty list, so a stopped one still lists the tools
// of the layers above it. What it cannot answer at all is left out.
func TestInventoryPostsNoToolsAsAnEmptyListAndLeavesOutWhatItCannotAnswer(t *testing.T) {
	inventory, err := (&handler{}).GetSandboxAgentInventory(context.Background(), sandboxapi.GetSandboxAgentInventoryParams{})
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Tools == nil || len(inventory.Tools) != 0 {
		t.Fatalf("tools = %#v, want an empty list", inventory.Tools)
	}
	if inventory.Services != nil {
		t.Fatalf("services = %#v, want the part left out: this sandbox cannot list them", inventory.Services)
	}
	if inventory.Execs != nil || inventory.Screens.Set {
		t.Fatalf("execs = %#v, screens set = %v; want both left out", inventory.Execs, inventory.Screens.Set)
	}
}
