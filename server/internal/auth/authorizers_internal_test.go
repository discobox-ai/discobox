package auth

import "testing"

// A server-scoped route has no project membership to check, so nothing narrower
// authorizes it: without an entry here every request to it answers 403 while
// every service-level and router-level test still passes. That is how `/peer`
// shipped broken the first time it was wired, so the allowlist gets a test of
// its own rather than being covered incidentally.
func TestAuthenticatedAllowedPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"this server's own peer ID (ADR 0098)", "/peer", true},
		{"what this server calls itself (ADR 0116)", "/server", true},
		{"enrolled peers", "/peers", true},
		{"one enrolled peer", "/peers/d1-dtztd73", true},
		{"projects", "/projects", true},
		{"the provider catalog", "/providers/catalog", true},

		{"a route below an exact entry", "/peer/secrets", false},
		{"a route below the server entry", "/server/secrets", false},
		{"a route that merely starts the same way", "/peering", false},
		{"a project's sandboxes", "/projects/proj-1/sandboxes", false},
		{"an unlisted server-scoped route", "/secrets", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAuthenticatedAllowedPath(tc.path); got != tc.want {
				t.Fatalf("isAuthenticatedAllowedPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
