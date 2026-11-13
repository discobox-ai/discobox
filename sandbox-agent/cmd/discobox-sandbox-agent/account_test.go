//go:build !windows

package main

import (
	"encoding/base64"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/sandbox-agent/autostop"
	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/intake"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
	"github.com/discobox-ai/discobox/sandbox-agent/secretswatch"
	"github.com/discobox-ai/discobox/sandbox-agent/terminal"
)

// linkFreeTempDir is a test's temporary directory with its links resolved,
// so a hand-over walking it meets only links of the system's: a TMPDIR the
// user reached through a link of their own is refused, as it should be.
func linkFreeTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func accountConfig(name string) config.Config {
	return config.Config{ExecDefaults: config.ExecDefaults{Username: name}, RuntimeDir: ""}
}

// An agent already running as the bootstrap's account has nothing to drop:
// the account is resolved, and nothing is handed over or changed.
func TestDroppingToTheAccountTheAgentIsChangesNothing(t *testing.T) {
	current, err := user.Current()
	if err != nil || current.Uid == "0" {
		t.Skip("needs an account that is not root")
	}
	cfg := accountConfig(current.Username)
	cfg.RuntimeDir = filepath.Join(t.TempDir(), "terminals")
	acct, dropped, err := dropToAccount(cfg, "/nonexistent/sandbox.json")
	if err != nil {
		t.Fatalf("drop to %q: %v", current.Username, err)
	}
	if dropped || acct.UID != os.Geteuid() {
		t.Fatalf("account = %+v, dropped %v; want this process's, nothing dropped", acct, dropped)
	}
	// The terminals' directory is the account's to make, private to it.
	if info, err := os.Stat(cfg.RuntimeDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("terminals directory = %v, %v; want it made 0700", info, err)
	}
}

// An agent that is neither root nor the account cannot become it, and says
// which it is; an account the guest does not have is refused by name.
func TestOnlyRootDropsToAnotherAccount(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can drop to any account")
	}
	if _, _, err := dropToAccount(accountConfig("no-such-discobox-account"), ""); err == nil || !strings.Contains(err.Error(), `"no-such-discobox-account"`) {
		t.Fatalf("unknown account: %v", err)
	}
	if _, _, err := dropToAccount(accountConfig(""), ""); err == nil {
		t.Fatal("a bootstrap naming no account was dropped to")
	}
	other := "nobody"
	if _, err := user.Lookup(other); err != nil {
		t.Skip("no account named nobody")
	}
	if _, _, err := dropToAccount(accountConfig(other), ""); err == nil || !strings.Contains(err.Error(), "neither root nor the bootstrap's account") {
		t.Fatalf("drop to nobody as uid %d: %v", os.Geteuid(), err)
	}
}

// Root believes only a bootstrap the account could not have written.
func TestRootTrustsOnlyABootstrapThatIsRootsAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a file root writes is root's")
	}
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapIsRoots(path); err == nil || !strings.Contains(err.Error(), "not root's alone") {
		t.Fatalf("a bootstrap this account owns: %v", err)
	}
	// /etc/hosts stands for a bootstrap root placed, in a directory only root
	// writes.
	if err := bootstrapIsRoots("/etc/hosts"); err != nil {
		t.Skipf("no root-owned file to stand in: %v", err)
	}
}

// A directory is handed over only when it is reached from / without a link:
// a link the account put in place of a name is refused, never followed.
func TestHandingOverNeverFollowsALink(t *testing.T) {
	dir := linkFreeTempDir(t)
	plain := filepath.Join(dir, "real")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := handOver(plain, os.Getuid(), os.Getgid(), 0o750); err != nil {
		t.Fatalf("hand over a plain directory: %v", err)
	}
	if info, _ := os.Stat(plain); info.Mode().Perm() != 0o750 {
		t.Fatalf("mode = %s, want 0750", info.Mode())
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(plain, link); err != nil {
		t.Fatal(err)
	}
	if err := handOver(link, os.Getuid(), os.Getgid(), 0); err == nil {
		t.Fatal("handed over a directory through a link")
	}
	if err := handOver(filepath.Join(link, "x"), os.Getuid(), os.Getgid(), 0); err == nil {
		t.Fatal("walked through a link on the way to a directory")
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := handOver(file, os.Getuid(), os.Getgid(), 0); err == nil {
		t.Fatal("handed over a file, which a hard link could make anybody's")
	}
	if err := handOver(filepath.Join(dir, "absent"), os.Getuid(), os.Getgid(), 0); err != nil {
		t.Fatalf("an absent directory is nothing to hand over: %v", err)
	}
}

// Everything the agent writes for a boot, or keeps, is under a directory the
// account is handed, or in the intake's directory the account may add to: a
// state path outside them would stay root's, and a dropped agent could not
// write it. A new state path fails here, on Linux, rather than on a Mac.
func TestEveryStatePathIsTheDroppedAgents(t *testing.T) {
	// The agent's own defaults, as a bootstrap naming none of them gets them.
	manifest := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(manifest, []byte(`{"apiVersion": "`+sandboxconfig.APIVersion+`", "sandboxId": "sbx_1",
		"provider": {"kind": "discobox-pool", "projectId": "proj_1", "poolId": "pool_1",
			"publicKeys": {"controlPlane": "`+base64.StdEncoding.EncodeToString(make([]byte, 32))+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults, err := config.Load(manifest)
	if err != nil {
		t.Fatal(err)
	}
	layout := intake.DefaultLayout()
	paths := []string{
		autostop.DefaultLeaseDir, autostop.DefaultSleepDir, autostop.DefaultControlPlaneMarker, autostop.DefaultHandoverMarker,
		provisionspec.StatusPath, provision.ConfigurationPlanPath, terminal.CaptureStateDirectory,
		secretswatch.DefaultPath, sandboxconfig.RawSecretsDir, layout.StatePath, layout.SecretsPath,
		defaults.DatabasePath, defaults.RuntimeDir,
	}
	writable := append(handedOver(config.Config{}), sandboxconfig.SandboxConfigDir)
	for _, path := range paths {
		if path == "" {
			t.Fatal("a state path is empty")
		}
		under := false
		for _, dir := range writable {
			if path == dir || strings.HasPrefix(path, dir+"/") {
				under = true
			}
		}
		if !under {
			t.Errorf("%s is under none of %v", path, writable)
		}
	}
	for _, dir := range []string{layout.ConfigDir, layout.ProxyDir, layout.AgentTLSDir} {
		if dir != sandboxconfig.SandboxConfigDir && filepath.Dir(dir) != sandboxconfig.SandboxConfigDir {
			t.Errorf("the intake writes %s, outside %s", dir, sandboxconfig.SandboxConfigDir)
		}
	}
}

// A link only root can have made is followed: macOS reaches /etc, /var (every
// test's temporary directory) and the template's /run through such links, and
// a walk refusing them would hand over nothing there. A link in a directory
// someone else may write is refused, root's or not.
func TestAWalkFollowsOnlyALinkRootAloneCouldMake(t *testing.T) {
	// On macOS t.TempDir() is reached through /var, root's link to
	// /private/var; elsewhere a TMPDIR may be reached through the user's own.
	if runtime.GOOS == "darwin" {
		raw := t.TempDir()
		fd, found, err := openDir(raw)
		if err != nil || !found {
			t.Fatalf("open %s through macOS's /var link: %v, %v", raw, found, err)
		}
		_ = closeFD(fd)
	}
	dir := linkFreeTempDir(t)
	if info, err := os.Lstat("/bin"); err == nil && info.Mode()&os.ModeSymlink != 0 && ownerOf(info) == 0 {
		fd, found, err := openDir("/bin")
		if err != nil || !found {
			t.Fatalf("open /bin through root's link: %v, %v", found, err)
		}
		_ = closeFD(fd)
	}
	sticky := filepath.Join(dir, "shared")
	if err := os.Mkdir(sticky, 0o1777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o1777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(sticky, "l")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openDir(filepath.Join(sticky, "l")); err == nil {
		t.Fatal("followed a link in a directory others may write")
	}
}

// What the pool placed is handed over only from a directory root alone holds:
// one the account already owns is left as it is, since a name the account
// makes there for a root file could be removed between root's check and its
// change. A directory root holds alone is told apart from one it does not.
func TestFilesAreHandedOverOnlyFromADirectoryRootAloneHolds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory root makes is root's")
	}
	dir := linkFreeTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, "agent.key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, _, err := openDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rootsAlone(fd) {
		t.Fatalf("%s, this account's, reads as root's alone", dir)
	}
	_ = closeFD(fd)
	if err := handOverWithFiles(dir, 0, 0); err != nil {
		t.Fatalf("an account's directory is left alone, not refused: %v", err)
	}
	fd, _, err = openDir("/")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFD(fd)
	if !rootsAlone(fd) {
		t.Skip("/ is not root's alone here")
	}
}
