//go:build linux

package sandboxruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/sandboxuser"
)

// The sandbox's data root is its $HOME, and unarchiving is a create against a
// tree that is already full (ADR 0022 §6) -- so the create path is the one place
// where asserting ownership over that tree reaches files a sandbox user wrote.
// It took them: everything under ~ came back owned by root, and the only thing
// that gave any of it back was the sandbox's own boot-time walk, which stops at
// home's own filesystem and does nothing at all for a sandbox running as root.
func TestPrepareSandboxVolumesLeavesSandboxHomeAlone(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("giving a file away requires root")
	}
	state := withTestRoot(t)
	runtime := &DockerSandboxRuntime{root: state, projectID: "proj_a", poolID: "pool_a"}
	const sandboxID = "sandbox-1"
	const uid, gid = 1000, 1000

	claude := filepath.Join(runtime.sandboxDataRootPath(sandboxID), "home", "dev", ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(claude, "settings.json")
	if err := os.WriteFile(settings, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{claude, settings} {
		if err := os.Lchown(path, uid, gid); err != nil {
			t.Fatal(err)
		}
	}

	req := &workerapimodel.PoolSandboxCreateRequest{SandboxId: sandboxID}
	if _, err := runtime.prepareSandboxVolumes(context.Background(), sandboxID, req, sandboxuser.User{}); err != nil {
		t.Fatalf("prepareSandboxVolumes: %v", err)
	}

	for _, path := range []string{claude, settings} {
		if owner, group := ownerOf(t, path); owner != uid || group != gid {
			t.Errorf("%s owned by %d:%d after a create, want it left at %d:%d", path, owner, group, uid, gid)
		}
	}
	// The bind source itself is still the pool's own, which is the whole of what
	// a mountpoint needs: the sandbox chowns the mounted target from the image's
	// declared volume list, not from here.
	if owner, group := ownerOf(t, runtime.sandboxDataRootPath(sandboxID)); owner != 0 || group != 0 {
		t.Errorf("data root owned by %d:%d, want 0:0", owner, group)
	}
}

// The trees this agent writes end to end keep the opposite guarantee: a sandbox
// user may not own the resolved secrets or the manifest, so a create asserts
// root over everything in them.
func TestPrepareSandboxVolumesOwnsWhatItWrites(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("giving a file away requires root")
	}
	state := withTestRoot(t)
	runtime := &DockerSandboxRuntime{root: state, projectID: "proj_a", poolID: "pool_a"}
	const sandboxID = "sandbox-1"

	planted := map[string]string{
		"config":  filepath.Join(runtime.sandboxConfigRoot(sandboxID), "sandbox.json"),
		"secrets": filepath.Join(runtime.sandboxSecretsRoot(sandboxID), "secrets.json"),
	}
	for _, path := range planted {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Lchown(path, 1000, 1000); err != nil {
			t.Fatal(err)
		}
	}

	req := &workerapimodel.PoolSandboxCreateRequest{SandboxId: sandboxID}
	if _, err := runtime.prepareSandboxVolumes(context.Background(), sandboxID, req, sandboxuser.User{}); err != nil {
		t.Fatalf("prepareSandboxVolumes: %v", err)
	}

	for name, path := range planted {
		if owner, group := ownerOf(t, path); owner != 0 || group != 0 {
			t.Errorf("%s %s owned by %d:%d, want 0:0", name, path, owner, group)
		}
	}
}

// A sandbox with no source still gets primary source data: a directory of its
// own inside its data tree, owned by the sandbox user so a harness can write
// there, and bound where a keyed source's shared data would be.
func TestPrepareSandboxVolumesGivesASourcelessSandboxPrivateSourceData(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("giving a file away requires root")
	}
	state := withTestRoot(t)
	runtime := &DockerSandboxRuntime{root: state, projectID: "proj_a", poolID: "pool_a"}
	const sandboxID = "sandbox-1"
	const uid, gid = 1000, 1000
	userUID, userGID := int64(uid), int64(gid)

	req := &workerapimodel.PoolSandboxCreateRequest{SandboxId: sandboxID}
	mounts, err := runtime.prepareSandboxVolumes(context.Background(), sandboxID, req, sandboxuser.User{UID: &userUID, GID: &userGID})
	if err != nil {
		t.Fatalf("prepareSandboxVolumes: %v", err)
	}

	private := filepath.Join(runtime.sandboxDataRootPath(sandboxID), ".discobox", "data-per-source", "primary")
	if owner, group := ownerOf(t, private); owner != uid || group != gid {
		t.Errorf("%s owned by %d:%d, want %d:%d", private, owner, group, uid, gid)
	}
	var found bool
	for _, m := range mounts {
		if m.Target == sandboxSourceDataMount+"/primary" {
			found = true
			if m.Source != runtime.daemonPath(private) || m.ReadOnly {
				t.Errorf("primary source data mount = %#v, want a read-write bind of %s", m, private)
			}
		}
	}
	if !found {
		t.Fatalf("no primary source data mount in %#v", mounts)
	}
}

func ownerOf(t *testing.T, path string) (uid, gid int) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no stat information for %s", path)
	}
	return int(st.Uid), int(st.Gid)
}

// A source's checkout is the sandbox's: a create gives each source an empty
// directory the first time and never touches what the sandbox has cloned into
// it since — no chown, no git — and binds no origin beside it, since the
// sandbox fetches its origin over the pool's git-origins route (ADR 0126 §4).
func TestPrepareSandboxVolumesLeavesSourceCheckoutsToTheSandbox(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("giving a file away requires root")
	}
	state := withTestRoot(t)
	runtime := &DockerSandboxRuntime{root: state, projectID: "proj_a", poolID: "pool_a", paths: linuxPaths}
	const sandboxID = "sandbox-1"
	const uid, gid = 1000, 1000
	userUID, userGID := int64(uid), int64(gid)
	req := deliveryTestRequest()
	req.SandboxId = sandboxID

	checkout := runtime.sandboxSourcePath(sandboxID, "primary")
	cloned := filepath.Join(checkout, ".git", "HEAD")
	if err := os.MkdirAll(filepath.Dir(cloned), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cloned, []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(cloned), cloned} {
		if err := os.Lchown(path, uid, gid); err != nil {
			t.Fatal(err)
		}
	}

	mounts, err := runtime.prepareSandboxVolumes(context.Background(), sandboxID, req, sandboxuser.User{UID: &userUID, GID: &userGID})
	if err != nil {
		t.Fatalf("prepareSandboxVolumes: %v", err)
	}
	for _, path := range []string{filepath.Dir(cloned), cloned} {
		if owner, group := ownerOf(t, path); owner != uid || group != gid {
			t.Errorf("%s owned by %d:%d after a create, want it left at %d:%d", path, owner, group, uid, gid)
		}
	}
	for _, m := range mounts {
		if strings.HasPrefix(m.Target, "/.discobox/origins") {
			t.Errorf("an origin is bound into the sandbox: %#v", m)
		}
	}
}
