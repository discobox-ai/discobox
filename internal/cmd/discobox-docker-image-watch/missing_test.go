package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/devimage"
)

// A built image can leave the daemon without any file changing — reclamation
// removes a superseded one, a developer prunes — and nothing rebuilt it, so the
// manifest kept naming an image that could not come back and every pool
// reconcile failed against it.
func TestMissingImageSpecsRebuildsWhatLeftTheDaemon(t *testing.T) {
	specs := []imageSpec{
		{name: baseSpecName, baseImage: "discobox-sandbox-base:local"},
		{name: "pool-agent", baseImage: "discobox-pool-agent:local", parent: baseSpecName},
	}
	present := map[string]struct{}{
		"discobox-sandbox-base:local": {},
		"discobox-pool-agent:local":   {},
	}

	if got := specNames(missingFrom(specs, builtIn(present, nil))); len(got) != 0 {
		t.Fatalf("missing = %v, want none when every image is present", got)
	}

	delete(present, "discobox-pool-agent:local")
	got := specNames(missingFrom(specs, builtIn(present, nil)))
	if len(got) != 1 || got[0] != "pool-agent" {
		t.Fatalf("missing = %v, want [pool-agent]", got)
	}
}

// A rebuilt image is a new base, so everything layered on it has to be rebuilt
// too — publishing an image built on a base that no longer exists is how the
// manifest ends up describing something unbuildable. The real chain is base ->
// pool-agent; the names here are synthetic so the mechanism is tested with more
// than one child.
func TestMissingParentRebuildsEveryImageLayeredOnIt(t *testing.T) {
	specs := []imageSpec{
		{name: baseSpecName, baseImage: "discobox-sandbox-base:local"},
		{name: "pool-agent", baseImage: "discobox-pool-agent:local", parent: baseSpecName},
		{name: "middle", baseImage: "middle:local", parent: baseSpecName},
		{name: "leaf-a", baseImage: "leaf-a:local", parent: "middle"},
		{name: "leaf-b", baseImage: "leaf-b:local", parent: "middle"},
	}
	present := map[string]struct{}{
		"discobox-sandbox-base:local": {},
		"discobox-pool-agent:local":   {},
		"leaf-a:local":                {},
		"leaf-b:local":                {},
	}

	got := specNames(missingFrom(specs, builtIn(present, nil)))
	want := map[string]bool{"middle": true, "leaf-a": true, "leaf-b": true}
	if len(got) != len(want) {
		t.Fatalf("missing = %v, want the middle image and both leaves", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("unexpected rebuild of %q", name)
		}
	}
}

// Losing the shared base takes the pool agent with it. The sweep runs to a
// fixed point, so a chain deeper than one level (synthetic here) is taken whole,
// which a single non-repeated sweep would miss.
func TestMissingSharedBaseRebuildsEveryImageBuiltOnIt(t *testing.T) {
	specs := []imageSpec{
		{name: baseSpecName, baseImage: "discobox-sandbox-base:local"},
		{name: "pool-agent", baseImage: "discobox-pool-agent:local", parent: baseSpecName},
		{name: "middle", baseImage: "middle:local", parent: baseSpecName},
		{name: "leaf", baseImage: "leaf:local", parent: "middle"},
	}
	present := map[string]struct{}{
		"discobox-pool-agent:local": {},
		"middle:local":              {},
		"leaf:local":                {},
	}

	got := specNames(missingFrom(specs, builtIn(present, nil)))
	if len(got) != len(specs) {
		t.Fatalf("missing = %v, want every image", got)
	}
}

// A bundle is in no daemon, so what has to still be there is the layout the
// manifest published: one deleted (a `task clean`, a removed build directory)
// is exported again rather than left named by the manifest and .env.
func TestMissingBundleLayoutIsExportedAgain(t *testing.T) {
	dir := t.TempDir()
	digest := "sha256:" + strings.Repeat("0a", 32)
	layout := digestLayout(dir, digest)
	if err := os.WriteFile(layout, []byte("layout"), 0o600); err != nil {
		t.Fatal(err)
	}
	specs := []imageSpec{
		{name: "harness-codex", devPrefix: "discobox-harness-codex:dev-", bundle: true, layoutDir: dir},
		{name: "pool-agent", baseImage: "discobox-pool-agent:local"},
	}
	present := map[string]struct{}{"discobox-pool-agent:local": {}}
	published := []devimage.Bundle{{Reference: devImageTag("discobox-harness-codex:dev-", digest), Digest: digest, Layout: layout}}

	if got := specNames(missingFrom(specs, builtIn(present, published))); len(got) != 0 {
		t.Fatalf("missing = %v, want none while the layout is there", got)
	}
	if got := specNames(missingFrom(specs, builtIn(present, nil))); len(got) != 1 || got[0] != "harness-codex" {
		t.Fatalf("missing = %v, want [harness-codex] when no manifest published it", got)
	}
	if err := os.Remove(layout); err != nil {
		t.Fatal(err)
	}
	got := specNames(missingFrom(specs, builtIn(present, published)))
	if len(got) != 1 || got[0] != "harness-codex" {
		t.Fatalf("missing = %v, want [harness-codex]", got)
	}
}

// A rebuild names a new layout and never rewrites the published one: a pass
// that fails after exporting a bundle leaves the manifest naming a layout that
// still holds its digest. Once a pass publishes, layouts neither it nor the
// manifest it replaced names are removed.
func TestPruneLayoutsKeepsThePublishedAndTheReplaced(t *testing.T) {
	dir := t.TempDir()
	layout := func(b byte) string {
		path := digestLayout(dir, "sha256:"+strings.Repeat(string([]byte{'0', b}), 32))
		if err := os.WriteFile(path, []byte("layout"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	oldest, previous, current := layout('1'), layout('2'), layout('3')
	if err := os.WriteFile(partialLayout(dir), []byte("export in flight"), 0o600); err != nil {
		t.Fatal(err)
	}
	specs := []imageSpec{{name: "harness-codex", bundle: true, layoutDir: dir}}

	pruneLayouts(specs, []devimage.Bundle{{Layout: current}}, []devimage.Bundle{{Layout: previous}})

	for path, want := range map[string]bool{oldest: false, previous: true, current: true, partialLayout(dir): true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s present = %v, want %v", filepath.Base(path), err == nil, want)
		}
	}
}
