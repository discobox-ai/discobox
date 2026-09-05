package boot

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
)

func cacheVolume(path string, scope harness.VolumeScope) harness.ResolvedVolume {
	return harness.ResolvedVolume{Path: path, Kind: harness.VolumeCache, Scope: scope}
}

func dataVolume(path string) harness.ResolvedVolume {
	return harness.ResolvedVolume{Path: path, Kind: harness.VolumeData, Scope: harness.VolumeScopeUser}
}

func TestVolumeDir(t *testing.T) {
	if got := volumeDir(dataVolume("/var/lib/docker"), 1000); got != "/.discobox/data/var/lib/docker" {
		t.Fatalf("data volumeDir = %q", got)
	}
	if got := volumeDir(cacheVolume("/var/lib/discobox/pnpm", harness.VolumeScopeUser), 1000); got != "/.discobox/cache/.users/1000/var/lib/discobox/pnpm" {
		t.Fatalf("cache volumeDir = %q", got)
	}
	if got := volumeDir(dataVolume("/home/dev/"), 1000); got != "/.discobox/data/home/dev" {
		t.Fatalf("trailing-slash volumeDir = %q", got)
	}
}

// The bug this partition exists for: two clients of one server whose local
// accounts have different uids run sandboxes in one pool, against one cache
// directory that every declared path chowns to its own user (ADR 0094).
func TestVolumeDirPartitionsTheCacheByUID(t *testing.T) {
	target := cacheVolume("/home/darren/.cache", harness.VolumeScopeUser)
	darrenOnLinux := volumeDir(target, 1000)
	darrenOnTheOtherMachine := volumeDir(target, 4000)
	if darrenOnLinux == darrenOnTheOtherMachine {
		t.Fatalf("two uids share one cache directory %q", darrenOnLinux)
	}

	// The same uid is the same user, whatever else differs: sharing is what a
	// pool-shared cache is for, and only ownership decides whether it is safe.
	if volumeDir(target, 1000) != darrenOnLinux {
		t.Fatal("one uid was given two cache directories")
	}

	// Being root-owned is not the exemption -- saying so is. A cache path that
	// declares no scope is the sandbox user's, whoever the image says owns the
	// mountpoint, because an undeclared uid is still a directory the sandbox
	// user fills.
	rootOwned := cacheVolume("/opt/build-cache", "")
	rootOwned.UID, rootOwned.GID = new(int), new(int)
	if got := volumeDir(rootOwned, 4000); got != "/.discobox/cache/.users/4000/opt/build-cache" {
		t.Fatalf("undeclared-scope cache volumeDir = %q, want the user's partition", got)
	}

	// A data volume is this sandbox's own, so it has nobody to be partitioned
	// from, and moving it would strand every existing sandbox's home.
	if got := volumeDir(dataVolume("/home/darren"), 4000); got != "/.discobox/data/home/darren" {
		t.Fatalf("data volumeDir = %q, want it unpartitioned", got)
	}
}

// A shared cache path is the declared exception: /nix is root-owned,
// content-addressed and reached through a root daemon, so every uid belongs on
// one store. It keeps the unpartitioned location, which is where such a tree
// already is -- so declaring the scope moves nothing and re-seeds nothing.
func TestVolumeDirLeavesASharedCachePathWhereItIs(t *testing.T) {
	nix := cacheVolume("/nix", harness.VolumeScopeShared)
	mine := volumeDir(nix, 1000)
	if mine != "/.discobox/cache/nix" {
		t.Fatalf("shared cache volumeDir = %q, want the unpartitioned path", mine)
	}
	if theirs := volumeDir(nix, 4000); theirs != mine {
		t.Fatalf("shared cache volumeDir = %q for another uid, want one directory for all of them", theirs)
	}
	if !strings.HasPrefix(volumeDir(cacheVolume("/nix", harness.VolumeScopeUser), 1000), "/.discobox/cache/"+cacheUsersDir+"/") {
		t.Fatal("the same path scoped to the user was not partitioned")
	}
}

// The partition sits beside the target paths mirrored into it, so its name has
// to be one no image can declare as a cache target.
func TestCacheUsersDirCannotBeAMirroredTarget(t *testing.T) {
	if !strings.HasPrefix(cacheUsersDir, ".") {
		t.Fatalf("cache partition %q is a plausible absolute path element", cacheUsersDir)
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
