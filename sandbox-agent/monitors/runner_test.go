package monitors

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startInstance enables a monitor whose script is body and runs its runner
// until it returns or timeout passes.
func startInstance(t *testing.T, body string, options map[string]string, timeout time.Duration) (Dirs, error) {
	t.Helper()
	d := HomeDirs(t.TempDir())
	path := writeMonitor(t, t.TempDir(), "probe.sh", body)
	inst := Instance{Name: "probe", Monitor: "probe", Path: path, Workdir: t.TempDir(), Options: options}
	if _, err := Enable(d, inst); err != nil {
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
	d := HomeDirs(t.TempDir())
	path := writeMonitor(t, t.TempDir(), "probe.sh", "#!/bin/sh\n#---\n# interval: 1s\n#---\n")
	if _, err := Enable(d, Instance{Name: "probe", Monitor: "probe", Path: path}); err != nil {
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
	if _, err := Enable(d, Instance{Name: "probe", Monitor: "probe", Path: path, Options: map[string]string{"other": "yes"}}); err != nil {
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
