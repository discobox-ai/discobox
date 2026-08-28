package auth

import "testing"

// The pool runtime allowlist decides which routes a pool agent can authenticate
// to at all. A route missing from it answers 403 to the real agent while every
// service-level test still passes, so the allowlist gets its own test rather
// than being covered only incidentally by whichever routes happen to be
// exercised elsewhere.
func TestPoolRuntimePathAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"a listed action", "/api/pools/pool-1/status", true},
		{"sentinel resolution", "/api/pools/pool-1/resolve-sandbox-secret", true},
		{"listing agent credentials", "/api/pools/pool-1/sandbox-credentials", true},
		{"recording a credential request", "/api/pools/pool-1/sandbox-credential-requests", true},
		{"polling one credential request", "/api/pools/pool-1/sandbox-credential-requests/sreq_abc", true},

		{"an unlisted action", "/api/pools/pool-1/secrets", false},
		{"a misspelled action", "/api/pools/pool-1/sandbox-credential", false},
		// An action that takes no resource ID must not acquire one: otherwise an
		// unlisted subroute inherits pool access from its listed parent.
		{"a resource ID on an action that has none", "/api/pools/pool-1/status/extra", false},
		{"a subroute below a resource ID", "/api/pools/pool-1/sandbox-credential-requests/sreq_abc/approve", false},
		{"no action at all", "/api/pools/pool-1", false},
		{"a project route", "/api/projects/proj-1/sandboxes", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPoolRuntimePath(tc.path); got != tc.want {
				t.Fatalf("isPoolRuntimePath(%q) = %v, want %v", tc.path, got, tc.want)
			}
			// Whatever the matcher accepts, the authenticator must be able to name
			// the pool it belongs to; the two decisions cannot disagree.
			poolID, err := poolIDFromRuntimePath(tc.path)
			if tc.want && (err != nil || poolID != "pool-1") {
				t.Fatalf("poolIDFromRuntimePath(%q) = %q, %v; want pool-1", tc.path, poolID, err)
			}
			if !tc.want && err == nil {
				t.Fatalf("poolIDFromRuntimePath(%q) accepted a path the matcher rejected", tc.path)
			}
		})
	}
}

// Every route the OpenAPI contract puts under /api/pools/{poolId}/ must be
// listed, since the contract is what the pool agent actually calls.
func TestEveryPoolRuntimeActionIsReachable(t *testing.T) {
	for action := range poolRuntimeActions {
		if !isPoolRuntimePath("/api/pools/pool-1/" + action) {
			t.Fatalf("action %q is listed but its own path does not match", action)
		}
	}
}
