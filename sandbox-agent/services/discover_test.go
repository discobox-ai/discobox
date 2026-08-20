package services

import (
	"os"
	"path/filepath"
	"testing"
)

// writeService puts one declaration in root's service directory.
func writeService(t *testing.T, root, name, body string, mode os.FileMode) {
	t.Helper()
	dir := filepath.Join(root, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

const apiScript = `#!/bin/bash
#---
# name: Discobox API
# description: Runs the server with hot reload
#---
exec task dev:server
`

func TestDiscoverReadsDeclarations(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "10-discobox-api.sh", apiScript, 0o755)
	writeService(t, root, "15-otel.sh", "#!/bin/bash\n#---\n# name: OTEL\n#---\nexec dashboard\n", 0o755)

	defs, err := Discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("discovered %d services, want 2: %+v", len(defs), defs)
	}
	// Filename order, which is what the numeric prefix is for.
	if defs[0].ID != "discobox-api" || defs[1].ID != "otel" {
		t.Fatalf("ids = %q, %q; want discobox-api, otel", defs[0].ID, defs[1].ID)
	}
	if defs[0].Name != "Discobox API" {
		t.Errorf("name = %q, want %q", defs[0].Name, "Discobox API")
	}
	if defs[0].Description != "Runs the server with hot reload" {
		t.Errorf("description = %q", defs[0].Description)
	}
	if defs[0].FileName != "10-discobox-api.sh" {
		t.Errorf("fileName = %q", defs[0].FileName)
	}
	if !defs[0].Runnable() {
		t.Errorf("problem = %q, want none", defs[0].Problem)
	}
	if defs[0].Path != filepath.Join(root, DirName, "10-discobox-api.sh") {
		t.Errorf("path = %q", defs[0].Path)
	}
}

// A repository that declares nothing is the common case and must cost nothing
// but a failed read.
func TestDiscoverAbsentDirectory(t *testing.T) {
	defs, err := Discover(t.TempDir())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("discovered %d services, want none", len(defs))
	}
}

// A name defaults from the filename, so a declaration that says nothing is
// still addressable and still readable.
func TestDiscoverDefaultsNameFromFilename(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "20-web-ui.sh", "#!/bin/sh\n#---\n#---\nexec serve\n", 0o755)

	defs, err := Discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 1 || defs[0].Name != "Web Ui" {
		t.Fatalf("defs = %+v, want one named %q", defs, "Web Ui")
	}
}

// A declaration that cannot run is listed with the reason rather than dropped:
// a file the author believes is a service and that nothing ever mentions is the
// failure this avoids.
func TestDiscoverReportsUnrunnableDeclarations(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "10-not-executable.sh", apiScript, 0o644)
	writeService(t, root, "20-no-shebang.sh", "#---\n# name: Nope\n#---\necho hi\n", 0o755)
	writeService(t, root, "30-no-front-matter.sh", "#!/bin/sh\necho hi\n", 0o755)

	defs, err := Discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 3 {
		t.Fatalf("discovered %d services, want 3", len(defs))
	}
	for _, def := range defs {
		if def.Runnable() {
			t.Errorf("%s: expected a problem, got none", def.ID)
		}
	}
	if got := defs[0].Problem; got != "script is not executable" {
		t.Errorf("problem = %q, want %q", got, "script is not executable")
	}
	if got := defs[1].Problem; got != "script must start with a shebang line" {
		t.Errorf("problem = %q, want %q", got, "script must start with a shebang line")
	}
	// A file with no front matter at all is a declaration error, not a service
	// with no name: `.discobox/services` is not a scripts folder.
	if defs[2].Problem == "" {
		t.Error("a file with no front matter must report a problem")
	}
}

// Two files whose names normalize to one id would otherwise take turns being
// "the" service depending on directory order.
func TestDiscoverReportsDuplicateIDs(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "10-api.sh", apiScript, 0o755)
	writeService(t, root, "20-api.sh", apiScript, 0o755)

	defs, err := Discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("discovered %d services, want 2", len(defs))
	}
	if !defs[0].Runnable() {
		t.Errorf("the first declaration keeps the id: %q", defs[0].Problem)
	}
	if defs[1].Runnable() {
		t.Error("the second declaration of an id must report the conflict")
	}
}

func TestDiscoverSkipsDirectoriesAndDotfiles(t *testing.T) {
	root := t.TempDir()
	writeService(t, root, "10-api.sh", apiScript, 0o755)
	writeService(t, root, ".hidden.sh", apiScript, 0o755)
	if err := os.MkdirAll(filepath.Join(root, DirName, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	defs, err := Discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(defs) != 1 || defs[0].ID != "api" {
		t.Fatalf("defs = %+v, want just api", defs)
	}
}
