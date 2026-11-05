package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

func TestHarnessPreviewEscapesUntrustedDisplayData(t *testing.T) {
	var preview api.HarnessConfigChanges
	if err := json.Unmarshal([]byte(`{"candidateId":"candidate","configRevision":1,"state":"pending","harnessConfigId":"harness","files":[{"path":"settings.json\u001b[2J","file":{"path":"settings.json","content":"{\n  \"nested\": {\"theme\": \"light\"}\n}\u001b[2J"}}],"credentials":[{"provider":"provider","envName":"TOKEN","type":"oauth","account":"account\u001b[2J"}],"diagnostics":[{"code":"warning","path":"settings.json","message":"bad\u001b[2J"}]}`), &preview); err != nil {
		t.Fatal(err)
	}
	got := toTUIHarnessChanges(&preview)
	for _, value := range []string{got.Files[0].Path, got.Files[0].Diff, got.Credentials[0], got.Diagnostics[0]} {
		if strings.Contains(value, "\x1b") {
			t.Fatalf("raw terminal control in %q", value)
		}
	}
	if !strings.Contains(got.Files[0].Diff, "\n") || !strings.Contains(got.Files[0].Diff, `"nested"`) {
		t.Fatalf("nested multiline settings lost: %q", got.Files[0].Diff)
	}
	if !strings.Contains(got.Diagnostics[0], "settings.json") {
		t.Fatal("diagnostic path lost")
	}
	if got.CandidateID != "candidate" {
		t.Fatal("candidate identity changed")
	}
}

// A change is shown as what a save changes in its file: an update as its
// edited lines, a new file as all additions, a removal as all deletions, and a
// removal the harness has its own file for as the reset to it.
func TestHarnessChangeDiffShowsWhatChanged(t *testing.T) {
	file := func(content string) api.OptHarnessCaptureFile {
		return api.NewOptHarnessCaptureFile(api.HarnessCaptureFile{Path: "settings.json", Content: content})
	}
	before := "{\n  \"model\": \"opus\",\n  \"theme\": \"dark\"\n}\n"
	after := "{\n  \"model\": \"opus\",\n  \"theme\": \"light\"\n}\n"
	for _, tc := range []struct {
		name, action, diff string
		change             api.HarnessCaptureFileChange
	}{
		{"update", "update", "@@ -1,4 +1,4 @@\n {\n   \"model\": \"opus\",\n-  \"theme\": \"dark\"\n+  \"theme\": \"light\"\n }", api.HarnessCaptureFileChange{Path: "settings.json", File: file(after), Previous: file(before)}},
		{"add", "add", "@@ -0,0 +1,2 @@\n+{}\n+x", api.HarnessCaptureFileChange{Path: "settings.json", File: file("{}\nx\n")}},
		{"no final newline", "update", "@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+b\n\\ No newline at end of file", api.HarnessCaptureFileChange{Path: "settings.json", File: file("b"), Previous: file("a")}},
		{"remove", "remove", "@@ -1,2 +0,0 @@\n-{}\n-x", api.HarnessCaptureFileChange{Path: "settings.json", Remove: api.NewOptBool(true), Previous: file("{}\nx\n")}},
		{"reset", "reset", "@@ -1,4 +1,4 @@\n {\n   \"model\": \"opus\",\n-  \"theme\": \"light\"\n+  \"theme\": \"dark\"\n }", api.HarnessCaptureFileChange{Path: "settings.json", Remove: api.NewOptBool(true), Previous: file(after), File: file(before)}},
		{"layout only", "update", "", api.HarnessCaptureFileChange{Path: "settings.json", File: file(before), Previous: file(`{"model":"opus","theme":"dark"}`)}},
		{"compact bundle file", "update", "@@ -1,4 +1,4 @@\n {\n   \"model\": \"opus\",\n-  \"theme\": \"dark\"\n+  \"theme\": \"light\"\n }", api.HarnessCaptureFileChange{Path: "settings.json", File: file(after), Previous: file(`{"model":"opus","theme":"dark"}`)}},
		{"reset to the same", "reset", "", api.HarnessCaptureFileChange{Path: "settings.json", Remove: api.NewOptBool(true), Previous: file(before), File: file(before)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := harnessChangeAction(tc.change); got != tc.action {
				t.Fatalf("action = %q, want %q", got, tc.action)
			}
			if got := harnessChangeDiff(tc.change); got != tc.diff {
				t.Fatalf("diff =\n%s\nwant\n%s", got, tc.diff)
			}
		})
	}
}

// The file comes from a box, and comparing it line by line costs memory in
// its lines times its edits. A file past the bound is shown replaced whole,
// at a cost linear in its size, rather than taking the client down.
func TestHarnessChangeDiffOfALargeFileIsBounded(t *testing.T) {
	var before, after strings.Builder
	const lines = 50000
	for n := range lines {
		fmt.Fprintf(&before, "%d\n", n)
		fmt.Fprintf(&after, "x%d\n", n)
	}
	change := api.HarnessCaptureFileChange{Path: "big.json", File: api.NewOptHarnessCaptureFile(api.HarnessCaptureFile{Path: "big.json", Content: after.String()}), Previous: api.NewOptHarnessCaptureFile(api.HarnessCaptureFile{Path: "big.json", Content: before.String()})}
	start := time.Now()
	diff := harnessChangeDiff(change)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("diff of %d lines took %s", lines, elapsed)
	}
	got := strings.Split(diff, "\n")
	if got[0] != fmt.Sprintf("@@ -1,%d +1,%d @@", lines, lines) || len(got) != 1+2*lines || got[1] != "-0" || got[lines+1] != "+x0" {
		t.Fatalf("whole-file diff wrong: header %q, %d lines", got[0], len(got))
	}
}

// Re-indenting puts each value on its own line as deep as it is nested, so a
// small, deeply nested document would grow by depth times values. Past the
// bound it is compared as it came, rather than indented into gigabytes.
func TestHarnessChangeDiffDoesNotIndentADeepDocumentWithoutBound(t *testing.T) {
	const depth, values = 9000, 50000
	deep := `{"a":` + strings.Repeat("[", depth) + strings.TrimSuffix(strings.Repeat("1,", values), ",") + strings.Repeat("]", depth) + "}"
	if !json.Valid([]byte(deep)) {
		t.Fatal("fixture is not JSON")
	}
	if got := diffText(deep); got != deep {
		t.Fatalf("deep document was re-indented to %d bytes", len(got))
	}
	if got, want := diffText(`{"a":[1,2]}`), "{\n  \"a\": [\n    1,\n    2\n  ]\n}\n"; got != want {
		t.Fatalf("small document = %q, want %q", got, want)
	}
	if size, got := indentedSize(`{"a":[1,2]}`), len("{\n  \"a\": [\n    1,\n    2\n  ]\n}"); size < got {
		t.Fatalf("indentedSize %d is under the indented size %d", size, got)
	}
}

func TestHarnessChangesAcceptsProjectAndServerFlags(t *testing.T) {
	root := NewRootCommand()
	var output strings.Builder
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--server", "http://127.0.0.1:8080", "--project", "project-review", "admin", "harness", "changes", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "changes SANDBOX") {
		t.Fatalf("wrong command: %s", output.String())
	}
}

func TestSaveHarnessChangesSendsCandidateOnce(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.UserAgent() == "discobox-sandbox-agent (port probe)" {
					w.WriteHeader(200)
					return
				}
				requests++
				if r.URL.RawQuery != "" {
					t.Errorf("unexpected save options: %s", r.URL.RawQuery)
				}
				var body api.HarnessConfigCandidateRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.CandidateId != "reviewed" {
					t.Error("candidate changed")
				}
				if fail {
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(409)
					_, _ = w.Write([]byte(`{"status":409,"title":"Conflict","detail":"upgrade or recreate the sandbox"}`))
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"candidateId":"reviewed","harnessConfigId":"harness","configRevision":2,"state":"saved"}`))
				}
			}))
			defer server.Close()
			client, err := api.NewClient(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			ds := &apiDataSource{client: client, projectID: "project-1"}
			state, err := ds.SaveHarnessChanges(context.Background(), "box", "reviewed")
			if fail {
				if err == nil || !strings.Contains(err.Error(), "upgrade or recreate") {
					t.Fatalf("lost recovery: %v", err)
				}
			} else if err != nil || state != "saved" {
				t.Fatalf("save: %s %v", state, err)
			}
			if requests != 1 {
				t.Fatalf("requests=%d", requests)
			}
		})
	}
}

// A candidate is immutable, but whether it changes shared state is not. Every
// listing checks again, without discarding it or recording a dismissal.
func TestListingChecksHarnessChangesAgainstCurrentSavedState(t *testing.T) {
	var response atomic.Value
	response.Store(`{"candidateId":"candidate","harnessConfigId":"harness","configRevision":2,"state":"pending","files":[],"credentials":[]}`)
	var previews atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() == "discobox-sandbox-agent (port probe)" {
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/harness-config-changes"):
			previews.Add(1)
			fmt.Fprint(w, response.Load().(string))
		case strings.HasSuffix(r.URL.Path, "/sandboxes"):
			row := listedSandboxJSON(sandboxA, "2026-01-01T00:00:00Z")
			row = strings.Replace(row, `"generation":1`, `"agentStatus":{"harnessConfigCandidate":"candidate"},"generation":1`, 1)
			fmt.Fprintf(w, `{"sandboxes":[%s]}`, row)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := api.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ds := &apiDataSource{client: client, projectID: "project-1"}
	for _, tc := range []struct {
		name, body string
		pending    bool
	}{
		{"bundle upgraded", `"state":"pending","files":[],"credentials":[]`, false},
		{"shared edit makes candidate relevant", `"state":"pending","files":[{"path":"settings.json","file":{"path":"settings.json","content":"changed"}}],"credentials":[]`, true},
		{"another box saved", `"state":"pending","files":[],"credentials":[]`, false},
		{"real credential", `"state":"pending","files":[],"credentials":[{"provider":"fixture","envName":"TOKEN","type":"token"}]`, true},
		{"handoff pending", `"state":"saved-handoff-pending","files":[],"credentials":[]`, true},
		{"candidate moved", `"state":"pending","files":[],"credentials":[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := "candidate"
			if tc.name == "candidate moved" {
				candidate = "newer"
			}
			response.Store(fmt.Sprintf(`{"candidateId":%q,"harnessConfigId":"harness","configRevision":2,%s}`, candidate, tc.body))
			listing, err := ds.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(listing.Sandboxes) != 1 {
				t.Fatalf("listing=%+v", listing)
			}
			box := listing.Sandboxes[0]
			if box.HarnessConfigPending != tc.pending || box.HarnessConfigCandidate != "candidate" {
				t.Fatalf("pending=%v candidate=%q", box.HarnessConfigPending, box.HarnessConfigCandidate)
			}
		})
	}
	if previews.Load() != 6 {
		t.Fatalf("previews=%d", previews.Load())
	}
}

func TestHarnessOfferDoesNotWakeStoppedBoxesOrSurvivePreviewFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() == "discobox-sandbox-agent (port probe)" {
			w.WriteHeader(204)
			return
		}
		if strings.Contains(r.URL.Path, "stopped") {
			t.Error("preview woke a stopped box")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, err := api.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ds := &apiDataSource{client: client, projectID: "project-1"}
	boxes := []tui.Sandbox{{ID: "stopped", State: tui.StateStopped, HarnessConfigCandidate: "candidate"}, {ID: "running", State: tui.StateRunning, HarnessConfigCandidate: "candidate"}}
	ds.refreshHarnessChanges(context.Background(), boxes)
	for _, box := range boxes {
		if box.HarnessConfigPending {
			t.Fatalf("unverified offer: %+v", box)
		}
	}
}
