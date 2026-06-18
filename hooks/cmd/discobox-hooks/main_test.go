package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	hooks "github.com/obot-platform/discobox/hooks"
	hookapi "github.com/obot-platform/discobox/hooks/api"
	"github.com/obot-platform/discobox/hooks/client"
	"github.com/obot-platform/discobox/hooks/models"
	"github.com/spf13/cobra"
)

func TestRootCommandRejectsUnknownOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(cliOptions{stdout: &stdout, stderr: &stderr})
	cmd.SetArgs([]string{"-o", "yaml", "list"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "unsupported output format") {
		t.Fatalf("expected unsupported output error, got %v", err)
	}
}

func TestCommandLayout(t *testing.T) {
	cmd := newRootCommand(cliOptions{})
	for _, args := range [][]string{
		{"daemon"},
		{"daemon", "status"},
		{"daemon", "shutdown"},
		{"ls"},
		{"list"},
		{"run"},
		{"db"},
		{"db", "runs"},
		{"db", "changes"},
		{"db", "snapshots"},
		{"db", "queue"},
	} {
		if found, _, err := cmd.Find(args); err != nil || found == nil {
			t.Fatalf("Find(%v) = cmd %#v err %v", args, found, err)
		}
	}
	for _, args := range [][]string{
		{"rerun"},
		{"status"},
		{"shutdown"},
	} {
		if found, _, err := cmd.Find(args); err == nil && found != cmd {
			t.Fatalf("Find(%v) unexpectedly found %q", args, found.CommandPath())
		}
	}
}

func TestDBChangesLimitZeroSendsExplicitLimit(t *testing.T) {
	temp, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temp) })
	t.Setenv("XDG_STATE_HOME", filepath.Join(temp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(temp, "run"))
	repoRoot := filepath.Join(temp, "repo")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatal(err)
	}
	paths := computeSessionPaths(repoRoot, "test-session")
	if err := os.MkdirAll(paths.RuntimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	seen := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/changes" {
			http.NotFound(w, r)
			return
		}
		seen <- r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.ChangesResponse{Changes: []client.ObservedFileChange{}})
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())

	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(cliOptions{
		repoRoot:  repoRoot,
		sessionID: "test-session",
		noStart:   true,
		stdout:    &stdout,
		stderr:    &stderr,
	})
	cmd.SetArgs([]string{"db", "changes", "--limit", "0"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("db changes --limit 0: %v\nstderr: %s", err, stderr.String())
	}
	select {
	case got := <-seen:
		if got != "limit=0" {
			t.Fatalf("expected explicit limit=0 query, got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for db changes request")
	}
}

func TestWriteHooksTable(t *testing.T) {
	a := &app{opts: cliOptions{output: "table"}}
	cmd, out := testOutputCommand()
	hooksList := []client.HookStatus{{
		Hook:      hooks.Hook{ID: "lint", Name: "Lint", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Pattern: "**/*.go"},
		Status:    "success",
		RunCount:  2,
		FailCount: 1,
	}}
	if err := a.writeHooks(cmd, hooksList); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"ID", "NAME", "TYPE", "ENGINE", "STATUS", "lint", "Lint", "**/*.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("table output missing %q: %s", want, got)
		}
	}
}

func TestWriteHooksJSON(t *testing.T) {
	a := &app{opts: cliOptions{output: "json"}}
	cmd, out := testOutputCommand()
	if err := a.writeHooks(cmd, []client.HookStatus{{Hook: hooks.Hook{ID: "lint"}}}); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Hooks []client.HookStatus `json:"hooks"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out.String())
	}
	if len(decoded.Hooks) != 1 || decoded.Hooks[0].Hook.ID != "lint" {
		t.Fatalf("unexpected json: %#v", decoded)
	}
}

func TestFilterRunTargetsOnlyIncludesPendingUnphasedAndRequestedPhase(t *testing.T) {
	statuses := []client.HookStatus{
		{Hook: hooks.Hook{ID: "lint"}, Status: models.StatusQueued},
		{Hook: hooks.Hook{ID: "review", Phase: "review"}, Status: models.StatusQueued},
		{Hook: hooks.Hook{ID: "deploy", Phase: "deploy"}, Status: models.StatusQueued},
		{Hook: hooks.Hook{ID: "success"}, Status: models.StatusSuccess},
		{Hook: hooks.Hook{ID: "failed"}, Status: models.StatusFailure},
	}
	got := filterRunTargets(statuses, runTargetOptions{Phase: "review"})
	if strings.Join(got, ",") != "lint,review,failed" {
		t.Fatalf("unexpected review targets: %v", got)
	}

	got = filterRunTargets(statuses, runTargetOptions{})
	if strings.Join(got, ",") != "lint,failed" {
		t.Fatalf("unexpected unphased targets: %v", got)
	}

	got = filterRunTargets(statuses, runTargetOptions{Phase: "review", Force: true})
	if strings.Join(got, ",") != "lint,review,success,failed" {
		t.Fatalf("unexpected forced review targets: %v", got)
	}

	got = filterRunTargets(statuses, runTargetOptions{Force: true, AllPhases: true})
	if strings.Join(got, ",") != "lint,review,deploy,success,failed" {
		t.Fatalf("unexpected forced all-phase targets: %v", got)
	}
}

func TestWriteStatusTable(t *testing.T) {
	a := &app{opts: cliOptions{output: "table"}}
	cmd, out := testOutputCommand()
	if err := a.writeStatus(cmd, &client.StatusResponse{SessionID: "s1", RepoRoot: "/repo", Queued: 3, Hooks: []client.HookStatus{{}}}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"SESSION", "REPO", "QUEUED", "s1", "/repo", "3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status table missing %q: %s", want, got)
		}
	}
}

func TestWriteDatabaseInspectionTables(t *testing.T) {
	a := &app{opts: cliOptions{output: "table"}}
	for name, tt := range map[string]struct {
		want string
		fn   func(*cobra.Command) error
	}{
		"runs": {want: "STATUS", fn: func(cmd *cobra.Command) error {
			return a.writeRuns(cmd, []client.Run{{ID: "run-1", HookID: "lint", Status: models.StatusFailure, ExitCode: 1, Error: "boom"}})
		},
		},
		"changes": {want: "DIFF_BYTES", fn: func(cmd *cobra.Command) error {
			return a.writeObservedChanges(cmd, []client.ObservedFileChange{{ID: "change-1", Path: "main.go", Kind: "modified", Diff: "diff"}})
		},
		},
		"snapshots": {want: "PATCH_BYTES", fn: func(cmd *cobra.Command) error {
			return a.writeSnapshots(cmd, []client.WorkspaceSnapshot{{ID: "snapshot-1", PatchBytes: 42, ChangedFiles: []client.ChangedFile{{Path: "main.go"}}}})
		},
		},
		"queue": {want: "POSITION", fn: func(cmd *cobra.Command) error {
			return a.writeQueue(cmd, []client.QueuedHook{{HookID: "lint", Position: 7, ChangedFiles: []client.ChangedFile{{Path: "main.go"}}}})
		},
		},
	} {
		cmd, out := testOutputCommand()
		if err := tt.fn(cmd); err != nil {
			t.Fatalf("%s writer: %v", name, err)
		}
		if got := out.String(); !strings.Contains(got, tt.want) {
			t.Fatalf("%s table did not include header %q: %s", name, tt.want, got)
		}
	}
}

func TestWriteRunsJSON(t *testing.T) {
	a := &app{opts: cliOptions{output: "json"}}
	cmd, out := testOutputCommand()
	if err := a.writeRuns(cmd, []client.Run{{ID: "run-1", HookID: "lint"}}); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Runs []client.Run `json:"runs"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out.String())
	}
	if len(decoded.Runs) != 1 || decoded.Runs[0].ID != "run-1" {
		t.Fatalf("unexpected runs json: %#v", decoded)
	}
}

func TestWriteEventsTable(t *testing.T) {
	a := &app{opts: cliOptions{output: "table"}}
	cmd, out := testOutputCommand()
	events := []client.Event{{ID: "event-1", Type: "hook.run.finished", HookID: "lint", RunID: "run-7", Message: "hook run finished"}}
	if err := a.writeEvents(cmd, events); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"ID", "TIME", "TYPE", "HOOK", "RUN", "MESSAGE", "hook.run.finished", "lint", "run-7"} {
		if !strings.Contains(got, want) {
			t.Fatalf("events table missing %q: %s", want, got)
		}
	}
}

func TestWriteEventsJSONUsesJSONL(t *testing.T) {
	a := &app{opts: cliOptions{output: "json"}}
	cmd, out := testOutputCommand()
	events := []client.Event{
		{ID: "event-1", Type: "hook.run.started", HookID: "lint"},
		{ID: "event-2", Type: "hook.run.finished", HookID: "lint"},
	}
	if err := a.writeEvents(cmd, events); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected one JSON object per line, got %d lines: %q", len(lines), out.String())
	}
	for i, line := range lines {
		var decoded client.Event
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line %d is invalid json: %v\n%s", i, err, line)
		}
		if decoded.ID != events[i].ID || decoded.Type != events[i].Type {
			t.Fatalf("unexpected event on line %d: %#v", i, decoded)
		}
	}
	var envelope struct {
		Events []client.Event `json:"events"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err == nil {
		t.Fatalf("expected JSONL, not a JSON event envelope: %#v", envelope)
	}
}

func TestWriteJSONLineIsCompact(t *testing.T) {
	var out bytes.Buffer
	event := client.Event{ID: "event-1", Type: "hook.run.finished", HookID: "lint"}
	if err := writeJSONLine(&out, event); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected compact single-line json, got %d lines: %q", len(lines), out.String())
	}
	var decoded client.Event
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("invalid json line: %v\n%s", err, lines[0])
	}
	if decoded.ID != event.ID || decoded.Type != event.Type || decoded.HookID != event.HookID {
		t.Fatalf("unexpected decoded event: %#v", decoded)
	}
}

func TestEventsListTypesTableDoesNotRequireDaemon(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(cliOptions{stdout: &stdout, stderr: &stderr, noStart: true})
	cmd.SetArgs([]string{"events", "--list-types"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("events --list-types: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{"TYPE", "DESCRIPTION", "DETAILS", "daemon.started", "hook.run.finished", "file.change.observed", "daemon_session_id*"} {
		if !strings.Contains(got, want) {
			t.Fatalf("event type table missing %q: %s", want, got)
		}
	}
}

func TestEventsListTypesJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(cliOptions{stdout: &stdout, stderr: &stderr, output: "json"})
	cmd.SetArgs([]string{"events", "--list-types"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("events --list-types json: %v\nstderr=%s", err, stderr.String())
	}
	var decoded struct {
		EventTypes []hookapi.EventTypeInfo `json:"event_types"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(decoded.EventTypes) == 0 {
		t.Fatal("expected event types")
	}
	if decoded.EventTypes[0].Type == "" || decoded.EventTypes[0].Description == "" || len(decoded.EventTypes[0].Details) == 0 {
		t.Fatalf("expected type descriptions, got %#v", decoded.EventTypes[0])
	}
}

func TestEventsListTypesRejectsFollowAndHookIDs(t *testing.T) {
	for _, args := range [][]string{
		{"events", "--list-types", "--follow"},
		{"events", "--list-types", "lint"},
	} {
		var stdout, stderr bytes.Buffer
		cmd := newRootCommand(cliOptions{stdout: &stdout, stderr: &stderr})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("expected error for args %v", args)
		}
	}
}

func TestKnownEventTypesDocumentProductionEvents(t *testing.T) {
	known := map[string]bool{}
	for _, eventType := range hookapi.KnownEventTypes() {
		if eventType.Description == "" {
			t.Fatalf("event type %q has no description", eventType.Type)
		}
		if len(eventType.Details) == 0 {
			t.Fatalf("event type %q has no detail schema", eventType.Type)
		}
		for _, detail := range eventType.Details {
			if detail.Name == "" || detail.Description == "" {
				t.Fatalf("event type %q has incomplete detail schema: %#v", eventType.Type, detail)
			}
		}
		known[eventType.Type] = true
	}

	events := productionEventLiterals(t)
	for _, eventType := range events {
		if !known[eventType] {
			t.Fatalf("production event type %q is not documented in knownEventTypes", eventType)
		}
	}
}

func productionEventLiterals(t *testing.T) []string {
	t.Helper()
	root := hooksModuleRoot(t)
	recordEventRE := regexp.MustCompile(`\.recordEvent\(\s*"([a-z][a-z0-9]*(?:\.[a-z0-9]+)+)"`)
	managerRecordEventRE := regexp.MustCompile(`\.RecordEvent\([^,\n]+,\s*"([a-z][a-z0-9]*(?:\.[a-z0-9]+)+)"`)
	eventTypeRE := regexp.MustCompile(`Type:\s*"([a-z][a-z0-9]*(?:\.[a-z0-9]+)+)"`)
	events := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "gen" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.ToSlash(path) == filepath.ToSlash(filepath.Join(root, "api", "event_types.go")) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range recordEventRE.FindAllSubmatch(content, -1) {
			events[string(match[1])] = true
		}
		for _, match := range managerRecordEventRE.FindAllSubmatch(content, -1) {
			events[string(match[1])] = true
		}
		for _, match := range eventTypeRE.FindAllSubmatch(content, -1) {
			events[string(match[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan production events: %v", err)
	}
	return sortedKeys(events)
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func hooksModuleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			data, err := os.ReadFile(filepath.Join(wd, "go.mod"))
			if err != nil {
				t.Fatalf("read go.mod: %v", err)
			}
			if strings.Contains(string(data), "module github.com/obot-platform/discobox/hooks") {
				return wd
			}
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("could not find hooks module root")
		}
		wd = parent
	}
}

func testOutputCommand() (*cobra.Command, *bytes.Buffer) {
	out := &bytes.Buffer{}
	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(out)
	return cmd, out
}

func TestWriteHookOutputsSingle(t *testing.T) {
	var out bytes.Buffer
	if err := writeHookOutputs(&out, []hookOutput{{HookID: "lint", Output: "ok"}}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "ok\n" {
		t.Fatalf("single output = %q", got)
	}
}

func TestWriteHookOutputsMultiple(t *testing.T) {
	var out bytes.Buffer
	if err := writeHookOutputs(&out, []hookOutput{{HookID: "lint", Output: "ok\n"}, {HookID: "test", Output: "pass"}}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"==> lint <==", "ok", "==> test <==", "pass"} {
		if !strings.Contains(got, want) {
			t.Fatalf("multi output missing %q: %s", want, got)
		}
	}
}

func TestUniqueStrings(t *testing.T) {
	got := uniqueStrings([]string{"lint", "", "test", "lint", " test "})
	want := []string{"lint", "test"}
	if len(got) != len(want) {
		t.Fatalf("uniqueStrings length = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("uniqueStrings[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFollowEventHookID(t *testing.T) {
	hookID, err := followEventHookID(nil)
	if err != nil || hookID != "" {
		t.Fatalf("all follow target = %q err=%v", hookID, err)
	}
	hookID, err = followEventHookID([]string{"lint"})
	if err != nil || hookID != "lint" {
		t.Fatalf("single follow target = %q err=%v", hookID, err)
	}
	if _, err := followEventHookID([]string{"lint", "test"}); err == nil {
		t.Fatal("expected error for multiple follow hook ids")
	}
	cmd := newRootCommand(cliOptions{})
	eventsCmd, _, err := cmd.Find([]string{"events"})
	if err != nil {
		t.Fatal(err)
	}
	if flag := eventsCmd.Flags().Lookup("follow"); flag == nil || flag.Shorthand != "f" {
		t.Fatalf("expected events -f/--follow flag, got %#v", flag)
	}
	if flag := eventsCmd.Flags().Lookup("list-types"); flag == nil {
		t.Fatalf("expected events --list-types flag")
	}
}

func TestComputeSessionPaths(t *testing.T) {
	stateHome := t.TempDir()
	runtimeHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)

	repoRoot := filepath.Clean("/tmp/repo/")
	paths := computeSessionPaths(repoRoot, "s/1")
	repoKey := repoStateKey(repoRoot)
	wantState := filepath.Join(stateHome, "discobox", "session", "s-1", "hooks", repoKey)
	wantRuntime := filepath.Join(runtimeHome, "discobox", "session", "s-1", "hooks", repoKey)
	if paths.StateDir != wantState {
		t.Fatalf("StateDir = %q, want %q", paths.StateDir, wantState)
	}
	if paths.RuntimeDir != wantRuntime {
		t.Fatalf("RuntimeDir = %q, want %q", paths.RuntimeDir, wantRuntime)
	}
	checks := map[string]string{
		"Socket":  filepath.Join(wantRuntime, "daemon.sock"),
		"Lock":    filepath.Join(wantRuntime, "startup.lock"),
		"DB":      filepath.Join(wantState, "hooks.db"),
		"Runtime": filepath.Join(wantRuntime, "runtime.json"),
	}
	if paths.Socket != checks["Socket"] || paths.Lock != checks["Lock"] || paths.DB != checks["DB"] || paths.Runtime != checks["Runtime"] {
		t.Fatalf("unexpected paths: %#v", paths)
	}
}

func TestXDGPathFallbacks(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := xdgStateHome(); got == "" || filepath.Base(got) != "state" {
		t.Fatalf("xdgStateHome fallback = %q", got)
	}
	if got := xdgRuntimeHome(); got == "" || filepath.Base(got) != "run" {
		t.Fatalf("xdgRuntimeHome fallback = %q", got)
	}
}

func TestResolveSessionID(t *testing.T) {
	old, had := os.LookupEnv("DISCOBOX_SESSION_ID")
	defer func() {
		if had {
			_ = os.Setenv("DISCOBOX_SESSION_ID", old)
		} else {
			_ = os.Unsetenv("DISCOBOX_SESSION_ID")
		}
	}()
	repoRoot := filepath.Clean("/tmp/repo")
	_ = os.Unsetenv("DISCOBOX_SESSION_ID")
	if got, want := resolveSessionID("", repoRoot), repoStateKey(repoRoot); got != want {
		t.Fatalf("default session = %q, want repo hash %q", got, want)
	}
	_ = os.Setenv("DISCOBOX_SESSION_ID", "env-session")
	if got := resolveSessionID("", repoRoot); got != "env-session" {
		t.Fatalf("env session = %q", got)
	}
	if got := resolveSessionID("flag-session", repoRoot); got != "flag-session" {
		t.Fatalf("explicit session = %q", got)
	}
}

func TestClientNewerThanDaemon(t *testing.T) {
	tests := []struct {
		name          string
		clientVersion int64
		daemonVersion int64
		want          bool
	}{
		{name: "client newer", clientVersion: 2, daemonVersion: 1, want: true},
		{name: "same version", clientVersion: 2, daemonVersion: 2},
		{name: "daemon newer", clientVersion: 1, daemonVersion: 2},
		{name: "zero daemon version", clientVersion: 1, daemonVersion: 0, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientNewerThanDaemon(tt.clientVersion, tt.daemonVersion); got != tt.want {
				t.Fatalf("clientNewerThanDaemon(%d, %d) = %t, want %t", tt.clientVersion, tt.daemonVersion, got, tt.want)
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{in: "123", want: 123, ok: true},
		{in: " 456 ", want: 456, ok: true},
		{in: ""},
		{in: "abc"},
		{in: "0"},
		{in: "-1"},
	}
	for _, tt := range tests {
		got, ok := parseVersion(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("parseVersion(%q) = (%d, %t), want (%d, %t)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
