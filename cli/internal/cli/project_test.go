package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

func TestParseCopyItems(t *testing.T) {
	items, err := parseCopyItems(copyableResources)
	if err != nil {
		t.Fatalf("parseCopyItems: %v", err)
	}
	if len(items) != 3 || items[0] != apiclientgen.CreateProjectBodyCopyItemProviders {
		t.Fatalf("items = %#v, want all three copyable resources", items)
	}

	// "none" is how the flag's non-empty default is turned off, and it must
	// produce an empty selection rather than an error or a nil-ish default.
	items, err = parseCopyItems([]string{"none"})
	if err != nil {
		t.Fatalf("parseCopyItems(none): %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %#v, want none", items)
	}

	if _, err := parseCopyItems([]string{"sandboxes"}); err == nil {
		t.Fatal("parseCopyItems(sandboxes) error = nil, want unknown value")
	}
}

// --copy without --from names a source that does not exist, so it is a
// mistake worth reporting rather than a silently ignored flag.
func TestProjectCreateRejectsCopyWithoutFrom(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"admin", "project", "create", "Thing", "--copy", "pools"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--copy needs --from") {
		t.Fatalf("execute error = %v, want --copy needs --from", err)
	}
}

// --welcomed=false is the way to see the launcher's welcome screen again on a
// project that has already shown it: PATCHing welcomed back to false is the
// server's own escape hatch for a screen that otherwise appears once (see
// projects/service.go). Absent, the flag must not touch the field at all —
// every other field on this command already tells "not given" from "given as
// the type's zero value" through cmd.Flags().Changed, and welcomed must too.
func TestProjectUpdateWelcomedFlag(t *testing.T) {
	const projectID = "proj_aaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name string
		args []string
		want *bool
	}{
		{"not given", nil, nil},
		{"false", []string{"--welcomed=false"}, boolPtr(false)},
		{"bare flag means true", []string{"--welcomed"}, boolPtr(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posted map[string]any
			var method, path string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.Path
				defer r.Body.Close()
				if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_, _ = w.Write([]byte(`{"id":"` + projectID + `","ownerUserId":"user-1","name":"Project",` +
					`"default":false,"welcomed":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
			}))
			t.Cleanup(server.Close)

			cmd := NewRootCommand()
			args := append([]string{"--server", server.URL, "admin", "project", "update", projectID}, tc.args...)
			cmd.SetArgs(args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})

			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if method != http.MethodPatch || path != "/projects/"+projectID {
				t.Fatalf("request = %s %s, want PATCH /projects/%s", method, path, projectID)
			}
			got, ok := posted["welcomed"]
			if tc.want == nil {
				if ok {
					t.Fatalf("welcomed = %v in the request body, want it left out entirely", got)
				}
				return
			}
			if !ok || got != *tc.want {
				t.Fatalf("welcomed = %#v, want %v", got, *tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
