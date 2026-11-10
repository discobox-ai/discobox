package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console"
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

// A tool's file is edited in place through the frontend: the copy itself is
// what its editor opens, whatever its name (a frontend that opens files by
// their type copies it first: the desktop bridge's edit).
func TestEditToolFileEditsTheCopyItself(t *testing.T) {
	dir := t.TempDir()
	setConfigHome(t, dir)
	d := &apiDataSource{}
	for _, name := range []string{"config.json", "init.lua"} {
		file := console.ToolFile{Tool: "fresh", Name: name, Default: "default\n"}
		want := filepath.Join(dir, "discobox", "tools", "fresh", name)
		n := console.NewNarration()
		var opened string
		go func() {
			for e := range n.Events() {
				if e.Editing != nil {
					opened = e.Editing.Path
					if err := os.WriteFile(opened, []byte("mine\n"), 0o600); err != nil {
						t.Error(err)
					}
					e.Editing.Done(nil)
				}
			}
		}()
		changed, err := d.EditToolFile(t.Context(), file, n)
		n.Close()
		if err != nil || !changed {
			t.Fatalf("%s: changed %v, %v", name, changed, err)
		}
		if opened != want {
			t.Errorf("%s: opened %q, want the copy itself, %q", name, opened, want)
		}
		if saved, err := os.ReadFile(want); err != nil || string(saved) != "mine\n" {
			t.Errorf("%s: saved %q, %v", name, saved, err)
		}
	}
}
