package assetcache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pool that upgrades finds the bundles.json its earlier agent kept, which
// named each bundle's kind. It reads as the bundles it names, platform first,
// so the sandbox may still fetch them, and no agent, so the pool places
// nothing in a container that keeps the agent placed in it then. The next
// record replaces it.
func TestARecordFromBeforeTheAgentWasABundleReads(t *testing.T) {
	root := t.TempDir()
	platform := "sha256:" + strings.Repeat("a", 64)
	harness := "sha256:" + strings.Repeat("b", 64)
	legacy := `[{"kind":"harness","name":"discobox-harness-shell","digest":"` + harness + `"},{"kind":"platform","name":"discobox-platform","digest":"` + platform + `"}]`
	if err := os.WriteFile(filepath.Join(root, legacyRecordFileName), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := ReadRecord(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Pin{{Name: "discobox-platform", Digest: platform}, {Name: "discobox-harness-shell", Digest: harness}}
	if !d.Delivered() || d.PlacesAgent() || len(d.Bundles) != 2 || d.Bundles[0] != want[0] || d.Bundles[1] != want[1] {
		t.Fatalf("legacy record = %+v, want its bundles, platform first, and no agent", d)
	}

	if err := WriteRecord(root, Delivery{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, legacyRecordFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the legacy record survived a new one: %v", err)
	}
	if d, err := ReadRecord(root); err != nil || d.Delivered() {
		t.Fatalf("after recording nothing: %+v, %v; want nothing delivered", d, err)
	}
}
