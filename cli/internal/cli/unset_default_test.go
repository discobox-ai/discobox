package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// unset-default takes no argument: it clears whichever default the project
// has, and with none set it asks the server to clear nothing.
func TestUnsetDefaultClearsTheProjectsDefault(t *testing.T) {
	const project = `{"id":"project-1","name":"p","defaultPoolId":"pool_default","defaultHarnessConfigId":"hc_default",` +
		`"default":true,"ownerUserId":"user-1","welcomed":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	const cleared = `{"id":"project-1","name":"p","default":true,"ownerUserId":"user-1","welcomed":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	for _, tc := range []struct {
		group, path string
	}{
		{"pool", "/projects/project-1/pools/pool_default/default"},
		{"harness", "/projects/project-1/harness-configs/hc_default/default"},
	} {
		t.Run(tc.group, func(t *testing.T) {
			for _, current := range []string{project, cleared} {
				var deleted []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1":
						_, _ = io.WriteString(w, current)
					case r.Method == http.MethodDelete:
						deleted = append(deleted, r.URL.Path)
						_, _ = io.WriteString(w, cleared)
					default:
						http.NotFound(w, r)
					}
				}))
				runCLI(t, server.URL, "admin", tc.group, "unset-default")
				server.Close()
				want := []string{tc.path}
				if current == cleared {
					want = nil
				}
				if len(deleted) != len(want) || (len(want) == 1 && deleted[0] != want[0]) {
					t.Fatalf("cleared %v, want %v", deleted, want)
				}
			}
			runCLIRefused(t, "http://127.0.0.1:1", "admin", tc.group, "unset-default", "something")
		})
	}
}
