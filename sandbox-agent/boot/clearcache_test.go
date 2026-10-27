package boot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
)

// Clearing a machine sandbox's caches empties every cache path its bootstrap
// declares, keeping each directory, and touches no data path, one declared
// inside a cache included, as /nix's profiles are (ADR 26-10-23-551 §3).
func TestClearCachesEmptiesTheDeclaredCachesInPlace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a sandbox's caches are a Linux box's")
	}
	t.Cleanup(runuser.FixedDatabase())
	root := t.TempDir()
	cache := filepath.Join(root, "home", "dev", ".cache", "go-build")
	data := filepath.Join(root, "home", "dev", "work")
	nested := filepath.Join(cache, "var", "profiles")
	for _, file := range []string{filepath.Join(cache, "ab", "object"), filepath.Join(cache, "top"), filepath.Join(cache, "var", "db"), filepath.Join(nested, "default"), filepath.Join(data, "notes")} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	uid := int64(1000)
	bootstrap := sandboxconfig.Config{
		User: sandboxconfig.User{UID: &uid, Name: "dev"},
		Volumes: []harness.Volume{
			{Path: cache, Volume: harness.VolumeCache},
			{Path: data, Volume: harness.VolumeData},
			{Path: nested, Volume: harness.VolumeData},
			{Path: filepath.Join(root, "never-made"), Volume: harness.VolumeCache},
		},
	}
	path := filepath.Join(root, "sandbox.json")
	encoded, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}

	cleared, err := ClearCaches(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cleared, []string{cache, filepath.Join(root, "never-made")}) {
		t.Fatalf("cleared %v, want both declared caches", cleared)
	}
	var left []string
	if err := filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			left = append(left, path)
		}
		return err
	}); err != nil {
		t.Fatalf("cache directory: %v, want it kept", err)
	}
	if !slices.Equal(left, []string{filepath.Join(nested, "default")}) {
		t.Fatalf("cache holds %v, want only the data volume inside it", left)
	}
	if _, err := os.Stat(filepath.Join(data, "notes")); err != nil {
		t.Fatalf("data: %v, want it untouched", err)
	}
}
