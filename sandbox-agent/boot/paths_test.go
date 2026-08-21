package boot

import (
	"testing"

	"github.com/discobox-ai/discobox/harness"
)

func TestVolumeDir(t *testing.T) {
	if got := volumeDir(harness.VolumeData, "/var/lib/docker"); got != "/.discobox/data/var/lib/docker" {
		t.Fatalf("data volumeDir = %q", got)
	}
	if got := volumeDir(harness.VolumeCache, "/var/lib/discobox/pnpm"); got != "/.discobox/cache/var/lib/discobox/pnpm" {
		t.Fatalf("cache volumeDir = %q", got)
	}
	if got := volumeDir(harness.VolumeData, "/home/dev/"); got != "/.discobox/data/home/dev" {
		t.Fatalf("trailing-slash volumeDir = %q", got)
	}
}

func TestOverlayDirs(t *testing.T) {
	upper, work := overlayDirs("/.discobox/data/nix")
	if upper != "/.discobox/data/nix/upper" || work != "/.discobox/data/nix/work" {
		t.Fatalf("overlayDirs = %q, %q", upper, work)
	}
}

func TestUseOverlay(t *testing.T) {
	cases := []struct {
		kind     harness.VolumeKind
		nonEmpty bool
		want     bool
	}{
		{harness.VolumeData, true, true},
		{harness.VolumeData, false, false},
		{harness.VolumeCache, true, false}, // cache is never an overlay
		{harness.VolumeCache, false, false},
	}
	for _, c := range cases {
		if got := useOverlay(c.kind, c.nonEmpty); got != c.want {
			t.Fatalf("useOverlay(%s, %v) = %v, want %v", c.kind, c.nonEmpty, got, c.want)
		}
	}
}

func TestSortVolumesByDepth(t *testing.T) {
	volumes := []harness.ResolvedVolume{
		{Path: "/var/lib/discobox/pnpm"},
		{Path: "/var/lib/discobox"},
		{Path: "/home/dev"},
	}
	sortVolumesByDepth(volumes)
	// /home/dev and /var/lib/discobox both come before the deeper pnpm path.
	if volumes[len(volumes)-1].Path != "/var/lib/discobox/pnpm" {
		t.Fatalf("deepest path not last: %#v", volumes)
	}
	// The parent must precede its nested child.
	var parent, child int
	for i, v := range volumes {
		switch v.Path {
		case "/var/lib/discobox":
			parent = i
		case "/var/lib/discobox/pnpm":
			child = i
		}
	}
	if parent > child {
		t.Fatalf("parent wired after child: %#v", volumes)
	}
}
