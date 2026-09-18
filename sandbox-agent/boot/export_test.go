package boot

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
	"github.com/discobox-ai/discobox/sandboxtree"
	"github.com/discobox-ai/discobox/tarsums"
)

// What stays behind is named by where boot stores each declared path, so the
// export and the boot that wrote the tree cannot disagree about a location --
// including a %HOME% path, which only the sandbox can resolve (ADR 0129 §1).
func TestExportExcludedNamesAreTheBackingDirectories(t *testing.T) {
	volumes, err := harness.ResolveVolumes([]harness.Volume{
		{Path: "%HOME%", Volume: harness.VolumeData},
		{Path: "/var/lib/docker", Volume: harness.VolumeData, ExcludeFromExport: true},
		{Path: "/home/linuxbrew/.linuxbrew", Volume: harness.VolumeData, ExcludeFromExport: true},
		{Path: "%HOME%/.local/share/containers", Volume: harness.VolumeData, ExcludeFromExport: true},
		{Path: "%HOME%/.cache", Volume: harness.VolumeCache},
	}, harness.VolumeRuntime{Home: "/home/ada", UID: 1500, GID: 1600})
	if err != nil {
		t.Fatal(err)
	}
	got := exportExcludedNames(volumes, 1500)
	want := []string{
		"data/var/lib/docker",
		"data/home/linuxbrew/.linuxbrew",
		"data/home/ada/.local/share/containers",
	}
	if len(got) != len(want) {
		t.Fatalf("excluded = %v, want exactly %v", got, want)
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("%s was not excluded; got %v", name, got)
		}
	}
}

// A declaration of "/" would name the data root and export no home at all.
func TestExportExcludedNamesNeverTheDataRoot(t *testing.T) {
	got := exportExcludedNames([]harness.ResolvedVolume{
		{Path: "/", Kind: harness.VolumeData, ExcludeFromExport: true},
	}, 0)
	if len(got) != 0 {
		t.Fatalf("excluded = %v, want the data root left in", got)
	}
}

func TestExportRefusesASubtreeItDoesNotRead(t *testing.T) {
	exportFixture(t, "")
	for _, subtree := range []string{sandboxtree.Origins, "config", "secrets"} {
		if err := export(t.Context(), []string{subtree}, io.Discard); err == nil {
			t.Errorf("export of %q was accepted", subtree)
		}
	}
}

// exportFixture points the export mode at temporary primary volumes and a
// manifest that may not exist, so a test never reads the sandbox it runs in.
func exportFixture(t *testing.T, manifest string) (data, sources string) {
	t.Helper()
	data, sources = t.TempDir(), t.TempDir()
	savedMounts, savedManifest := exportMountPaths, manifestPath
	exportMountPaths = map[string]string{sandboxtree.Data: data, sandboxtree.Sources: sources}
	manifestPath = filepath.Join(t.TempDir(), "sandbox.json")
	t.Cleanup(func() { exportMountPaths, manifestPath = savedMounts, savedManifest })
	if manifest != "" {
		if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return data, sources
}

func writeTestFile(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The archive is named by subtree and ends in SHA256SUMS; a sandbox with no
// sandbox.json travels whole.
func TestExportWritesTheNamedSubtrees(t *testing.T) {
	data, sources := exportFixture(t, "")
	writeTestFile(t, filepath.Join(data, "notes"), "hi\n")
	writeTestFile(t, filepath.Join(sources, "main.go"), "package main\n")

	got := exportedFiles(t, sandboxtree.Data, sandboxtree.Sources)
	if got["data/notes"] != "hi\n" || got["sources/main.go"] != "package main\n" {
		t.Fatalf("archive = %v", got)
	}
}

// End to end from the manifest: a declared %HOME% path is found where boot put
// it for the user the manifest names, and it stays behind with the image's
// other excluded paths while the rest of home travels.
func TestExportLeavesOutWhatTheManifestExcludes(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	t.Setenv("DISCOBOX_USER_UID", "1000")
	t.Setenv("DISCOBOX_USER_GID", "2000")
	t.Setenv("DISCOBOX_USER_GROUP", "")
	t.Setenv("DISCOBOX_USER_NAME", "dev")
	t.Setenv("DISCOBOX_USER_HOME", "/home/dev")
	data, _ := exportFixture(t, `{"volumes":[
		{"path":"%HOME%","volume":"data"},
		{"path":"/var/lib/docker","volume":"data","excludeFromExport":true},
		{"path":"%HOME%/.local/share/containers","volume":"data","excludeFromExport":true}
	]}`)
	writeTestFile(t, filepath.Join(data, "home", "dev", "work.txt"), "mine\n")
	writeTestFile(t, filepath.Join(data, "home", "dev", ".local", "share", "containers", "storage.db"), "big\n")
	writeTestFile(t, filepath.Join(data, "var", "lib", "docker", "overlay2", "l"), "big\n")
	writeTestFile(t, filepath.Join(data, "var", "lib", "discobox", "sandbox-agent.db"), "state\n")

	got := exportedFiles(t, sandboxtree.Data)
	for name, want := range map[string]string{
		"data/home/dev/work.txt":                 "mine\n",
		"data/var/lib/discobox/sandbox-agent.db": "state\n",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	for _, name := range []string{
		"data/home/dev/.local/share/containers/storage.db",
		"data/var/lib/docker/overlay2/l",
	} {
		if _, ok := got[name]; ok {
			t.Errorf("%s traveled; the manifest excludes it", name)
		}
	}
}

// exportedFiles runs the export mode over the fixture and returns its regular
// files by name, verifying the archive's SHA256SUMS on the way.
func exportedFiles(t *testing.T, subtrees ...string) map[string]string {
	t.Helper()
	var buf bytes.Buffer
	if err := export(t.Context(), subtrees, &buf); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	reader := tarsums.NewReader(&buf)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			got[header.Name] = string(body)
		}
	}
	return got
}
