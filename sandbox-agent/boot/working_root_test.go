//go:build linux

package boot

import (
	"os"
	"path/filepath"
	"testing"
)

// sandboxUID/sandboxGID stand in for the sandbox user: ids this process is not
// running as, so a chown that never happened cannot pass for one that did.
const sandboxUID, sandboxGID = 12345, 12345

// The bug this pins: the image creates the working root as root, at a point
// where the sandbox user does not exist yet, and wireSources chowns only the
// targets it binds. A sandbox created with --no-source binds none, so the
// directory its harness starts in -- and, with nothing checked out, the only
// directory it has to work in -- stayed root:root and the sandbox user could
// not create a file in it. A source at /workspace/source left it root-owned
// too, for the same reason.
//
// Giving a directory away is root's privilege, which is the same constraint
// the boot flow runs under and the reason chown_test.go skips too.
func TestSeedWorkingRootGivesTheDirectoryToTheSandboxUser(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("giving a directory to another user requires root")
	}
	id := identity{uid: sandboxUID, gid: sandboxGID, name: "sandbox", home: "/home/sandbox"}

	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, root string)
	}{
		{
			// The ordinary case: the image's own `mkdir -p /workspace`, which
			// ran as root and is what the sandbox user cannot write in.
			name: "root-owned in the image",
			prepare: func(t *testing.T, root string) {
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// An image that ships no working root at all: created here, as
			// root, so it needs the same handover.
			name: "absent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "workspace")
			if tc.prepare != nil {
				tc.prepare(t, root)
			}
			if err := newBooter().seedWorkingRoot(root, id); err != nil {
				t.Fatalf("seedWorkingRoot: %v", err)
			}
			fi, err := os.Stat(root)
			if err != nil {
				t.Fatal(err)
			}
			if !fi.IsDir() {
				t.Fatalf("%s is not a directory", root)
			}
			if uid, gid := ownerOf(t, root); uid != sandboxUID || gid != sandboxGID {
				t.Fatalf("%s owned by %d:%d, want %d:%d", root, uid, gid, sandboxUID, sandboxGID)
			}
		})
	}
}

// Creating the directory is the half that holds without root, so it is asserted
// without it: the skip above must not be the only thing standing between a
// working root that is never created and a green test run.
func TestSeedWorkingRootCreatesADirectoryTheImageDoesNotShip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deep", "workspace")
	id := identity{uid: os.Getuid(), gid: os.Getgid(), name: "sandbox", home: "/home/sandbox"}
	if err := newBooter().seedWorkingRoot(root, id); err != nil {
		t.Fatalf("seedWorkingRoot: %v", err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s is not a directory", root)
	}
}

// The same failure one level down: wireSources MkdirAll's a source target as
// root and chowns only the target itself, so a destination of
// /workspace/repos/api left /workspace/repos root-owned -- and for a target
// outside the working root, nothing would ever chown it.
//
// The other half is what it must not touch: a directory the image already
// shipped is the image's statement about who owns it, and boot passing through
// on the way to a mountpoint is not a reason to overwrite it.
func TestMkdirAllOwnedGivesAwayOnlyWhatItCreated(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("giving a directory to another user requires root")
	}
	root := t.TempDir()
	shipped := filepath.Join(root, "workspace")
	if err := os.MkdirAll(shipped, 0o755); err != nil {
		t.Fatal(err)
	}
	const imageUID, imageGID = 4321, 4321
	if err := os.Chown(shipped, imageUID, imageGID); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(shipped, "repos", "api")
	if err := mkdirAllOwned(target, sandboxUID, sandboxGID); err != nil {
		t.Fatalf("mkdirAllOwned: %v", err)
	}

	for _, p := range []string{filepath.Join(shipped, "repos"), target} {
		if uid, gid := ownerOf(t, p); uid != sandboxUID || gid != sandboxGID {
			t.Errorf("%s owned by %d:%d, want %d:%d", p, uid, gid, sandboxUID, sandboxGID)
		}
	}
	if uid, gid := ownerOf(t, shipped); uid != imageUID || gid != imageGID {
		t.Errorf("pre-existing %s owned by %d:%d, want it left at %d:%d", shipped, uid, gid, imageUID, imageGID)
	}
}
