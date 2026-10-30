package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	api "github.com/discobox-ai/discobox/api/gen"
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
