package builtin

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
)

func environ(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestABuiltinRunsWithTheInstanceTheRunnerDescribes(t *testing.T) {
	state := t.TempDir()
	vars := environ(map[string]string{
		"DISCOBOX_MONITOR_NAME":    "pr-412",
		"DISCOBOX_MONITOR_STATE":   state,
		"DISCOBOX_MONITOR_OPTIONS": `{"repo":"o/r","pr":"412"}`,
	})
	var seen Env
	set := map[string]Monitor{"probe": func(_ context.Context, env Env) error {
		seen = env
		if err := env.WriteState("s.json", map[string]int{"n": 1}); err != nil {
			return err
		}
		return env.Emit(monitors.Event{Kind: "probe.ran", Title: "ran", Key: "k", Final: true, ID: "dropped", Instance: "dropped"})
	}}
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), set, []string{"probe"}, vars, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if seen.Instance != "pr-412" || seen.State != state || seen.Options["pr"] != "412" {
		t.Fatalf("env = %+v", seen)
	}
	// What it printed is the one line the runner reads, with only what a
	// monitor may say.
	if strings.Contains(stdout.String(), "dropped") {
		t.Fatalf("printed fields the queue owns: %s", stdout.String())
	}
	ev, ok := monitors.ParseLine(bytes.TrimSpace(stdout.Bytes()))
	if !ok || ev.Kind != "probe.ran" || ev.Title != "ran" || ev.Key != "k" || !ev.Final {
		t.Fatalf("line %q read as %+v", stdout.String(), ev)
	}
	var got map[string]int
	if ok, err := seen.ReadState("s.json", &got); !ok || err != nil || got["n"] != 1 {
		t.Fatalf("state = %v, %v, %v", got, ok, err)
	}
	if ok, err := seen.ReadState("missing.json", &got); ok || err != nil {
		t.Fatalf("a missing state file = %v, %v", ok, err)
	}
}

func TestABuiltinIsGivenSinceOnlyWhenTheRunnerSetsIt(t *testing.T) {
	var seen Env
	set := map[string]Monitor{"probe": func(_ context.Context, env Env) error { seen = env; return nil }}
	vars := map[string]string{"DISCOBOX_MONITOR_STATE": t.TempDir()}
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), set, []string{"probe"}, environ(vars), &stdout, &stderr); code != 0 || !seen.Since.IsZero() {
		t.Fatalf("without it: exit %d, since %v: %s", code, seen.Since, stderr.String())
	}
	vars["DISCOBOX_MONITOR_SINCE"] = "2026-11-01T03:00:00Z"
	if code := Main(context.Background(), set, []string{"probe"}, environ(vars), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if want := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC); !seen.Since.Equal(want) {
		t.Fatalf("since = %v, want %v", seen.Since, want)
	}
}

func TestAFailingBuiltinExitsWithItsReasonOnStderr(t *testing.T) {
	vars := environ(map[string]string{"DISCOBOX_MONITOR_STATE": t.TempDir()})
	set := map[string]Monitor{"probe": func(context.Context, Env) error { return errors.New("no credential: ask with discobox-access") }}
	var stdout, stderr bytes.Buffer
	if code := Main(context.Background(), set, []string{"probe"}, vars, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "discobox-access") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func TestMainRefusesWhatIsNotABuiltinOrNotUnderARunner(t *testing.T) {
	set := map[string]Monitor{"probe": func(context.Context, Env) error { return nil }}
	for _, tc := range []struct {
		args []string
		vars map[string]string
		want string
	}{
		{nil, nil, "usage"},
		{[]string{"other"}, map[string]string{"DISCOBOX_MONITOR_STATE": "/x"}, "no built-in monitor"},
		{[]string{"probe"}, nil, "DISCOBOX_MONITOR_STATE"},
		{[]string{"probe"}, map[string]string{"DISCOBOX_MONITOR_STATE": "/x", "DISCOBOX_MONITOR_OPTIONS": "{"}, "DISCOBOX_MONITOR_OPTIONS"},
		{[]string{"probe"}, map[string]string{"DISCOBOX_MONITOR_STATE": "/x", "DISCOBOX_MONITOR_SINCE": "an hour ago"}, "DISCOBOX_MONITOR_SINCE"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Main(context.Background(), set, tc.args, environ(tc.vars), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%v: exit %d, stderr %q", tc.args, code, stderr.String())
		}
	}
}
