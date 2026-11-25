package sandboxruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// heldSnapshotScript is a snapshot command that reports the docker feature on
// its install step, writes its log, and waits for release to exist on the
// host — a fake guest's command runs there — before it reports again and
// exits with exit.
func heldSnapshotScript(t *testing.T, release string, exit int) string {
	t.Helper()
	line := func(status provisionspec.Status) string {
		data, err := agentlayer.SnapshotProgressLine(status)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSuffix(string(data), "\n")
	}
	installing := line(provisionspec.Status{State: provisionspec.StateProvisioning, Features: map[string]provisionspec.FeatureStatus{
		"docker": {State: provisionspec.StateProvisioning, Step: "install"},
		"brew":   {State: provisionspec.StateOff},
	}})
	last := provisionspec.Status{State: provisionspec.StateReady, Features: map[string]provisionspec.FeatureStatus{
		"docker": {State: provisionspec.StateReady}, "brew": {State: provisionspec.StateOff},
	}}
	if exit != 0 {
		last = provisionspec.Status{State: provisionspec.StateFailed, Features: map[string]provisionspec.FeatureStatus{
			"docker": {State: provisionspec.StateFailed, Error: "install: exit status 100"}, "brew": {State: provisionspec.StateOff},
		}}
	}
	logs := `"$root` + provisionspec.LogDir + `"`
	script := `root="$DISCO_VM_GUEST_ROOT"; mkdir -p ` + logs + ` && printf 'fetching docker\ninstalling docker\n' > ` + logs + `/docker.log
echo 'stray output, not progress'
echo '` + installing + `'
while [ ! -e '` + release + `' ]; do sleep 0.02; done
echo '` + line(last) + `'
`
	if exit != 0 {
		return script + `echo "the docker feature failed" >&2; exit 1`
	}
	return script + `rm -rf "$root` + agentlayer.SnapshotDir + `"/* && : > "$root` + agentlayer.SealedFile + `"`
}

// awaitSnapshotProgress waits for the one build under way to report
// progress, and returns it as the pool reports it.
func awaitSnapshotProgress(t *testing.T, r Runtime) BuildingSnapshot {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		report, err := r.Snapshots(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Building) == 1 && report.Building[0].Progress != nil {
			return report.Building[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the build never reported its progress")
	return BuildingSnapshot{}
}

// A running build is reported with its box and the progress it streams,
// stray output dropped; its feature logs are read through its box; and once
// it fails, its failure keeps the last progress and its logs' tails, which
// the logs read serves (ADR 26-11-23-004 §§1, 2, 4, 6).
func TestARunningBuildReportsItsBoxAndProgressAndAFailureKeepsItsLogs(t *testing.T) {
	f := newMachineFixture(t, nil)
	release := filepath.Join(t.TempDir(), "release")
	f.agents.snapshotRun = heldSnapshotScript(t, release, 1)
	ctx := context.Background()
	r := f.runtime
	req := machineSnapshotCreate("sbx_1")
	key := machineSnapshotKey(t, f, req)
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}

	building := awaitSnapshotProgress(t, r)
	if building.Key != key || building.Box != snapshotMachineName(key) {
		t.Fatalf("building = %+v, want %s in %s", building, key, snapshotMachineName(key))
	}
	if docker := building.Progress.Features["docker"]; building.Progress.State != "provisioning" || docker.State != "provisioning" || docker.Step != "install" {
		t.Fatalf("progress = %+v, want docker provisioning on install", building.Progress)
	}

	var logs bytes.Buffer
	if err := r.SnapshotBuildLogs(ctx, key, SnapshotLogsRequest{Feature: "docker"}, &logs); err != nil {
		t.Fatal(err)
	}
	if logs.String() != "fetching docker\ninstalling docker\n" {
		t.Fatalf("docker's log read %q", logs.String())
	}
	logs.Reset()
	if err := r.SnapshotBuildLogs(ctx, key, SnapshotLogsRequest{Feature: "docker", Lines: 1}, &logs); err != nil || logs.String() != "installing docker\n" {
		t.Fatalf("docker's last line read %q, %v", logs.String(), err)
	}
	if err := r.SnapshotBuildLogs(ctx, key, SnapshotLogsRequest{Feature: "nix"}, &logs); !errors.Is(err, ErrSnapshotFeatureUnknown) {
		t.Fatalf("a feature the build does not provision: %v", err)
	}
	if err := r.SnapshotBuildLogs(ctx, key, SnapshotLogsRequest{Feature: "../../etc/passwd"}, &logs); !errors.Is(err, ErrSnapshotFeatureUnknown) {
		t.Fatalf("a feature named as a path: %v", err)
	}

	// A follow ends when the build does.
	followed := make(chan error, 1)
	var follow bytes.Buffer
	go func() {
		followed <- r.SnapshotBuildLogs(ctx, key, SnapshotLogsRequest{Feature: "docker", Follow: true}, &lockedWriter{w: &follow})
	}()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	select {
	case err := <-followed:
		if err != nil {
			t.Fatalf("follow: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a follow outlived its build")
	}

	report, err := r.Snapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failed) != 1 || report.Failed[0].Progress == nil || report.Failed[0].Progress.Features["docker"].Error != "install: exit status 100" {
		t.Fatalf("failed = %+v, want docker's failure with its error", report.Failed)
	}
	logs.Reset()
	if err := r.SnapshotBuildLogs(ctx, key, SnapshotLogsRequest{}, &logs); err != nil {
		t.Fatal(err)
	}
	if want := "==> " + provisionspec.LogDir + "/docker.log <==\nfetching docker\ninstalling docker\n"; logs.String() != want {
		t.Fatalf("the failed build's kept logs read %q, want %q", logs.String(), want)
	}
	if _, _, err := r.SnapshotBuildExec(ctx, key, machine.ExecRequest{Argv: []string{"true"}}); !errors.Is(err, ErrSnapshotBuildNotFound) {
		t.Fatalf("an exec into a build that ended: %v", err)
	}

	// The logs go with the failure.
	r.sweepSnapshotFailures(time.Now().Add(snapshotRetryAfter))
	if err := r.SnapshotBuildLogs(ctx, key, SnapshotLogsRequest{}, &logs); !errors.Is(err, ErrSnapshotBuildNotFound) {
		t.Fatalf("the kept logs outlived their failure: %v", err)
	}
	if _, err := os.Stat(r.snapshotFailureLogsPath(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the kept logs are still on disk: %v", err)
	}
}

// A build an admin ran a command in is never committed, nor recorded as
// failed, though its provisioning succeeds: the next create asks again (ADR
// 26-11-23-004 §5).
func TestABuildAnAdminOpenedIsNotCommitted(t *testing.T) {
	f := newMachineFixture(t, nil)
	release := filepath.Join(t.TempDir(), "release")
	f.agents.snapshotRun = heldSnapshotScript(t, release, 0)
	ctx := context.Background()
	r := f.runtime
	req := machineSnapshotCreate("sbx_1")
	key := machineSnapshotKey(t, f, req)
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	awaitSnapshotProgress(t, r)

	process, ended, err := r.SnapshotBuildExec(ctx, key, machine.ExecRequest{Argv: []string{"echo", "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if code, err := process.Wait(&out, &out); err != nil || code != 0 || out.String() != "hello\n" {
		t.Fatalf("exec = %d %q, %v", code, out.String(), err)
	}
	_ = process.Close()
	if ended.Err() != nil {
		t.Fatal("the build's session ended before the build did")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	if ended.Err() == nil {
		t.Error("the build ended and its session's context did not")
	}
	if _, ok, _ := r.readSnapshotRecord(key); ok {
		t.Fatal("a build an admin opened was committed")
	}
	if r.snapshotFailedRecently(key, time.Now()) {
		t.Fatal("a build an admin opened was recorded as failed")
	}
	if _, err := f.owner.Instance(ctx, snapshotMachineName(key)); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("a build an admin opened left its machine: %v", err)
	}
	// Nothing holds the next create back from asking again.
	f.agents.mu.Lock()
	f.agents.snapshotRun = ""
	f.agents.mu.Unlock()
	if _, err := r.CreateSandbox(ctx, machineSnapshotCreate("sbx_2")); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	if _, ok, _ := r.readSnapshotRecord(key); !ok {
		t.Fatal("the plan's next build, opened by no one, was not committed")
	}
}

// A key that names no build, and a build with no box yet, are refused, and
// so is a build whose agent reports no progress, which has no feature to
// read a log of.
func TestABuildsLogsAndExecRefuseWhatHasNoBox(t *testing.T) {
	r := &sandboxCore{}
	ctx := context.Background()
	none, queued := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := r.SnapshotBuildLogs(ctx, none, SnapshotLogsRequest{}, &bytes.Buffer{}); !errors.Is(err, ErrSnapshotBuildNotFound) {
		t.Fatalf("logs of no build: %v", err)
	}
	if _, _, err := r.SnapshotBuildExec(ctx, none, machine.ExecRequest{Argv: []string{"true"}}); !errors.Is(err, ErrSnapshotBuildNotFound) {
		t.Fatalf("exec into no build: %v", err)
	}
	// A key that is no plan's is never made a path, a failure's kept logs'
	// included.
	for _, key := range []string{"..", ".", "../" + none, strings.ToUpper(none), none[:63]} {
		if err := r.SnapshotBuildLogs(ctx, key, SnapshotLogsRequest{}, &bytes.Buffer{}); !errors.Is(err, ErrSnapshotBuildNotFound) {
			t.Errorf("logs of key %q: %v", key, err)
		}
		if _, _, err := r.SnapshotBuildExec(ctx, key, machine.ExecRequest{Argv: []string{"true"}}); !errors.Is(err, ErrSnapshotBuildNotFound) {
			t.Errorf("exec into key %q: %v", key, err)
		}
	}
	build := newSnapshotBuild(SnapshotPlanName{}, func() {})
	r.snapshots.active = map[string]*snapshotBuild{queued: build}
	if err := r.SnapshotBuildLogs(ctx, queued, SnapshotLogsRequest{}, &bytes.Buffer{}); !errors.Is(err, ErrSnapshotBuildQueued) {
		t.Fatalf("logs of a queued build: %v", err)
	}
	if _, _, err := r.SnapshotBuildExec(ctx, queued, machine.ExecRequest{Argv: []string{"true"}}); !errors.Is(err, ErrSnapshotBuildQueued) {
		t.Fatalf("exec into a queued build: %v", err)
	}
	if build.opened {
		t.Fatal("a refused exec marked the build opened")
	}
	r.attachSnapshotBuild(build, "box", stubGuest{})
	if err := r.SnapshotBuildLogs(ctx, queued, SnapshotLogsRequest{}, &bytes.Buffer{}); !errors.Is(err, ErrSnapshotBuildNoFeatures) {
		t.Fatalf("logs of a build that reported nothing: %v", err)
	}
	// Once its command has ended its box is stopping, which is said as such
	// rather than as queued.
	r.detachSnapshotBuild(build)
	if err := r.SnapshotBuildLogs(ctx, queued, SnapshotLogsRequest{}, &bytes.Buffer{}); !errors.Is(err, ErrSnapshotBuildProvisioned) {
		t.Fatalf("logs of a build stopping its box: %v", err)
	}
	if _, _, err := r.SnapshotBuildExec(ctx, queued, machine.ExecRequest{Argv: []string{"true"}}); !errors.Is(err, ErrSnapshotBuildProvisioned) {
		t.Fatalf("exec into a build stopping its box: %v", err)
	}
}

// stubGuest is a guest nothing is run in.
type stubGuest struct{ machine.Guest }

// The progress reader takes a line split across writes, drops stray output
// and a line past the limit, and reads the line after it.
func TestTheProgressReaderReadsLinesAcrossWritesAndDropsTheRest(t *testing.T) {
	var got []SnapshotProgress
	w := &snapshotProgressWriter{report: func(p SnapshotProgress) { got = append(got, p) }}
	line, err := agentlayer.SnapshotProgressLine(provisionspec.Status{State: provisionspec.StateProvisioning, Features: map[string]provisionspec.FeatureStatus{
		"go": {State: provisionspec.StateProvisioning, Step: "download"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("hello\n"))
	_, _ = w.Write(line[:10])
	_, _ = w.Write(line[10:])
	_, _ = w.Write([]byte(agentlayer.SnapshotProgressPrefix + strings.Repeat("x", agentlayer.SnapshotProgressLimit) + "\n"))
	_, _ = w.Write(bytes.TrimSuffix(line, []byte("\n")))
	w.flush()
	if len(got) != 2 || got[0].Features["go"].Step != "download" || got[1].State != "provisioning" {
		t.Fatalf("read %+v, want the line twice", got)
	}
}

// A build an admin opened that then fails is still no failure of its plan,
// and keeps nothing of its logs.
func TestABuildAnAdminOpenedThatFailsRecordsNothing(t *testing.T) {
	f := newMachineFixture(t, nil)
	release := filepath.Join(t.TempDir(), "release")
	f.agents.snapshotRun = heldSnapshotScript(t, release, 1)
	ctx := context.Background()
	r := f.runtime
	req := machineSnapshotCreate("sbx_1")
	key := machineSnapshotKey(t, f, req)
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	awaitSnapshotProgress(t, r)
	process, _, err := r.SnapshotBuildExec(ctx, key, machine.ExecRequest{Argv: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = process.Wait(io.Discard, io.Discard)
	_ = process.Close()
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	if r.snapshotFailedRecently(key, time.Now()) {
		t.Fatal("a build an admin opened was recorded as failed")
	}
	if _, err := os.Stat(r.snapshotFailureLogsPath(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a build an admin opened kept its logs: %v", err)
	}
}
