package declared

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func write(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestReadDirReadsBothShapes(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "10-desktop.yaml", "name: Desktop\nport: 6900\n", 0o644)
	write(t, dir, "20-api.sh", "#!/bin/sh\n#---\n# description: the api\n#---\nexec api\n", 0o755)
	write(t, dir, "30-lint.yml", "", 0o644)

	files, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("read %d files, want 3: %+v", len(files), files)
	}
	desktop, api, lint := files[0], files[1], files[2]
	if desktop.ID != "desktop" || desktop.Name != "Desktop" || !desktop.Metadata || desktop.Problem != "" {
		t.Errorf("desktop = %+v", desktop)
	}
	if desktop.Fields.String("port") != "6900" {
		t.Errorf("port = %q", desktop.Fields.String("port"))
	}
	if api.ID != "api" || api.Metadata || api.Description != "the api" || api.Problem != "" {
		t.Errorf("api = %+v", api)
	}
	// An empty .yaml declares nothing, which is a declaration with every field
	// defaulted rather than a broken one.
	if lint.ID != "lint" || lint.Name != "Lint" || lint.Problem != "" {
		t.Errorf("lint = %+v", lint)
	}
}

func TestReadDirAbsentDirectoryIsNothingDeclared(t *testing.T) {
	files, err := ReadDir(filepath.Join(t.TempDir(), "missing"))
	if err != nil || files != nil {
		t.Fatalf("files, err = %v, %v; want nil, nil", files, err)
	}
}

func TestReadDirSkipsDirectoriesAndDotfiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "fresh.yaml", "name: fresh\n", 0o644)
	write(t, filepath.Join(dir, "fresh"), "config.jsonc", "{}", 0o644)
	write(t, dir, ".hidden.yaml", "name: hidden\n", 0o644)

	files, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != "fresh" {
		t.Fatalf("files = %+v, want only fresh", files)
	}
}

func TestReadDirReportsDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "10-api.sh", "#!/bin/sh\n#---\n#---\n", 0o755)
	write(t, dir, "20-api.yaml", "", 0o644)

	files, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if files[0].Problem != "" {
		t.Errorf("first declaration has problem %q", files[0].Problem)
	}
	if files[1].Problem == "" {
		t.Errorf("second declaration of api has no problem")
	}
}

func TestParseReportsMalformedMetadata(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "list.yaml", "- a\n- b\n", 0o644)
	write(t, dir, "bad-id.yaml", "id: Not.Valid\n", 0o644)
	write(t, dir, "good-id.yaml", "id: ai.discobox.desktop\n", 0o644)

	files, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]File{}
	for _, f := range files {
		byName[f.FileName] = f
	}
	if byName["list.yaml"].Problem == "" {
		t.Error("a top-level list is not a declaration, and has no problem")
	}
	if got := byName["bad-id.yaml"]; got.Problem == "" || got.ID != "bad-id" {
		t.Errorf("bad id = %+v, want a problem and the filename's id", got)
	}
	if got := byName["good-id.yaml"]; got.Problem != "" || got.ID != "ai.discobox.desktop" {
		t.Errorf("good id = %+v", got)
	}
}

func TestMapReadsAMappingField(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "fresh.yaml", "files:\n  z.json: .z\n  config.jsonc: .config/fresh/config.json\n", 0o644)
	write(t, dir, "bad.yaml", "files: [a, b]\n", 0o644)

	files, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Map(files[1], "files")
	if err != nil {
		t.Fatal(err)
	}
	want := []KeyValue{{"config.jsonc", ".config/fresh/config.json"}, {"z.json", ".z"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("files = %+v, want %+v", got, want)
	}
	if _, err := Map(files[0], "files"); err == nil {
		t.Error("a list is not a mapping, and read as one")
	}
	if got, err := Map(files[0], "absent"); got != nil || err != nil {
		t.Errorf("absent = %v, %v", got, err)
	}
}

func TestScriptProblem(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "ok.sh", "#!/bin/sh\n", 0o755)
	write(t, dir, "no-shebang.sh", "echo hi\n", 0o755)
	write(t, dir, "not-executable.sh", "#!/bin/sh\n", 0o644)

	if p := ScriptProblem(parse(os.DirFS(dir), dir, "ok.sh")); p != "" {
		t.Errorf("ok.sh problem = %q", p)
	}
	if p := ScriptProblem(parse(os.DirFS(dir), dir, "no-shebang.sh")); p == "" {
		t.Error("a script without a shebang has no problem")
	}
	if runtime.GOOS != "windows" {
		if p := ScriptProblem(parse(os.DirFS(dir), dir, "not-executable.sh")); p == "" {
			t.Error("a script without the executable bit has no problem")
		}
	}
}
