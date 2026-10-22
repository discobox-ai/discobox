package cli

import (
	"testing"

	"github.com/discobox-ai/discobox/pkg/tools"
)

// A tool's file is written through the files API under the default user's
// home, only where nothing is there: a second install leaves the discobox's
// own copy and still succeeds.
func TestInstallToolFilesWritesOnlyWhenAbsent(t *testing.T) {
	fake, app := newFakeFilesAPI(t)
	setConfigHome(t, t.TempDir())
	def := tools.Definition{ID: "fresh", Files: []tools.File{{Name: "config.json", Home: "/.config/fresh/config.json", Default: "first\n"}}}
	if err := app.installToolFiles(t.Context(), "project-1", "sbx_1", def); err != nil {
		t.Fatalf("first install: %v", err)
	}
	def.Files[0].Default = "second\n"
	if err := app.installToolFiles(t.Context(), "project-1", "sbx_1", def); err != nil {
		t.Fatalf("second install should succeed and change nothing: %v", err)
	}
	got, ok := fake.files[".config/fresh/config.json"]
	if !ok || got.content != "first\n" {
		t.Fatalf("files = %+v, want the first content kept at a home-relative path", fake.files)
	}
	for _, call := range fake.calls {
		if call != "PUT content ifExists=fail&parents=true&path=.config%2Ffresh%2Fconfig.json" {
			t.Fatalf("call = %q, want a write that fails when the file exists and creates its parents", call)
		}
	}
}

// Content survives verbatim: it is the request's body, so nothing in it is
// read by a shell.
func TestInstallToolFilesKeepsContentVerbatim(t *testing.T) {
	fake, app := newFakeFilesAPI(t)
	setConfigHome(t, t.TempDir())
	content := "// vim: set ft=jsonc :\n{\n  // a $dollar, `backticks`, 100% literal, \"quotes\", 'single',\n  \"k\": false,\n}\n"
	def := tools.Definition{ID: "x", Files: []tools.File{{Name: "y.json", Home: "x/y.json", Default: content}}}
	if err := app.installToolFiles(t.Context(), "project-1", "sbx_1", def); err != nil {
		t.Fatal(err)
	}
	if got := fake.files["x/y.json"].content; got != content {
		t.Fatalf("content came through as\n%q\nwant\n%q", got, content)
	}
}
