package provisioner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"

	"github.com/discobox-ai/discobox/pool-agent/vmtest"
)

// shimSubcommand is this test binary run as the engine's shim.
const shimSubcommand = "__vm-shim"

// TestMain lets this binary be the two processes the engine starts for a
// fake machine: its guest's agent, as `disco-vm guest` is, and its shim.
func TestMain(m *testing.M) {
	vmtest.RunFakeGuestIfInvoked()
	if len(os.Args) > 1 && os.Args[1] == shimSubcommand {
		if err := runShim(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "shim:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runShim opens the test's engine again, on the root and project it was
// given, and serves the instance it was started for.
func runShim(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: <root> <project> <shim arguments>")
	}
	e, err := openEngine(args[0], args[1])
	if err != nil {
		return err
	}
	defer e.Close()
	return e.ShimMain(context.Background(), args[2:])
}

func openEngine(root, project string) (*engine.Engine, error) {
	driver, err := machine.New("fake")
	if err != nil {
		return nil, err
	}
	e, err := engine.Open(root, driver, nil, project)
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		_ = e.Close()
		return nil, err
	}
	e.ShimCommand = []string{exe, shimSubcommand, root, project}
	return e, nil
}

// fakeMachine boots a machine of disco-vm's fake driver from one of its
// bases, as a sandbox's or a pool's machine boots a driver's base, and
// returns its guest operations.
func fakeMachine(t *testing.T) machine.Guest {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the commands these tests provision with are POSIX shell")
	}
	bases, err := json.Marshal([]machine.Base{{Name: "base", ID: "base-id", GuestOS: machine.OS(runtime.GOOS)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fake.BasesEnv, string(bases))
	e, err := openEngine(t.TempDir(), "provisioner-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	ctx := context.Background()
	inst, err := e.Create(ctx, "base-id", engine.CreateOptions{Name: "sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx, inst, engine.StartOptions{Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	g, err := e.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

const secret = "key-0123456789abcdef"

// tree is a tar of files, each path to its contents; a path ending in "/" is
// a directory, and a mode is 0o755 for a path under bin/ and 0o600 otherwise.
func tree(t *testing.T, files map[string]string, order ...string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range order {
		header := &tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(files[name]))}
		switch {
		case strings.HasSuffix(name, "/"):
			header.Typeflag, header.Mode, header.Size = tar.TypeDir, 0o755, 0
		case strings.Contains(name, "bin/"):
			header.Mode = 0o755
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// installScript stands in for an agent's `install`: it reads the key placed
// beside it, and links and records what it installed only when that has
// changed, so a second run with the same tree is a no-op that says so.
const installScript = `#!/bin/sh
set -eu
root="${DISCO_VM_GUEST_ROOT:-}"
[ -s "$root/etc/agent/key" ] || { echo "no key" >&2; exit 2; }
if [ -e "$root/var/lib/agent/installed" ]; then
	echo "already installed"
	exit 0
fi
mkdir -p "$root/var/lib/agent"
echo v1 > "$root/var/lib/agent/installed"
echo "installed v1"
`

// agentTree is a bundle's tree and a root-only key beside it, placed at the
// guest's root.
func agentTree(t *testing.T) io.Reader {
	t.Helper()
	files := map[string]string{
		"opt/agent/bin/install": installScript,
		"etc/agent/key":         secret,
	}
	return tree(t, files, "opt/", "opt/agent/", "opt/agent/bin/", "opt/agent/bin/install", "etc/", "etc/agent/", "etc/agent/key")
}

func installPlan(t *testing.T) Plan {
	t.Helper()
	return Plan{
		Dir:  "/",
		Tree: agentTree(t),
		// The fake guest runs commands on the host, with its root named in
		// the environment; a real one runs /opt/agent/bin/install.
		Command: machine.ExecRequest{Argv: []string{"sh", "-c", `exec "$DISCO_VM_GUEST_ROOT/opt/agent/bin/install"`}},
	}
}

// readGuestFile reads one file from the machine through disco-vm's copy.
func readGuestFile(t *testing.T, g machine.Guest, path string) (string, int64) {
	t.Helper()
	rc, err := g.CopyFrom(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	header, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), header.Mode
}

// The issue's done-when: against disco-vm's fake driver, a tree is placed and
// a command run, and a second run is a no-op.
func TestProvisionPlacesTreeAndRunsCommandOnce(t *testing.T) {
	g := fakeMachine(t)
	ctx := context.Background()

	result, err := Provision(ctx, g, installPlan(t))
	if err != nil {
		t.Fatalf("provision: %v (stderr %q)", err, result.Stderr)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "installed v1\n" || len(result.Stderr) != 0 {
		t.Fatalf("first run = %+v", result)
	}
	if key, mode := readGuestFile(t, g, "/etc/agent/key"); key != secret || mode&0o777 != 0o600 {
		t.Fatalf("placed key = %q, mode %o", key, mode)
	}
	if _, mode := readGuestFile(t, g, "/opt/agent/bin/install"); mode&0o100 == 0 {
		t.Fatalf("placed install is not executable: mode %o", mode)
	}
	installed, _ := readGuestFile(t, g, "/var/lib/agent/installed")

	result, err = Provision(ctx, g, installPlan(t))
	if err != nil {
		t.Fatalf("second provision: %v (stderr %q)", err, result.Stderr)
	}
	if string(result.Stdout) != "already installed\n" {
		t.Fatalf("second run = %+v, want a no-op", result)
	}
	if again, _ := readGuestFile(t, g, "/var/lib/agent/installed"); again != installed {
		t.Fatalf("second run changed what was installed: %q, was %q", again, installed)
	}
}

// The issue's done-when: a failing command's exit code and stderr come back,
// and the error names neither the key nor anything else the tree placed.
func TestProvisionReportsAFailingCommand(t *testing.T) {
	g := fakeMachine(t)
	plan := installPlan(t)
	plan.Command.Argv = []string{"sh", "-c", `echo "partial" ; echo "unit failed to start" >&2 ; exit 3`}

	result, err := Provision(context.Background(), g, plan)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode != 3 {
		t.Fatalf("err = %v, want an exit error with status 3", err)
	}
	if result.ExitCode != 3 || string(result.Stderr) != "unit failed to start\n" || string(result.Stdout) != "partial\n" {
		t.Fatalf("result = %+v", result)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error %q names the placed key", err)
	}
}

// A command that hangs past its deadline still returns what it wrote, which
// is where it says what it waits on.
func TestProvisionKeepsAHungCommandsOutput(t *testing.T) {
	g := fakeMachine(t)
	plan := installPlan(t)
	plan.Command.Argv = []string{"sh", "-c", `echo "waiting for the unit" >&2; exec sleep 60`}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := Provision(ctx, g, plan)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if result.ExitCode != -1 || string(result.Stderr) != "waiting for the unit\n" {
		t.Fatalf("result = %+v", result)
	}
}

// A tree that cannot be placed fails before anything runs, and its error does
// not carry the tree's contents.
func TestProvisionRefusedCopyNamesNoSecret(t *testing.T) {
	g := fakeMachine(t)
	if _, err := Provision(context.Background(), g, installPlan(t)); err != nil {
		t.Fatal(err)
	}
	// A directory where the tree has a file: the copy cannot replace it.
	plan := installPlan(t)
	plan.Tree = tree(t, map[string]string{"etc/agent/key/": "", "etc/agent/key/x": secret}, "etc/agent/key/", "etc/agent/key/x")
	plan.Command.Argv = []string{"sh", "-c", `echo ran > "$DISCO_VM_GUEST_ROOT/ran"`}

	_, err := Provision(context.Background(), g, plan)
	if err == nil {
		t.Fatal("provision placed a directory over a file")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error %q names the placed key", err)
	}
	if _, err := g.CopyFrom(context.Background(), "/ran"); err == nil {
		t.Fatal("the command ran after the tree failed to place")
	}
}

func TestProvisionRefusesAnIncompletePlan(t *testing.T) {
	cmd := machine.ExecRequest{Argv: []string{"true"}}
	for name, plan := range map[string]Plan{
		"no dir":     {Tree: strings.NewReader(""), Command: cmd},
		"no tree":    {Dir: "/", Command: cmd},
		"no command": {Dir: "/", Tree: strings.NewReader("")},
		"tty":        {Dir: "/", Tree: strings.NewReader(""), Command: machine.ExecRequest{Argv: []string{"true"}, TTY: true}},
	} {
		if _, err := Provision(context.Background(), nil, plan); err == nil {
			t.Errorf("%s: provisioned", name)
		}
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	var b tailBuffer
	big := strings.Repeat("a", outputLimit)
	_, _ = b.Write([]byte(big))
	_, _ = b.Write([]byte("tail"))
	got := b.Bytes()
	if len(got) != outputLimit || !strings.HasSuffix(string(got), "tail") {
		t.Fatalf("kept %d bytes ending %q", len(got), got[len(got)-8:])
	}
	_, _ = b.Write([]byte(big + "end"))
	if got := b.Bytes(); len(got) != outputLimit || !strings.HasSuffix(string(got), "end") {
		t.Fatalf("kept %d bytes after a write longer than the limit", len(got))
	}
}
