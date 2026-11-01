package monitors

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireScripts skips a test that runs a monitor script: Windows runs no
// `#!` script by its path, and a Windows sandbox's monitors are its own kind
// of executable, which these scripts are not.
func requireScripts(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("runs a shell script by its path, which Windows cannot")
	}
}

// startInstance enables a monitor whose script is body and runs its runner
// until it returns or timeout passes.
func startInstance(t *testing.T, body string, options map[string]string, timeout time.Duration) (Dirs, error) {
	t.Helper()
	requireScripts(t)
	d := HomeDirs(t.TempDir())
	path := writeMonitor(t, t.TempDir(), "probe.sh", body)
	inst := Instance{Name: "probe", Monitor: "probe", Path: path, Workdir: t.TempDir(), Options: options}
	if _, err := Enable(d, inst, false); err != nil {
		t.Fatal(err)
	}
	inst, err := ReadInstance(d, inst.Name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err = (&Runner{Dirs: d, Instance: inst}).Run(ctx)
	return d, err
}

func pending(t *testing.T, d Dirs) []Event {
	t.Helper()
	got, err := d.Queue().Peek(Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAPollingMonitorReportsOnlyWhatChangesAfterItsBaseline(t *testing.T) {
	// The first run records a baseline and prints nothing; every later one
	// prints the run it is.
	d, err := startInstance(t, `#!/bin/sh
#---
# interval: 1s
#---
n=$(cat "$DISCOBOX_MONITOR_STATE/n" 2>/dev/null || echo 0)
[ "$n" -gt 0 ] && echo "{\"title\":\"run $n\",\"kind\":\"probe.run\"}"
echo $((n + 1)) >"$DISCOBOX_MONITOR_STATE/n"
`, nil, 2500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	got := pending(t, d)
	if len(got) < 1 || got[0].Title != "run 1" || got[0].Instance != "probe" || got[0].Monitor != "probe" {
		t.Fatalf("events = %+v", got)
	}
	st, ok, err := ReadStatus(d, "probe")
	if err != nil || !ok || st.Runs < 2 || st.Events != len(got) {
		t.Fatalf("status = %+v, %v, %v", st, ok, err)
	}
}

func TestAFinalEventEndsAStreamingMonitorAndItsInstance(t *testing.T) {
	start := time.Now()
	d, err := startInstance(t, `#!/bin/sh
#---
#---
echo "deploy started"
echo '{"title":"deploy finished","final":true}'
sleep 60
`, nil, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the runner waited %v for a monitor its final event ended", time.Since(start))
	}
	got := pending(t, d)
	if len(got) != 2 || got[0].Title != "deploy started" || !got[1].Final {
		t.Fatalf("events = %+v", got)
	}
	if _, err := ReadInstance(d, "probe"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("instance still enabled after its final event: %v", err)
	}
	if st, _, _ := ReadStatus(d, "probe"); st.State != StateDone {
		t.Fatalf("state = %q, want done", st.State)
	}
	if finished, _ := Finished(d); len(finished) != 1 {
		t.Fatalf("finished = %+v", finished)
	}
}

func TestAFailingMonitorIsReportedOnceAndItsRecoveryOnce(t *testing.T) {
	d, err := startInstance(t, `#!/bin/sh
#---
# interval: 1s
#---
n=$(cat "$DISCOBOX_MONITOR_STATE/n" 2>/dev/null || echo 0)
echo $((n + 1)) >"$DISCOBOX_MONITOR_STATE/n"
if [ "$n" -lt 2 ]; then echo "gh: HTTP 401: Bad credentials" >&2; exit 1; fi
`, nil, 3500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var failed, recovered []Event
	for _, ev := range pending(t, d) {
		switch ev.Kind {
		case KindFailed:
			failed = append(failed, ev)
		case KindRecovered:
			recovered = append(recovered, ev)
		}
	}
	// Recovery shares the failure's key, so a recovery not yet popped
	// replaces the failure it ends rather than queuing beside it.
	if len(failed)+len(recovered) != 1 || len(recovered) != 1 || recovered[0].Count != 2 {
		t.Fatalf("failed %+v, recovered %+v", failed, recovered)
	}
	logs, _ := os.ReadFile(LogPaths(d, "probe")[1])
	if strings.Count(string(logs), "Bad credentials") != 2 {
		t.Fatalf("stderr log = %q", logs)
	}
}

func TestAFailureCarriesTheEndOfStderr(t *testing.T) {
	d, err := startInstance(t, `#!/bin/sh
#---
# interval: 1m
#---
echo "gh: HTTP 401: Bad credentials" >&2
exit 1
`, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got := pending(t, d)
	if len(got) != 1 || got[0].Kind != KindFailed || !strings.Contains(got[0].Body, "Bad credentials") || !strings.Contains(got[0].Title, "Bad credentials") {
		t.Fatalf("events = %+v", got)
	}
	st, _, _ := ReadStatus(d, "probe")
	if !st.Failing || st.State != StateWaiting || st.NextRunAt == nil {
		t.Fatalf("status = %+v", st)
	}
}

func TestAMonitorGetsItsOptionsAndNoInheritedOnes(t *testing.T) {
	t.Setenv("DISCOBOX_OPT_STRAY", "leaked")
	d, err := startInstance(t, `#!/bin/sh
#---
#---
echo "{\"title\":\"$DISCOBOX_OPT_REPO|$DISCOBOX_OPT_MAX_AGE|${DISCOBOX_OPT_STRAY:-}|$DISCOBOX_MONITOR_NAME\",\"data\":$DISCOBOX_MONITOR_OPTIONS}"
`, map[string]string{"repo": "a/b", "max-age": "3"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got := pending(t, d)
	if len(got) != 1 || got[0].Title != "a/b|3||probe" || !strings.Contains(string(got[0].Data), `"max-age":"3"`) {
		t.Fatalf("events = %+v", got)
	}
}

func TestARunnerStopsWhenItsInstanceIsDisabledAndReplaced(t *testing.T) {
	requireScripts(t)
	d := HomeDirs(t.TempDir())
	path := writeMonitor(t, t.TempDir(), "probe.sh", "#!/bin/sh\n#---\n# interval: 1s\n#---\n")
	if _, err := Enable(d, Instance{Name: "probe", Monitor: "probe", Path: path}, false); err != nil {
		t.Fatal(err)
	}
	inst, err := ReadInstance(d, "probe")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (&Runner{Dirs: d, Instance: inst}).Run(ctx) }()
	time.Sleep(300 * time.Millisecond)
	// Disabled and enabled again under the name, with other options: the
	// file is there, but it is not this runner's instance.
	if err := Disable(d, "probe"); err != nil {
		t.Fatal(err)
	}
	if _, err := Enable(d, Instance{Name: "probe", Monitor: "probe", Path: path, Options: map[string]string{"other": "yes"}}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("the runner kept running an instance that was disabled and replaced")
	}
}

func TestATestRunWritesNoState(t *testing.T) {
	requireScripts(t)
	path := writeMonitor(t, t.TempDir(), "probe.sh", "#!/bin/sh\n#---\n#---\necho hello\n")
	state := t.TempDir()
	var got []Event
	r := &Runner{
		Test:         true,
		Instance:     Instance{Name: "test", Monitor: "probe", Path: path},
		MonitorState: state,
		Stderr:       os.Stderr,
		Emit:         func(ev Event) error { got = append(got, ev); return nil },
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "hello" {
		t.Fatalf("events = %+v", got)
	}
	entries, _ := os.ReadDir(state)
	if len(entries) != 0 {
		t.Fatalf("test wrote %v", entries)
	}
	if _, err := os.Stat(filepath.Join(".", "status.json")); err == nil {
		t.Fatal("test wrote a status file")
	}
}

func TestAnInstanceRunsAtTheIntervalItWasGiven(t *testing.T) {
	// Declared every 60s; the instance asks for 1s, so a second run comes
	// well inside the declared interval.
	d, err := startInstance(t, `#!/bin/sh
#---
# interval: 60s
# options:
# - interval
#---
echo "{\"title\":\"ran\"}"
`, map[string]string{"interval": "1s"}, 2500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok, _ := ReadStatus(d, "probe"); !ok || st.Runs < 2 {
		t.Fatalf("status = %+v", st)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAnUpdatedPollingInstanceRunsWithItsNewOptionsAndKeepsItsState(t *testing.T) {
	requireScripts(t)
	d := HomeDirs(t.TempDir())
	// Each run counts itself in the state directory and prints its count and
	// options, so a run after the update shows both carried on.
	path := writeMonitor(t, t.TempDir(), "probe.sh", `#!/bin/sh
#---
# interval: 1s
#---
n=$(cat "$DISCOBOX_MONITOR_STATE/n" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" >"$DISCOBOX_MONITOR_STATE/n"
echo "run $n pr=$DISCOBOX_OPT_PR"
`)
	inst := Instance{Name: "probe", Monitor: "probe", Path: path, Options: map[string]string{"pr": "1"}}
	if _, err := Enable(d, inst, false); err != nil {
		t.Fatal(err)
	}
	inst, err := ReadInstance(d, "probe")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (&Runner{Dirs: d, Instance: inst}).Run(ctx) }()
	waitFor(t, "a first run", 5*time.Second, func() bool { return len(pending(t, d)) >= 1 })

	if result, err := Enable(d, Instance{Name: "probe", Monitor: "probe", Path: path, Options: map[string]string{"pr": "1,2"}}, true); err != nil || result != Updated {
		t.Fatalf("update = %v, %v", result, err)
	}
	waitFor(t, "a run with the new options", 5*time.Second, func() bool {
		for _, ev := range pending(t, d) {
			if strings.HasSuffix(ev.Title, "pr=1,2") {
				return true
			}
		}
		return false
	})
	cancel()
	<-done
	events := pending(t, d)
	for i, ev := range events {
		if want := "run " + strconv.Itoa(i+1) + " pr="; !strings.HasPrefix(ev.Title, want) {
			t.Fatalf("event %d = %q; want %q…, one count carried across the update", i, ev.Title, want)
		}
	}
}

func TestAnUpdatedStreamingInstanceIsRestartedWithItsNewOptions(t *testing.T) {
	requireScripts(t)
	d := HomeDirs(t.TempDir())
	path := writeMonitor(t, t.TempDir(), "probe.sh", `#!/bin/sh
#---
#---
echo "started pr=$DISCOBOX_OPT_PR"
exec sleep 60
`)
	if _, err := Enable(d, Instance{Name: "probe", Monitor: "probe", Path: path, Options: map[string]string{"pr": "1"}}, false); err != nil {
		t.Fatal(err)
	}
	inst, err := ReadInstance(d, "probe")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	r := &Runner{Dirs: d, Instance: inst, OptionsCheck: 100 * time.Millisecond}
	go func() { done <- r.Run(ctx) }()
	waitFor(t, "the first start", 5*time.Second, func() bool { return len(pending(t, d)) == 1 })

	if _, err := Enable(d, Instance{Name: "probe", Monitor: "probe", Path: path, Options: map[string]string{"pr": "2"}}, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a restart with the new options", 5*time.Second, func() bool { return len(pending(t, d)) == 2 })
	cancel()
	<-done
	events := pending(t, d)
	if events[0].Title != "started pr=1" || events[1].Title != "started pr=2" {
		t.Fatalf("events = %q, %q", events[0].Title, events[1].Title)
	}
	for _, ev := range events {
		if ev.Kind == KindFailed {
			t.Fatalf("a restart for new options was reported as a failure: %+v", ev)
		}
	}
}

func TestSinceReachesOnlyTheFirstRun(t *testing.T) {
	requireScripts(t)
	path := writeMonitor(t, t.TempDir(), "probe.sh", `#!/bin/sh
#---
# interval: 1s
#---
echo "since=${DISCOBOX_MONITOR_SINCE:-none}"
`)
	since := time.Date(2026, 11, 1, 10, 0, 0, 0, time.FixedZone("x", 3600))
	var got []string
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	r := &Runner{
		Test:         true,
		Instance:     Instance{Name: "test", Monitor: "probe", Path: path},
		MonitorState: t.TempDir(),
		Stderr:       os.Stderr,
		Since:        since,
		Emit:         func(ev Event) error { got = append(got, ev.Title); return nil },
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 || got[0] != "since=2026-11-01T09:00:00Z" || got[1] != "since=none" {
		t.Fatalf("runs saw %q", got)
	}
}

func TestAFinalEventFromARunWithOldOptionsDoesNotEndAnUpdatedInstance(t *testing.T) {
	requireScripts(t)
	d := HomeDirs(t.TempDir())
	// A run with pr=1 says it is finished, but only after the update to
	// pr=1,2 has landed; a run with pr=1,2 keeps watching.
	gates := t.TempDir()
	gate, started := filepath.Join(gates, "updated"), filepath.Join(gates, "started")
	path := writeMonitor(t, t.TempDir(), "probe.sh", `#!/bin/sh
#---
# interval: 1s
#---
if [ "$DISCOBOX_OPT_PR" = 1 ]; then
  touch "`+started+`"
  while [ ! -e "`+gate+`" ]; do sleep 0.05; done
  echo '{"title":"everything is closed","final":true}'
else
  echo "watching $DISCOBOX_OPT_PR"
fi
`)
	if _, err := Enable(d, Instance{Name: "probe", Monitor: "probe", Path: path, Options: map[string]string{"pr": "1"}}, false); err != nil {
		t.Fatal(err)
	}
	inst, err := ReadInstance(d, "probe")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (&Runner{Dirs: d, Instance: inst}).Run(ctx) }()
	// The update lands while a run with the old options is under way.
	waitFor(t, "a run with the old options", 5*time.Second, func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	if _, err := Enable(d, Instance{Name: "probe", Monitor: "probe", Path: path, Options: map[string]string{"pr": "1,2"}}, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a run with the new options", 5*time.Second, func() bool {
		for _, ev := range pending(t, d) {
			if ev.Title == "watching 1,2" {
				return true
			}
		}
		return false
	})
	if got, err := ReadInstance(d, "probe"); err != nil || got.Options["pr"] != "1,2" {
		t.Fatalf("instance after the old run's final event = %+v, %v; want the update kept", got, err)
	}
	cancel()
	<-done
}
