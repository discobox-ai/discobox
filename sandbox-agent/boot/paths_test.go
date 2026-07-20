package boot

import (
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/config"
)

func TestVolumeDir(t *testing.T) {
	if got := volumeDir(config.VolumeData, "/var/lib/docker"); got != "/.discobox/data/var/lib/docker" {
		t.Fatalf("data volumeDir = %q", got)
	}
	if got := volumeDir(config.VolumeCache, "/var/lib/discobox/pnpm"); got != "/.discobox/cache/var/lib/discobox/pnpm" {
		t.Fatalf("cache volumeDir = %q", got)
	}
	if got := volumeDir(config.VolumeData, "/home/dev/"); got != "/.discobox/data/home/dev" {
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
		kind     config.VolumeKind
		nonEmpty bool
		want     bool
	}{
		{config.VolumeData, true, true},
		{config.VolumeData, false, false},
		{config.VolumeCache, true, false}, // cache is never an overlay
		{config.VolumeCache, false, false},
	}
	for _, c := range cases {
		if got := useOverlay(c.kind, c.nonEmpty); got != c.want {
			t.Fatalf("useOverlay(%s, %v) = %v, want %v", c.kind, c.nonEmpty, got, c.want)
		}
	}
}

func TestSortVolumesByDepth(t *testing.T) {
	volumes := []config.ResolvedVolume{
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
