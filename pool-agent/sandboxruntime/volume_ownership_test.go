//go:build linux

package sandboxruntime

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	workerapimodel "github.com/obot-platform/discobox/pool-agent/api/model"
	"github.com/obot-platform/discobox/sandboxuser"
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
	withTestRoot(t)
	runtime := &DockerSandboxRuntime{projectID: "proj_a", poolID: "pool_a"}
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
	if _, _, err := runtime.prepareSandboxVolumes(context.Background(), sandboxID, req, sandboxuser.User{}); err != nil {
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
	withTestRoot(t)
	runtime := &DockerSandboxRuntime{projectID: "proj_a", poolID: "pool_a"}
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
	if _, _, err := runtime.prepareSandboxVolumes(context.Background(), sandboxID, req, sandboxuser.User{}); err != nil {
		t.Fatalf("prepareSandboxVolumes: %v", err)
	}

	for name, path := range planted {
		if owner, group := ownerOf(t, path); owner != 0 || group != 0 {
			t.Errorf("%s %s owned by %d:%d, want 0:0", name, path, owner, group)
		}
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
