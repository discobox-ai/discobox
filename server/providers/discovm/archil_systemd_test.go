package discovm

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/imagecache"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// archilSystemdEnv runs TestArchilGuestRunsThePoolsAndSandboxsUnits, which
// needs Docker and the pool and agent bundles built as local images (`go
// tool task test:e2e:archil-systemd` builds them and runs it).
const archilSystemdEnv = "DISCOBOX_ARCHIL_SYSTEMD_E2E"

// An Archil guest's PID 1 is Archil's own init, which runs nothing of the
// image's. disco-vm's archil driver has Archil's supervisor start systemd as
// PID 1 of a PID and mount namespace of its own, at every boot, and runs
// every guest operation in its namespaces (disco-vm ADR 26-11-20-447). So
// the pool's and the sandbox's agents run as the same units as on boxd, a
// base needs only to carry systemd, and nothing of discobox's knows Archil
// (ADR 26-11-16-619 §4).
//
// Each machine here is a container of archilTestBase whose PID 1 is a
// shell that stands in for Archil's init and supervisor, as disco-vm's own container
// test does (pkg/machine/archil/container_test.go), with systemd registered
// and guest operations entered by the driver's lines (systemd.go there). On
// that, a pool's box is provisioned as the server provisions one
// (poolruntime.ProvisionBox, from the real pool and agent bundles), and a
// sandbox's machine is placed as its pool places one (the agent layer and
// its machine units, a bootstrap, then a restart), and each comes back
// after a reboot. disco-vm's driver and Archil itself are not run.
func TestArchilGuestRunsThePoolsAndSandboxsUnits(t *testing.T) {
	if os.Getenv(archilSystemdEnv) != "1" {
		t.Skipf("set %s=1, with Docker and the pool and agent bundles built, to run it", archilSystemdEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	box := localPoolBox(ctx, t, envOr("DISCOBOX_AGENT_BUNDLE", "discobox-agent:local"), envOr("DISCOBOX_POOL_AGENT_BUNDLE", "discobox-pool-agent-bundle:local"))

	t.Run("guest", func(t *testing.T) {
		g := startArchilGuest(ctx, t, "guest")
		if out := g.outer(ctx, t, "cat", "/proc/1/comm"); out == "systemd" {
			t.Fatal("the container's PID 1 is systemd, not a stand-in for Archil's init")
		}
		// Where guest operations run, systemd is PID 1.
		if out := g.run(ctx, t, "cat", "/proc/1/comm"); out != "systemd" {
			t.Fatalf("PID 1 where guest operations run is %q, not systemd", out)
		}
		g.launched(ctx, t)
		// A file copied in, to /run, and a disk mounted after systemd
		// started, as Archil mounts a data disk at a start, both reach a
		// unit. systemd mounts its /run early in its boot, so the copy
		// waits until systemd answers: one made the moment it is launched
		// can land under that mount, where no unit sees it. What a pool
		// places after a start is on the root disk (/etc, /opt), not there.
		g.answering(ctx, t)
		var tree bytes.Buffer
		tw := tar.NewWriter(&tree)
		writeTar(t, tw, "run/discobox-archil-test", "copied\n")
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := g.CopyTo(ctx, "/", &tree); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(g.disk, "marker"), []byte("on the disk\n"), 0o644); err != nil { //nolint:gosec // G306: read by the container
			t.Fatal(err)
		}
		g.mountDisk(ctx, t, "/.discobox/data")
		got := g.run(ctx, t, "systemd-run", "--wait", "--pipe", "--quiet", "cat", "/run/discobox-archil-test", "/.discobox/data/marker")
		if got != "copied\non the disk" {
			t.Fatalf("a unit read %q, want the copied file and the disk's", got)
		}
		// The test's base carries what the units need.
		g.run(ctx, t, "dpkg-query", "-W", "systemd", "systemd-sysv", "dbus", "nftables", "sudo")
	})

	t.Run("pool", func(t *testing.T) {
		g := startArchilGuest(ctx, t, "pool")
		g.launched(ctx, t)
		placement := archilPoolPlacement(box)
		if _, err := poolruntime.ProvisionBox(ctx, g, enrollAny{}, placement, poolruntime.BoxRecord{}); err != nil {
			t.Fatalf("provision the pool's box: %v\n%s", err, g.journal(ctx, t))
		}
		// The first apply runs the agent in the first slot, which it enables.
		agent := bundle.AgentUnit(bundle.Slots[0])
		first := g.started(ctx, t, agent, "discobox-pool-agent")
		g.reboot(ctx, t)
		if again := g.started(ctx, t, agent, "discobox-pool-agent"); again == first {
			t.Fatalf("%s is the same invocation after a reboot", agent)
		}
	})

	t.Run("sandbox", func(t *testing.T) {
		g := startArchilGuest(ctx, t, "sandbox")
		g.launched(ctx, t)
		placeSandbox(ctx, t, g, box)
		// As a pool restarts a machine onto its first placement, so the
		// wiring runs before anything starts (ADR 26-11-02-023 §3).
		g.reboot(ctx, t)
		g.wired(ctx, t)
		first := g.started(ctx, t, "discobox-sandbox-agent.service", "discobox-sandbox-agent")
		g.reboot(ctx, t)
		g.wired(ctx, t)
		if again := g.started(ctx, t, "discobox-sandbox-agent.service", "discobox-sandbox-agent"); again == first {
			t.Fatal("discobox-sandbox-agent.service is the same invocation after a reboot")
		}
	})
}

// archilTestBase is the base the test's machines run: $DISCOBOX_ARCHIL_TEST_BASE,
// or else the sandbox base image Docker sandboxes run, at a release. It
// carries systemd, and the sudo a sandbox's wiring sets its user up with.
// It is the test's, not the archil provider's default base, which #1516
// sets; the test moves to that once it lands.
func archilTestBase() string {
	return envOr("DISCOBOX_ARCHIL_TEST_BASE", "ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.5")
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// archilInit stands in for Archil's init, /opt/archil/init: PID 1, which
// mounts the guest's data disk while it boots, then starts each service
// Archil's supervisor holds, and waits.
const archilInit = `#!/bin/sh
if [ -e /.fake-archil/disk-at ]; then
	at=$(cat /.fake-archil/disk-at)
	mkdir -p "$at" && mount --bind /.fake-archil/disk "$at"
fi
for f in /var/lib/fake-archil/services/*; do
	case ${f##*/} in *.*) continue ;; esac
	[ -f "$f" ] && /opt/archil/archil-client services start "${f##*/}"
done
trap 'exit 0' TERM
while :; do sleep 1 & wait $!; done
`

// archilFakeGrace is how long the fake's stop waits for a service it sent
// TERM before it gives up, as Archil's waits about 30 s (vm#217).
const archilFakeGrace = 30

// archilClient stands in for Archil's archil-client: services, where a
// service is its argv in a file of the root disk, run again whenever it
// exits, and started at every boot by archilInit; sync, which flushes a
// mount; and halt, the fake's own, which plays Archil's stop: no service is
// run again, each is sent TERM, and the stop waits up to $2 seconds for them
// to exit, failing if one outlives that. A service's restart loop's PID is in
// <name>.loop, and the PID of its process in <name>.pid.
const archilClient = `#!/bin/sh
d=/var/lib/fake-archil/services
case $1 in
sync) exec sync -f "$2" ;;
halt)
	for l in "$d"/*.loop; do [ -f "$l" ] && kill -KILL "$(cat "$l")" 2>/dev/null; done
	pids=$(cat "$d"/*.pid 2>/dev/null)
	[ -n "$pids" ] || exit 0
	kill -TERM $pids 2>/dev/null
	i=0
	while [ $i -lt $(($2 * 10)) ]; do
		up=
		for p in $pids; do kill -0 "$p" 2>/dev/null && up=1; done
		[ -n "$up" ] || exit 0
		sleep 0.1; i=$((i + 1))
	done
	echo "a service outlived the stop's grace of $2 s" >&2
	exit 4 ;;
services) ;;
*) echo "archil-client: $*" >&2; exit 2 ;;
esac
case $2 in
get) [ -f "$d/$3" ] ;;
create)
	n=$3; shift 3
	[ "$1" = -- ] && shift
	[ -e "$d/$n" ] && { echo "a service named $n exists" >&2; exit 1; }
	mkdir -p "$d" && printf '%s\0' "$@" > "$d/$n" || exit
	exec "$0" services start "$n" ;;
start)
	setsid sh -c 'echo $$ > "$1.loop"; while :; do xargs -0 sh -c '\''echo $$ > "$0.pid"; exec "$@"'\'' "$1" < "$1"; sleep 1; done' sh "$d/$3" </dev/null >/dev/null 2>&1 &
	;;
*) echo "archil-client: services $2" >&2; exit 2 ;;
esac
`

// The lines disco-vm's archil driver runs in a guest, as it runs them at the
// pinned revision (pkg/machine/archil/systemd.go): systemd registered with
// Archil's supervisor at Create, every guest operation entered into its
// namespaces, and the halt that readies a guest for its stop, run outside
// them ("archilClient" there is /opt/archil/archil-client).
const (
	archilSystemdService = `trap 'exit 143' TERM INT HUP
mount --make-rshared / || exit
unshare --pid --fork --mount-proc --propagation slave --kill-child /lib/systemd/systemd --system --unit=multi-user.target </dev/null &
u=$!
trap 'kill -KILL "$u"; exit 143' TERM INT HUP
wait "$u"`
	archilHalt = `for d in /proc/[0-9]*; do
	[ "$(cat "$d/comm" 2>/dev/null)" = systemd ] || continue
	grep -Eq '^NSpid:[[:space:]]+[0-9]+[[:space:]]+1$' "$d/status" 2>/dev/null || continue
	kill -KILL "${d#/proc/}" 2>/dev/null
done
sync
if [ -n "$1" ] && cut -d ' ' -f 2 /proc/self/mounts | grep -qxF -- "$1"; then
	a=$(command -v archil) || a=/opt/archil/archil-client
	if ! out=$("$a" sync "$1" 2>&1); then
		echo "archil sync $1: $out" >&2
		exit 3
	fi
fi
exit 0`
	archilEnter = `p=
for d in /proc/[0-9]*; do
	[ "$(cat "$d/comm" 2>/dev/null)" = systemd ] || continue
	grep -Eq '^NSpid:[[:space:]]+[0-9]+[[:space:]]+1$' "$d/status" 2>/dev/null || continue
	p=${d#/proc/}
	break
done
if [ -z "$p" ]; then echo "systemd is not running in the guest" >&2; exit 125; fi
exec nsenter --target "$p" --all -- setpriv --pdeathsig TERM -- sh -c 'cd -- "$1" || exit; u=$2; shift 2
if [ -z "$u" ]; then exec "$@"; fi
exec runuser -u "$u" -- setpriv --pdeathsig TERM -- env HOME="$(getent passwd "$u" | cut -d: -f6)" "$@"' sh "$@"`
)

// archilGuest is a container standing in for an Archil sandbox, and a
// machine.Guest whose operations enter systemd's namespaces, as the archil
// driver's do.
type archilGuest struct {
	name string
	// disk is the guest's data disk, a host directory archilInit mounts.
	disk string
}

var _ machine.Guest = (*archilGuest)(nil)

// startArchilGuest makes and starts a container of archilTestBase standing in
// for an Archil sandbox, and registers systemd as the archil driver's
// Create does.
func startArchilGuest(ctx context.Context, t *testing.T, role string) *archilGuest {
	t.Helper()
	opt := t.TempDir()
	for name, text := range map[string]string{"init": archilInit, "archil-client": archilClient} {
		if err := os.WriteFile(filepath.Join(opt, name), []byte(text), 0o755); err != nil { //nolint:gosec // G306: run in the container
			t.Fatal(err)
		}
	}
	fake := t.TempDir()
	disk := filepath.Join(fake, "disk")
	if err := os.Mkdir(disk, 0o755); err != nil { //nolint:gosec // G301: read by the container
		t.Fatal(err)
	}
	g := &archilGuest{name: fmt.Sprintf("discobox-archil-%s-%d", role, time.Now().UnixNano()), disk: disk}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s's journal:\n%s", role, g.journal(context.Background(), t))
		}
		_ = exec.CommandContext(context.Background(), "docker", "rm", "--force", g.name).Run() //nolint:gosec // G204: the test's own container
		// What systemd's units wrote on the disk is root's.
		_ = exec.CommandContext(context.Background(), "docker", "run", "--rm", "--volume", fake+":/f", "--entrypoint", "rm", archilTestBase(), "-rf", "/f/disk").Run() //nolint:gosec // G204: the test's own directory
	})
	docker(ctx, t, "run", "--detach", "--name", g.name, "--hostname", role, "--privileged",
		"--volume", opt+":/opt/archil:ro", "--volume", fake+":/.fake-archil",
		"--entrypoint", "/opt/archil/init", archilTestBase())
	g.outer(ctx, t, "/opt/archil/archil-client", "services", "create", "disco-vm-systemd", "--", "sh", "-c", archilSystemdService)
	return g
}

func docker(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return strings.TrimSpace(stdout.String())
}

// outer runs argv in the container's own namespaces, as Archil's process
// API does: beside systemd, not in it.
func (g *archilGuest) outer(ctx context.Context, t *testing.T, argv ...string) string {
	t.Helper()
	return docker(ctx, t, append([]string{"exec", g.name}, argv...)...)
}

// run runs argv as a guest operation, and fails the test if it fails.
func (g *archilGuest) run(ctx context.Context, t *testing.T, argv ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := machine.Run(ctx, g, machine.ExecRequest{Argv: argv}, nil, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("%s: exit %d, %v: %s", strings.Join(argv, " "), code, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return strings.TrimSpace(stdout.String())
}

// try runs argv as a guest operation, and answers its output, or false.
func (g *archilGuest) try(ctx context.Context, argv ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	code, err := machine.Run(ctx, g, machine.ExecRequest{Argv: argv}, nil, &stdout, io.Discard)
	return strings.TrimSpace(stdout.String()), err == nil && code == 0
}

// eventually polls check every half second for a minute, failing the test
// with what it last answered.
func eventually(ctx context.Context, t *testing.T, what string, check func() (string, bool)) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	var last string
	for {
		got, ok := check()
		if ok {
			return
		}
		last = got
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("%s: last %q", what, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// launched waits until a guest operation enters systemd's namespaces, as the
// archil driver's Create and Start wait, and no longer: systemd may still be
// booting, so what the test runs next meets a guest as a pool's does.
func (g *archilGuest) launched(ctx context.Context, t *testing.T) {
	t.Helper()
	eventually(ctx, t, "systemd launched", func() (string, bool) {
		return g.try(ctx, "true")
	})
}

// answering waits until systemd answers on its bus, in whatever state its
// boot is: its early mounts, /run's among them, are made by then.
func (g *archilGuest) answering(ctx context.Context, t *testing.T) {
	t.Helper()
	eventually(ctx, t, "systemd answers", func() (string, bool) {
		state, _ := g.try(ctx, "systemctl", "is-system-running")
		return state, state != "" && state != "offline" && state != "unknown"
	})
}

// mountDisk mounts the guest's data disk at path from outside systemd's
// namespaces, now and at every boot, as Archil mounts one.
func (g *archilGuest) mountDisk(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	g.outer(ctx, t, "sh", "-c", `echo "$1" > /.fake-archil/disk-at && mkdir -p "$1" && mount --bind /.fake-archil/disk "$1"`, "sh", path)
}

// reboot stops the guest as the archil driver's Stop does, and starts it
// again: the driver's halt kills systemd and flushes the data disk where it
// is mounted, then Archil's stop sends its services TERM, which
// archilSystemdService must end at within the grace; at the start Archil's
// init starts systemd again, by its supervisor, and nothing of the test's.
func (g *archilGuest) reboot(ctx context.Context, t *testing.T) {
	t.Helper()
	g.outer(ctx, t, "sh", "-c", archilHalt, "sh", "/.discobox/data")
	g.outer(ctx, t, "/opt/archil/archil-client", "halt", strconv.Itoa(archilFakeGrace))
	docker(ctx, t, "restart", "--time", "10", g.name)
	g.launched(ctx, t)
}

// started waits until systemd has started unit, its main process being
// command and still running three seconds on, so a unit that starts and
// fails over and over is not taken for one running, and answers the
// invocation.
func (g *archilGuest) started(ctx context.Context, t *testing.T, unit, command string) string {
	t.Helper()
	var invocation string
	eventually(ctx, t, unit+" started by systemd", func() (string, bool) {
		out, _ := g.try(ctx, "systemctl", "show", "--property=ActiveState,MainPID,InvocationID", unit)
		props := map[string]string{}
		for line := range strings.Lines(out) {
			k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
			props[k] = v
		}
		pid, _ := strconv.Atoi(props["MainPID"])
		if props["ActiveState"] != "active" || pid == 0 {
			return out, false
		}
		comm, _ := g.try(ctx, "cat", "/proc/"+strconv.Itoa(pid)+"/comm")
		if !strings.HasPrefix(command, comm) {
			return out + " comm=" + comm, false
		}
		time.Sleep(3 * time.Second)
		again, _ := g.try(ctx, "systemctl", "show", "--property=ActiveState,MainPID,InvocationID", unit)
		invocation = props["InvocationID"]
		return again, again == out
	})
	return invocation
}

// wired checks a sandbox machine's boot wired it from its data disk.
func (g *archilGuest) wired(ctx context.Context, t *testing.T) {
	t.Helper()
	eventually(ctx, t, "discobox-wire.service ran", func() (string, bool) {
		out, _ := g.try(ctx, "systemctl", "show", "--property=ActiveState,Result", "discobox-wire.service")
		return out, strings.Contains(out, "ActiveState=active") && strings.Contains(out, "Result=success")
	})
	if _, ok := g.try(ctx, "findmnt", "--mountpoint", "/.discobox/data"); !ok {
		t.Fatal("the data disk is not mounted where systemd's units run")
	}
}

func (g *archilGuest) journal(ctx context.Context, t *testing.T) string {
	t.Helper()
	out, _ := g.try(ctx, "journalctl", "--no-pager", "--boot", "--lines=80")
	return out
}

// Exec runs req in systemd's namespaces, through `docker exec`, as Archil's
// process API runs the archil driver's line.
func (g *archilGuest) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	if req.TTY {
		return nil, errors.New("no terminal in the test's guest")
	}
	args := []string{"exec", "--interactive"}
	for _, kv := range req.Env {
		args = append(args, "--env", kv)
	}
	dir := req.Dir
	if dir == "" {
		dir = "/"
	}
	args = append(args, g.name, "sh", "-c", archilEnter, "sh", dir, req.User)
	cmd := exec.CommandContext(ctx, "docker", append(args, req.Argv...)...) //nolint:gosec // G204: the test's own container
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &dockerProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

// CopyTo extracts the tar in dir, in systemd's namespaces, as the archil
// driver's copy does.
func (g *archilGuest) CopyTo(ctx context.Context, dir string, tarStream io.Reader) error {
	var stderr bytes.Buffer
	code, err := machine.Run(ctx, g, machine.ExecRequest{Argv: []string{"sh", "-c", `mkdir -p "$1" && exec tar -x -p -C "$1"`, "sh", dir}}, tarStream, io.Discard, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("tar exit %d: %s", code, bytes.TrimSpace(stderr.Bytes()))
	}
	return err
}

func (g *archilGuest) CopyFrom(ctx context.Context, path string) (io.ReadCloser, error) {
	var stdout, stderr bytes.Buffer
	code, err := machine.Run(ctx, g, machine.ExecRequest{Argv: []string{"tar", "-c", "-C", filepath.Dir(path), filepath.Base(path)}}, nil, &stdout, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("tar exit %d: %s", code, bytes.TrimSpace(stderr.Bytes()))
	}
	if err != nil {
		return nil, err
	}
	return io.NopCloser(&stdout), nil
}

func (g *archilGuest) Info(context.Context) (machine.GuestInfo, error) {
	return machine.GuestInfo{OS: "linux", Arch: runtime.GOARCH, Hostname: g.name}, nil
}

func (g *archilGuest) Shutdown(context.Context, bool) error {
	return errors.New("the test reboots its guest itself")
}

type dockerProcess struct {
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdout, stderr io.ReadCloser
}

func (p *dockerProcess) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *dockerProcess) CloseStdin() error           { return p.stdin.Close() }
func (p *dockerProcess) Resize(uint16, uint16) error { return nil }
func (p *dockerProcess) Close() error {
	if p.cmd.ProcessState == nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	return nil
}

func (p *dockerProcess) Wait(stdout, stderr io.Writer) (int, error) {
	done := make(chan error, 2)
	go func() { _, err := io.Copy(stdout, p.stdout); done <- err }()
	go func() { _, err := io.Copy(stderr, p.stderr); done <- err }()
	for range 2 {
		if err := <-done; err != nil {
			return -1, err
		}
	}
	err := p.cmd.Wait()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// localPoolBox is the box a pool's box is placed with, as the server's
// asset store answers it (assets.Service.PoolBox), read from local images
// of the agent and pool bundles through `docker save`.
func localPoolBox(ctx context.Context, t *testing.T, agentRef, poolRef string) sandbox.PoolBox {
	t.Helper()
	dir := t.TempDir()
	save := exec.CommandContext(ctx, "docker", "save", agentRef, poolRef) //nolint:gosec // G204: the test's own images
	untar := exec.CommandContext(ctx, "tar", "-x", "-C", dir)             //nolint:gosec // G204: the test's own directory
	pipe, err := save.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	untar.Stdin = pipe
	var stderr bytes.Buffer
	save.Stderr = &stderr
	if err := save.Start(); err != nil {
		t.Fatal(err)
	}
	if out, err := untar.CombinedOutput(); err != nil {
		t.Fatalf("untar the saved bundles: %v: %s", err, out)
	}
	if err := save.Wait(); err != nil {
		t.Fatalf("docker save %s %s: %v: %s", agentRef, poolRef, err, bytes.TrimSpace(stderr.Bytes()))
	}
	blob := func(digest string) string { return filepath.Join(dir, "blobs", "sha256", agentlayer.Hex(digest)) }
	read := func(digest string) (imagecache.Manifest, error) {
		data, err := os.ReadFile(blob(digest))
		if err != nil {
			return imagecache.Manifest{}, err
		}
		return imagecache.ParseManifest(data)
	}
	var index struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	digestOf := func(ref string) string {
		for _, m := range index.Manifests {
			name := m.Annotations["io.containerd.image.name"]
			if name == ref || strings.HasSuffix(name, "/"+ref) {
				return m.Digest
			}
		}
		t.Fatalf("docker save wrote no %s", ref)
		return ""
	}
	platform := imagecache.PoolPlatform()
	agentDigest, poolDigest := digestOf(agentRef), digestOf(poolRef)
	agent, _, err := imagecache.ImageManifest(agentDigest, platform, read)
	if err != nil || len(agent.Layers) != 1 {
		t.Fatalf("the agent bundle %s: %d layers, %v", agentRef, len(agent.Layers), err)
	}
	var blobs []string
	pool, _, err := imagecache.ImageManifest(poolDigest, platform, func(digest string) (imagecache.Manifest, error) {
		blobs = append(blobs, digest)
		return read(digest)
	})
	if err != nil {
		t.Fatalf("the pool bundle %s: %v", poolRef, err)
	}
	for _, layer := range pool.Layers {
		blobs = append(blobs, layer.Digest)
	}
	return sandbox.PoolBox{
		Agent:      sandbox.BundlePin{Name: model.BundleName(agentRef), Digest: agentDigest},
		AgentLayer: agent.Layers[0].Digest,
		Bundles:    []sandbox.BundlePin{{Name: model.BundleName(poolRef), Digest: poolDigest}},
		Blobs:      blobs,
		OpenBlob: func(_ context.Context, digest string) (io.ReadCloser, int64, error) {
			f, err := os.Open(blob(digest))
			if err != nil {
				return nil, 0, err
			}
			info, err := f.Stat()
			if err != nil {
				_ = f.Close()
				return nil, 0, err
			}
			return f, info.Size(), nil
		},
	}
}

// archilPoolPlacement is what an archil provider places its pool's box with
// (poolMachine.placement): a machine pool on archil, reached over the
// control-plane channel, its machine's one URL an edge. Nothing in it
// reaches Archil, so the agent fails at its first call to the driver after
// it is up; it is restarted for as long as the box is a pool's.
func archilPoolPlacement(box sandbox.PoolBox) poolruntime.BoxPlacement {
	url := "https://7302-pool-archil-test.aws-us-east-1.archil.app"
	return poolruntime.BoxPlacement{
		Box: box,
		Bootstrap: poolagent.Bootstrap{
			ControlPlaneURL:     poolagent.ControlPlaneChannelURL,
			ControlPlaneChannel: true,
			ControlPlaneKey:     base64.StdEncoding.EncodeToString(make(ed25519.PublicKey, ed25519.PublicKeySize)),
			ProjectID:           "project-archil-test",
			PoolID:              "pool-archil-test",
			AgentListenURL:      "http://0.0.0.0:7302",
			Runtime:             poolagent.RuntimeMachine,
			Driver:              "archil",
			MachinePool:         "pool-archil-test",
			MachineBase:         archilTestBase(),
			DriverConfig:        json.RawMessage(`{"region":"aws-us-east-1"}`),
			IngressURL:          url,
			PoolURL:             url,
		},
	}
}

// enrollAny is a pool maker that takes any identity the box makes.
type enrollAny struct{}

func (enrollAny) Bootstrap(context.Context) (poolagent.Bootstrap, error) {
	return poolagent.Bootstrap{}, nil
}

func (enrollAny) Box(context.Context, imagecache.Platform) (sandbox.PoolBox, error) {
	return sandbox.PoolBox{}, errors.New("unused")
}
func (enrollAny) Enroll(context.Context, string) error { return nil }
func (enrollAny) RecordHost(context.Context, model.RawJSON, *model.PoolHostCredential) error {
	return nil
}

// placeSandbox places a sandbox's agent in g as its pool places it in a
// machine: the agent layer with the machine's units, from the agent bundle,
// and a bootstrap at /etc/discobox. It is a re-creation, not
// MachineSandboxRuntime.place, which needs a disco-vm driver whose machines
// are these containers: it does not cover place's order, its systemd check,
// the bootstrap the pool builds or its restart decision, which
// sandboxruntime's fake-driver tests do. Its pin is the pool bundle's, which
// is never applied here.
func placeSandbox(ctx context.Context, t *testing.T, g *archilGuest, box sandbox.PoolBox) {
	t.Helper()
	layer, _, err := box.OpenBlob(ctx, box.AgentLayer)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "discobox-sandbox-agent")
	err = agentlayer.ExtractAgent(layer, path)
	_ = layer.Close()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var tree bytes.Buffer
	agent := agentlayer.Agent{Digest: box.AgentLayer, Size: info.Size(), Open: func() (io.ReadCloser, error) { return os.Open(path) }}
	if err := agentlayer.Archive(&tree, agent, false, agentlayer.Pins{{Name: box.Bundles[0].Name, Digest: box.Bundles[0].Digest}}, agentlayer.MachineUnits); err != nil {
		t.Fatal(err)
	}
	if err := g.CopyTo(ctx, "/", &tree); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(make(ed25519.PublicKey, ed25519.PublicKeySize))
	bootstrap := `{"apiVersion":"discobox.ai/sandbox/v1","sandboxId":"sbx_archil_test",` +
		`"provider":{"kind":"machine","projectId":"project-archil-test","poolId":"pool-archil-test","publicKeys":{"controlPlane":"` + key + `"}},` +
		`"user":{"name":"discobox","uid":1000,"gid":1000,"homeDirectory":"/home/discobox"}}`
	tree.Reset()
	tw := tar.NewWriter(&tree)
	writeTar(t, tw, "etc/discobox/sandbox.json", bootstrap)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := g.CopyTo(ctx, "/", &tree); err != nil {
		t.Fatal(err)
	}
	// The sandbox's data disk, which Archil mounts at every start.
	g.mountDisk(ctx, t, "/.discobox/data")
}

func writeTar(t *testing.T, tw *tar.Writer, name, body string) {
	t.Helper()
	dir := filepath.Dir(name)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
}
