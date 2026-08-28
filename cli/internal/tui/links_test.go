package tui

import "testing"

// The rewriter is what a pane asks where a URL on its screen actually goes: the
// sandbox's port, answered with the local one the forward bound, on the name
// rather than the address.
func TestForwardedURLPointsAtTheLocalEnd(t *testing.T) {
	m := &Model{forward: newFakeForward(
		Binding{Port: 8080, Local: 8081},
		Binding{Port: 443, Local: 8443},
		Binding{Port: 80, Local: 8000},
	)}

	for _, tc := range []struct{ raw, want string }{
		// The port moved, so the URL does.
		{"http://localhost:8080/", "http://localhost:8081/"},
		// The bind address a server prints is not one to open; the port is
		// still the sandbox's, and the answer is the same either way.
		{"http://0.0.0.0:8080/health?x=1", "http://localhost:8081/health?x=1"},
		{"http://127.0.0.1:8080", "http://localhost:8081"},
		{"http://[::1]:8080/", "http://localhost:8081/"},
		// A port left out is the scheme's, and forwardable like any other.
		{"https://localhost/admin", "https://localhost:8443/admin"},
		{"http://localhost/", "http://localhost:8000/"},
		// Everything else is already right, and saying so is what leaves it as
		// text for the terminal's own linking.
		{"http://localhost:9999/", "http://localhost:9999/"},
		{"https://github.com/discobox-ai/discobox", "https://github.com/discobox-ai/discobox"},
		{"postgres://localhost:8080/db", "postgres://localhost:8080/db"},
		{"http://example.com:8080/", "http://example.com:8080/"},
	} {
		if got := m.forwardedURL(tc.raw); got != tc.want {
			t.Errorf("forwardedURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// With nothing forwarded — every screen but the workspace, and the workspace
// until its forward binds — nothing is moved and nothing is linked.
func TestForwardedURLIsInertWithoutAForward(t *testing.T) {
	var m Model
	if got, want := m.forwardedURL("http://localhost:8080/"), "http://localhost:8080/"; got != want {
		t.Errorf("forwardedURL = %q, want %q", got, want)
	}
}
