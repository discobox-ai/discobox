package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// openStore opens a store under a temporary directory and closes it when the
// test ends. The close is what makes the cleanup work: sqlite holds the file
// open, and Windows refuses to remove a file another handle still has, so a
// store left open fails t.TempDir's RemoveAll rather than the assertion.
func openStore(ctx context.Context, t *testing.T, dsn string) *Store {
	t.Helper()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

func TestRecordAndListEvents(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "harness.db"))
	if err := st.RecordExecEvent(ctx, "ex_1", "exec.created", "created", map[string]any{"harnessId": "codex"}); err != nil {
		t.Fatalf("record event: %v", err)
	}
	events, err := st.ListEvents(ctx, ExecEventFilter{ExecID: "ex_1", Limit: 10})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "exec.created" || events[0].Details["harnessId"] != "codex" {
		t.Fatalf("events = %#v", events)
	}
}

func TestPrimaryTerminalLaunchedMarker(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "harness.db")
	st := openStore(ctx, t, dbPath)
	launched, err := st.PrimaryTerminalLaunched(ctx)
	if err != nil {
		t.Fatalf("primary launched: %v", err)
	}
	if launched {
		t.Fatalf("expected primary not launched initially")
	}
	if err := st.MarkPrimaryTerminalLaunched(ctx); err != nil {
		t.Fatalf("mark primary launched: %v", err)
	}
	// Marking twice must be idempotent (upsert).
	if err := st.MarkPrimaryTerminalLaunched(ctx); err != nil {
		t.Fatalf("mark primary launched again: %v", err)
	}
	// A freshly reopened store (simulating a sandbox restart) must observe the
	// durable marker.
	reopened := openStore(ctx, t, dbPath)
	launched, err = reopened.PrimaryTerminalLaunched(ctx)
	if err != nil {
		t.Fatalf("primary launched after reopen: %v", err)
	}
	if !launched {
		t.Fatalf("expected primary launched after mark")
	}
}

func TestRecordAndListHarnessHooks(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "harness.db"))
	_, err := st.RecordHarnessHook(ctx, HarnessHookRecord{
		TerminalID: "agt_1",
		Provider:   "codex",
		Event:      "PreToolUse",
		Payload:    json.RawMessage(`{"tool_name":"Bash"}`),
	})
	if err != nil {
		t.Fatalf("record hook: %v", err)
	}
	hooks, err := st.ListHarnessHooks(ctx, HarnessHookFilter{TerminalID: "agt_1", Limit: 10})
	if err != nil {
		t.Fatalf("list hooks: %v", err)
	}
	if len(hooks) != 1 || hooks[0].Provider != "codex" || hooks[0].Event != "PreToolUse" {
		t.Fatalf("hooks = %#v", hooks)
	}
	if string(hooks[0].Payload) != `{"tool_name":"Bash"}` {
		t.Fatalf("payload = %s", hooks[0].Payload)
	}
}

func TestExecRecordIsDurableAndImmutable(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "harness.db"))
	created := time.Now().UTC()
	rec := execs.Exec{
		ID:        "ex_1",
		Command:   []string{"codex", "say hello"},
		Workdir:   "/workspace",
		TTY:       true,
		CreatedAt: created,
		Metadata:  map[string]string{"harnessId": "codex", "primary": "true"},
	}
	if err := st.SaveExecRecord(ctx, rec); err != nil {
		t.Fatalf("save record: %v", err)
	}
	// Observing status must not touch the immutable record.
	exited := time.Now().UTC()
	code := int64(0)
	if err := st.ObserveExec(ctx, execs.Exec{ID: "ex_1", Status: execs.StatusExited, ExitedAt: &exited, ExitCode: &code}); err != nil {
		t.Fatalf("observe: %v", err)
	}
	// A second save with different metadata must be ignored (immutable).
	if err := st.SaveExecRecord(ctx, execs.Exec{ID: "ex_1", Command: []string{"other"}, Metadata: map[string]string{"harnessId": "changed"}}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	records, err := st.LoadExecRecords(ctx)
	if err != nil {
		t.Fatalf("load records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	got := records[0]
	if got.Metadata["harnessId"] != "codex" || got.Metadata["primary"] != "true" {
		t.Fatalf("metadata not durable/immutable: %v", got.Metadata)
	}
	if len(got.Command) != 2 || got.Command[0] != "codex" {
		t.Fatalf("command = %v", got.Command)
	}
	// Status is joined from the latest ExecState observation.
	if got.Status != execs.StatusExited {
		t.Fatalf("status = %q, want exited (joined from ExecState)", got.Status)
	}
}

func TestObserveExecRecordsTransitions(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "harness.db"))
	createdAt := time.Now().UTC()
	status := execs.Exec{ID: "ex_1", Status: execs.StatusRunning, CreatedAt: createdAt}
	if err := st.ObserveExec(ctx, status); err != nil {
		t.Fatalf("observe running: %v", err)
	}
	exitedAt := time.Now().UTC()
	code := int64(7)
	status.Status = execs.StatusFailed
	status.ExitedAt = &exitedAt
	status.ExitCode = &code
	if err := st.ObserveExec(ctx, status); err != nil {
		t.Fatalf("observe failed: %v", err)
	}
	events, err := st.ListEvents(ctx, ExecEventFilter{ExecID: "ex_1", Limit: 10})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.Type] = true
	}
	for _, typ := range []string{"exec.observed", "exec.status.changed", "exec.exited"} {
		if !seen[typ] {
			t.Fatalf("missing event %s in %#v", typ, events)
		}
	}
}

func TestResourceSamplesRespectRetention(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "harness.db"))
	for i := range 3 {
		_, err := st.RecordResourceSample(ctx, ResourceSample{
			TerminalID: "agt_1",
			SampledAt:  time.Unix(int64(i), 0).UTC(),
			Source:     "test",
			Data:       []byte(`{"index":` + string(rune('0'+i)) + `}`),
		}, 2)
		if err != nil {
			t.Fatalf("record sample %d: %v", i, err)
		}
	}
	samples, err := st.ListResourceSamples(ctx, "agt_1", 10)
	if err != nil {
		t.Fatalf("list samples: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("len(samples) = %d, want 2: %#v", len(samples), samples)
	}
	if samples[0].SampledAt.Unix() != 1 || samples[1].SampledAt.Unix() != 2 {
		t.Fatalf("samples not oldest-to-newest retained tail: %#v", samples)
	}
}

func TestExecLogChunksAreOrderedAndDeletable(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "harness.db"))
	base := time.Now().UTC()
	for i := range 3 {
		if err := st.AppendExecLogChunk(ctx, "exec_1", base.Add(time.Duration(i)*time.Minute), "zstd", []byte{byte(i)}, 10); err != nil {
			t.Fatalf("append chunk %d: %v", i, err)
		}
	}
	// A second exec's chunks must not leak into the first exec's read.
	if err := st.AppendExecLogChunk(ctx, "exec_2", base, "zstd", []byte{9}, 1); err != nil {
		t.Fatalf("append other exec chunk: %v", err)
	}
	chunks, err := st.ListExecLogChunks(ctx, "exec_1")
	if err != nil {
		t.Fatalf("list chunks: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3: %#v", len(chunks), chunks)
	}
	for i, chunk := range chunks {
		if chunk.Data[0] != byte(i) {
			t.Fatalf("chunks[%d] = %#v, want oldest-first order", i, chunk)
		}
	}
	if err := st.DeleteExecLog(ctx, "exec_1"); err != nil {
		t.Fatalf("delete log: %v", err)
	}
	chunks, err = st.ListExecLogChunks(ctx, "exec_1")
	if err != nil {
		t.Fatalf("list chunks after delete: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("len(chunks) after delete = %d, want 0", len(chunks))
	}
	remaining, err := st.ListExecLogChunks(ctx, "exec_2")
	if err != nil {
		t.Fatalf("list other exec chunks: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("deleting exec_1 must not remove exec_2's chunks: %#v", remaining)
	}
}

func TestExecLogChunksPruneBeyondRetention(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "harness.db"))
	now := time.Now().UTC()
	if err := st.AppendExecLogChunk(ctx, "exec_1", now.Add(-defaultLogRetention-time.Hour), "zstd", []byte{1}, 1); err != nil {
		t.Fatalf("append stale chunk: %v", err)
	}
	if err := st.AppendExecLogChunk(ctx, "exec_1", now, "zstd", []byte{2}, 1); err != nil {
		t.Fatalf("append fresh chunk: %v", err)
	}
	chunks, err := st.ListExecLogChunks(ctx, "exec_1")
	if err != nil {
		t.Fatalf("list chunks: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Data[0] != 2 {
		t.Fatalf("chunks = %#v, want only the fresh chunk retained", chunks)
	}
}

// Deleting an exec's record removes its identity and observed status together,
// so nothing reads it back, and leaves its events: those are history.
func TestDeleteExecRecordKeepsEvents(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "harness.db"))
	exec := execs.Exec{ID: "ex_1", Command: []string{"codex"}, Status: execs.StatusRunning, CreatedAt: time.Now().UTC()}
	if err := st.SaveExecRecord(ctx, exec); err != nil {
		t.Fatalf("save record: %v", err)
	}
	if err := st.ObserveExec(ctx, exec); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if err := st.DeleteExecRecord(ctx, "ex_1"); err != nil {
		t.Fatalf("delete record: %v", err)
	}
	records, err := st.LoadExecRecords(ctx)
	if err != nil {
		t.Fatalf("load records: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %#v, want none", records)
	}
	var states int64
	if err := st.read.Model(&ExecState{}).Where("exec_id = ?", "ex_1").Count(&states).Error; err != nil {
		t.Fatalf("count states: %v", err)
	}
	if states != 0 {
		t.Fatalf("observed status survived the delete")
	}
	events, err := st.ListEvents(ctx, ExecEventFilter{ExecID: "ex_1", Limit: 10})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("deleting the record removed the exec's events")
	}
}

// A store written before DeleteExecRecord existed still holds the records of
// execs it deleted. Opening it purges those, and nothing that might be live: a
// purge cannot be undone, so either sign of an id created again keeps it.
func TestOpenPurgesRecordsOfDeletedExecs(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "harness.db")
	st, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Every step waits for the clock to pass the one before: the purge orders
	// events by time, and Windows' clock can date two quick steps the same.
	var last time.Time
	tick := func() {
		for !time.Now().UTC().After(last) {
			time.Sleep(100 * time.Microsecond)
		}
	}
	done := func() { last = time.Now().UTC() }
	event := func(id, typ string) {
		tick()
		if err := st.RecordExecEvent(ctx, id, typ, typ, nil); err != nil {
			t.Fatalf("record event: %v", err)
		}
		done()
	}
	// observe upserts the status row as an exec's run would, started at
	// startedAt; a left-behind record is re-observed with its old run's start.
	observe := func(id string, startedAt time.Time, status execs.Status) {
		tick()
		if err := st.ObserveExec(ctx, execs.Exec{ID: id, Status: status, CreatedAt: startedAt, StartedAt: &startedAt}); err != nil {
			t.Fatalf("observe: %v", err)
		}
		done()
	}
	create := func(id string) time.Time {
		tick()
		startedAt := time.Now().UTC()
		if err := st.SaveExecRecord(ctx, execs.Exec{ID: id, Command: []string{"codex"}, CreatedAt: startedAt}); err != nil {
			t.Fatalf("save record: %v", err)
		}
		event(id, "exec.created")
		observe(id, startedAt, execs.StatusRunning)
		return startedAt
	}

	// Deleted, then re-read from its left-behind record: neither the
	// observation events that produced nor an attach ending is a sign of life.
	ghostStarted := create("ex_deleted")
	event("ex_deleted", "exec.deleted")
	observe("ex_deleted", ghostStarted, execs.StatusLost)
	event("ex_deleted", "exec.status.changed")
	// Closed with a client attached: its attach ends after the deletion.
	event("ex_deleted", "exec.attach.closed")
	// Deleted and created again under the same id, with every event.
	create("primary")
	event("primary", "exec.deleted")
	create("primary")
	// Created again, but the exec.created write was lost: the status row,
	// upserted from the new exec, still says so.
	create("ex_created_quietly")
	event("ex_created_quietly", "exec.deleted")
	tick()
	observe("ex_created_quietly", time.Now().UTC(), execs.StatusRunning)
	// Created again, and it is the status row that never landed.
	create("ex_observed_quietly")
	event("ex_observed_quietly", "exec.deleted")
	event("ex_observed_quietly", "exec.started")
	// Never deleted.
	create("ex_live")

	// What an older agent's store looks like: the purge never ran.
	if err := st.write.Where("key = ?", deletedExecRecordsPurgedKey).Delete(&AgentState{}).Error; err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	st = openStore(ctx, t, dbPath)
	records, err := st.LoadExecRecords(ctx)
	if err != nil {
		t.Fatalf("load records: %v", err)
	}
	got := map[string]bool{}
	for _, record := range records {
		got[record.ID] = true
	}
	want := map[string]bool{"primary": true, "ex_created_quietly": true, "ex_observed_quietly": true, "ex_live": true}
	if len(got) != len(want) {
		t.Fatalf("records = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("records = %v, want %v", got, want)
		}
	}
	var states []ExecState
	if err := st.read.Find(&states).Error; err != nil {
		t.Fatalf("load states: %v", err)
	}
	for _, state := range states {
		if state.ExecID == "ex_deleted" {
			t.Fatal("the deleted exec's observed status survived the purge")
		}
	}
	if len(states) != len(want) {
		t.Fatalf("states = %d, want %d", len(states), len(want))
	}
}

// restamp spreads a table's rows one second apart in the order they were
// written, and writes each row's new time back through the pointers given, so a
// test that asserts an order does not depend on how finely the host's clock
// ticks. A trail orders by time and breaks ties by ID, and an ID is random by
// design (creation time lives in CreatedAt, not in the ID) — so rows sharing a
// timestamp have no defined order to assert.
func restamp(ctx context.Context, t *testing.T, st *Store, table string, out ...*time.Time) {
	t.Helper()
	var ids []string
	if err := st.read.WithContext(ctx).Raw("select id from " + table + " order by rowid").Scan(&ids).Error; err != nil {
		t.Fatalf("read %s order: %v", table, err)
	}
	if len(out) > 0 && len(out) != len(ids) {
		t.Fatalf("%s has %d rows, restamping %d", table, len(ids), len(out))
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range ids {
		at := base.Add(time.Duration(i) * time.Second)
		// Through the same table the store writes, so the text SQLite compares
		// is written the way the store writes it.
		if err := st.write.WithContext(ctx).Table(table).Where("id = ?", id).Update("created_at", at).Error; err != nil {
			t.Fatalf("restamp %s: %v", table, err)
		}
		if i < len(out) {
			*out[i] = at
		}
	}
}

// A terminal wait finds the first hook it named that was recorded after a
// point, behind any number it did not name, and is woken when one is recorded
// (ADR 0137 §3).
func TestFirstHarnessHookSinceAndSignal(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "harness.db"))
	record := func(terminalID, event string) HarnessHookRecord {
		t.Helper()
		signal := st.HarnessHookSignal()
		hook, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: terminalID, Provider: "claude", Event: event})
		if err != nil {
			t.Fatalf("record hook: %v", err)
		}
		select {
		case <-signal:
		default:
			t.Fatalf("recording %s did not signal", event)
		}
		return hook
	}
	first := record("exec_1", "UserPromptSubmit")
	record("exec_2", "Stop")
	// More hooks it did not name than any page would hold.
	for range 600 {
		record("exec_1", "PreToolUse")
	}
	stop := record("exec_1", "Stop")
	record("exec_1", "Stop")

	got, err := st.FirstHarnessHookSince(ctx, "exec_1", first.CreatedAt, []string{"Stop", "Notification"})
	if err != nil {
		t.Fatalf("first hook since: %v", err)
	}
	if got == nil || got.ID != stop.ID {
		t.Fatalf("hook = %+v, want this terminal's first Stop after the prompt, behind the tool calls", got)
	}
	if none, err := st.FirstHarnessHookSince(ctx, "exec_1", first.CreatedAt, []string{"SessionEnd"}); err != nil || none != nil {
		t.Fatalf("hook = %+v, %v; want none for an event never recorded", none, err)
	}
}

// Resume points and hook stamps share one clock that never repeats, so a hook
// recorded in the same wall-clock tick as the point, or as the hook before it,
// still comes after it. Windows' clock ticks coarsely enough for both.
func TestHarnessHookClockNeverRepeats(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "clock.db"))
	since := st.HarnessHookResumePoint()
	var last time.Time
	for i := range 50 {
		hook, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: "exec_1", Provider: "claude", Event: "Stop"})
		if err != nil {
			t.Fatalf("record hook: %v", err)
		}
		got, err := st.FirstHarnessHookSince(ctx, "exec_1", since, []string{"Stop"})
		if err != nil {
			t.Fatalf("first hook since: %v", err)
		}
		if got == nil || got.ID != hook.ID {
			t.Fatalf("hook %d after the last resume point = %+v, want %s", i, got, hook.ID)
		}
		if !hook.CreatedAt.After(last) {
			t.Fatalf("hook %d stamped %v, not after %v", i, hook.CreatedAt, last)
		}
		last, since = hook.CreatedAt, hook.CreatedAt
	}
	if point := st.HarnessHookResumePoint(); !point.After(last) {
		t.Fatalf("resume point %v, not after the last hook %v", point, last)
	}
}

func TestListHarnessHooksFiltersAndReadsForward(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "hooks.db"))
	record := func(provider, event string) HarnessHookRecord {
		t.Helper()
		rec, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: "agt_1", Provider: provider, Event: event, Payload: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatalf("record hook: %v", err)
		}
		return rec
	}
	first := record("claude-code", "SessionStart")
	second := record("claude-code", "PreToolUse")
	third := record("codex-cli", "PreToolUse")
	fourth := record("claude-code", "Stop")
	// The store stamps each record with the wall clock, and four written in a
	// row are only distinguishable if the clock advanced between them: Windows
	// ticks about every 15ms and stamps all four the same. Order among equal
	// timestamps is not a property this trail has, so give them times a second
	// apart and test the ordering the trail does promise.
	restamp(ctx, t, st, "harness_hook_logs", &first.CreatedAt, &second.CreatedAt, &third.CreatedAt, &fourth.CreatedAt)

	events := func(hooks []HarnessHookRecord) []string {
		out := make([]string, 0, len(hooks))
		for _, h := range hooks {
			out = append(out, h.Provider+"/"+h.Event)
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		filter HarnessHookFilter
		want   []string
	}{
		{name: "provider", filter: HarnessHookFilter{Provider: "codex-cli"}, want: []string{"codex-cli/PreToolUse"}},
		{name: "event", filter: HarnessHookFilter{Event: "PreToolUse"}, want: []string{"claude-code/PreToolUse", "codex-cli/PreToolUse"}},
		// The most recent N, still oldest first.
		{name: "latest two", filter: HarnessHookFilter{Limit: 2}, want: []string{"codex-cli/PreToolUse", "claude-code/Stop"}},
		// A follower reads forward from a bound: the earliest N at or after it.
		{name: "forward from the second", filter: HarnessHookFilter{Since: first.CreatedAt.Add(time.Nanosecond), Ascending: true, Limit: 2}, want: []string{"claude-code/PreToolUse", "codex-cli/PreToolUse"}},
		// Inclusive, and in a zone far from UTC: rows are recorded in UTC.
		{name: "since is inclusive in any zone", filter: HarnessHookFilter{Since: third.CreatedAt.In(time.FixedZone("UTC+14", 14*60*60)), Ascending: true}, want: []string{"codex-cli/PreToolUse", "claude-code/Stop"}},
		// Paging back: the latest N at or before a bound, inclusive.
		{name: "back from the third", filter: HarnessHookFilter{Until: third.CreatedAt.In(time.FixedZone("UTC+14", 14*60*60)), Limit: 2}, want: []string{"claude-code/PreToolUse", "codex-cli/PreToolUse"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks, err := st.ListHarnessHooks(ctx, tc.filter)
			if err != nil {
				t.Fatalf("list hooks: %v", err)
			}
			if got := events(hooks); !slices.Equal(got, tc.want) {
				t.Fatalf("hooks = %v, want %v", got, tc.want)
			}
		})
	}
}

// Exec events across every exec, the way an audit read asks for them.
func TestListEventsAcrossExecsFiltersAndReadsForward(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "events.db"))
	for _, e := range []struct{ exec, typ string }{
		{"ex_1", "exec.created"}, {"ex_1", "exec.started"}, {"ex_2", "exec.created"}, {"ex_2", "exec.stopped"},
	} {
		if err := st.RecordExecEvent(ctx, e.exec, e.typ, e.typ, nil); err != nil {
			t.Fatalf("record event: %v", err)
		}
	}
	restamp(ctx, t, st, "exec_events")
	names := func(events []Event) []string {
		out := make([]string, 0, len(events))
		for _, e := range events {
			out = append(out, e.TerminalID+"/"+e.Type)
		}
		return out
	}
	all, err := st.ListEvents(ctx, ExecEventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(all); !slices.Equal(got, []string{"ex_2/exec.stopped", "ex_2/exec.created", "ex_1/exec.started", "ex_1/exec.created"}) {
		t.Fatalf("every exec, newest first = %v", got)
	}
	created, err := st.ListEvents(ctx, ExecEventFilter{Type: "exec.created", Ascending: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(created); !slices.Equal(got, []string{"ex_1/exec.created", "ex_2/exec.created"}) {
		t.Fatalf("created, oldest first = %v", got)
	}
	forward, err := st.ListEvents(ctx, ExecEventFilter{Since: all[1].CreatedAt, Ascending: true, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(forward); !slices.Equal(got, []string{"ex_2/exec.created", "ex_2/exec.stopped"}) {
		t.Fatalf("forward from ex_2's create = %v", got)
	}
	back, err := st.ListEvents(ctx, ExecEventFilter{Until: all[1].CreatedAt, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(back); !slices.Equal(got, []string{"ex_2/exec.created", "ex_1/exec.started"}) {
		t.Fatalf("back from ex_2's create = %v", got)
	}
}

// A hook is recorded under the name its harness used and, when Claude Code has
// one for the same thing, under that too (ADR 0146). The harness's own name is
// never rewritten, and an event Claude Code has no word for gets no canonical
// name rather than a copy of its own.
func TestRecordHarnessHookResolvesTheCanonicalName(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "canonical.db"))
	for _, tc := range []struct {
		provider, event, wantCanonical string
	}{
		{"claude-code", "Stop", "Stop"},
		{"claude-code", "PreModelSwitch", "PreModelSwitch"},
		{"codex-cli", "Stop", "Stop"},
		{"codex-cli", "Interrupt", ""},
		{"a-third-party-harness", "Frobnicate", ""},
	} {
		rec, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: "agt_1", Provider: tc.provider, Event: tc.event})
		if err != nil {
			t.Fatalf("record %s/%s: %v", tc.provider, tc.event, err)
		}
		if rec.Event != tc.event {
			t.Errorf("%s/%s event = %q, want it recorded unchanged", tc.provider, tc.event, rec.Event)
		}
		if rec.CanonicalEvent != tc.wantCanonical {
			t.Errorf("%s/%s canonical = %q, want %q", tc.provider, tc.event, rec.CanonicalEvent, tc.wantCanonical)
		}
		read, err := st.ListHarnessHooks(ctx, HarnessHookFilter{ID: rec.ID})
		if err != nil || len(read) != 1 {
			t.Fatalf("read back %s: %+v, %v", rec.ID, read, err)
		}
		if read[0].CanonicalEvent != tc.wantCanonical {
			t.Errorf("%s/%s stored canonical = %q, want %q", tc.provider, tc.event, read[0].CanonicalEvent, tc.wantCanonical)
		}
	}
}

// A wait and a filter each take either name (ADR 0146 §6), so a caller need
// not know which harness the terminal runs to name the event that ends a turn.
func TestHarnessHookQueriesMatchEitherName(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "eithername.db"))
	since := st.HarnessHookResumePoint()
	interrupt, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: "exec_1", Provider: "codex-cli", Event: "Interrupt"})
	if err != nil {
		t.Fatalf("record interrupt: %v", err)
	}
	stop, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: "exec_1", Provider: "codex-cli", Event: "Stop"})
	if err != nil {
		t.Fatalf("record stop: %v", err)
	}
	// Named canonically, and named as the harness emits it: the same hook.
	for _, name := range []string{"Stop"} {
		got, err := st.FirstHarnessHookSince(ctx, "exec_1", since, []string{name})
		if err != nil || got == nil || got.ID != stop.ID {
			t.Fatalf("wait for %s = %+v, %v; want %s", name, got, err, stop.ID)
		}
	}
	// An event with no canonical name is still waitable under its own.
	got, err := st.FirstHarnessHookSince(ctx, "exec_1", since, []string{"Interrupt"})
	if err != nil || got == nil || got.ID != interrupt.ID {
		t.Fatalf("wait for Interrupt = %+v, %v; want %s", got, err, interrupt.ID)
	}
	for _, tc := range []struct {
		name    string
		wantIDs []string
	}{
		{"Stop", []string{stop.ID}},
		{"Interrupt", []string{interrupt.ID}},
	} {
		hooks, err := st.ListHarnessHooks(ctx, HarnessHookFilter{Event: tc.name})
		if err != nil {
			t.Fatalf("list %s: %v", tc.name, err)
		}
		if len(hooks) != len(tc.wantIDs) || hooks[0].ID != tc.wantIDs[0] {
			t.Fatalf("list %s = %+v, want %v", tc.name, hooks, tc.wantIDs)
		}
	}
}

// Hooks recorded before the column existed are filled on the next open, so a
// query by canonical name finds them too (ADR 0146 §7). Nothing is rewritten:
// the harness's own name is left exactly as it was recorded.
func TestOpenBackfillsCanonicalHookEvents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backfill.db")
	st := openStore(ctx, t, path)
	mapped, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: "agt_1", Provider: "codex-cli", Event: "Stop"})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	unmapped, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: "agt_1", Provider: "codex-cli", Event: "Interrupt"})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	// Back to how a store looked before any of this existed: no canonical
	// names, and no stamp saying the mapping was ever applied.
	if err := st.write.WithContext(ctx).Model(&HarnessHookLog{}).
		Where("1 = 1").Update("canonical_event", "").Error; err != nil {
		t.Fatalf("clear canonical: %v", err)
	}
	if err := st.write.WithContext(ctx).
		Where("key = ?", canonicalHookEventsBackfilledKey).
		Delete(&AgentState{}).Error; err != nil {
		t.Fatalf("clear backfill stamp: %v", err)
	}
	cleared, err := st.ListHarnessHooks(ctx, HarnessHookFilter{TerminalID: "agt_1"})
	if err != nil {
		t.Fatalf("list cleared: %v", err)
	}
	for _, hook := range cleared {
		if hook.CanonicalEvent != "" {
			t.Fatalf("hook %s still canonical %q; the test never exercises the backfill", hook.ID, hook.CanonicalEvent)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openStore(ctx, t, path)
	hooks, err := reopened.ListHarnessHooks(ctx, HarnessHookFilter{TerminalID: "agt_1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[string]HarnessHookRecord{}
	for _, hook := range hooks {
		byID[hook.ID] = hook
	}
	if got := byID[mapped.ID]; got.CanonicalEvent != "Stop" || got.Event != "Stop" {
		t.Errorf("mapped hook = %+v, want event and canonical both Stop", got)
	}
	if got := byID[unmapped.ID]; got.CanonicalEvent != "" || got.Event != "Interrupt" {
		t.Errorf("unmapped hook = %+v, want event Interrupt and no canonical name", got)
	}
	// And the backfill made it findable by its canonical name.
	found, err := reopened.ListHarnessHooks(ctx, HarnessHookFilter{Event: "Stop"})
	if err != nil || len(found) != 1 || found[0].ID != mapped.ID {
		t.Fatalf("list Stop after backfill = %+v, %v; want %s", found, err, mapped.ID)
	}
}

// A blank event name matches nothing. It used to be harmless — `event` is
// always populated, so `event IN (”)` found nothing — but a canonical name is
// empty for every event Claude Code has no word for, so an unfiltered blank
// would end a wait on the next Interrupt. The API sets no minLength on a
// wait's names and pflag reads `--hook Stop,` as two, so this arrives from a
// typo rather than from malice.
func TestFirstHarnessHookSinceIgnoresBlankEventNames(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "blank.db"))
	since := st.HarnessHookResumePoint()
	if _, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: "exec_1", Provider: "codex-cli", Event: "Interrupt"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	for _, events := range [][]string{{""}, {"  "}, {"", "  "}} {
		got, err := st.FirstHarnessHookSince(ctx, "exec_1", since, events)
		if err != nil {
			t.Fatalf("wait for %q: %v", events, err)
		}
		if got != nil {
			t.Errorf("wait for %q found %s/%s; a blank name must match nothing", events, got.Provider, got.Event)
		}
	}
	// A blank beside a real name leaves the real one working.
	stop, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: "exec_1", Provider: "codex-cli", Event: "Stop"})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := st.FirstHarnessHookSince(ctx, "exec_1", since, []string{"Stop", ""})
	if err != nil || got == nil || got.ID != stop.ID {
		t.Fatalf("wait for [Stop, \"\"] = %+v, %v; want %s", got, err, stop.ID)
	}
}

// An event filter is ANDed with the filters beside it, never ORed across them.
// The clause holds two columns joined by OR, and GORM only parenthesizes such
// an expression when it is combined with another — so a filter that reached
// the query unwrapped would return every terminal's and every provider's hooks
// of that name.
func TestListHarnessHooksEventFilterDoesNotEscapeItsOtherFilters(t *testing.T) {
	ctx := context.Background()
	st := openStore(ctx, t, filepath.Join(t.TempDir(), "scoped.db"))
	record := func(terminalID, provider, event string) HarnessHookRecord {
		t.Helper()
		rec, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: terminalID, Provider: provider, Event: event})
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		return rec
	}
	mine := record("exec_1", "codex-cli", "Stop")
	record("exec_2", "codex-cli", "Stop")
	record("exec_1", "claude-code", "Stop")

	for _, tc := range []struct {
		name   string
		filter HarnessHookFilter
		wantID string
	}{
		{"provider narrows it", HarnessHookFilter{Event: "Stop", Provider: "codex-cli", TerminalID: "exec_1"}, mine.ID},
		{"terminal narrows it", HarnessHookFilter{Event: "Stop", TerminalID: "exec_1", Provider: "codex-cli"}, mine.ID},
	} {
		hooks, err := st.ListHarnessHooks(ctx, tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(hooks) != 1 || hooks[0].ID != tc.wantID {
			t.Errorf("%s: got %d hooks %+v, want only %s", tc.name, len(hooks), hooks, tc.wantID)
		}
	}
}

// The backfill is stamped with the mapping's fingerprint, so an unchanged
// table is not re-scanned on every agent start — the rows it would scan are
// the ones with no canonical name, and those are never removed.
func TestBackfillIsSkippedWhenTheMappingHasNotChanged(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stamp.db")
	st := openStore(ctx, t, path)
	if _, err := st.RecordHarnessHook(ctx, HarnessHookRecord{TerminalID: "agt_1", Provider: "codex-cli", Event: "Stop"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	var stamp AgentState
	if err := st.write.WithContext(ctx).Where("key = ?", canonicalHookEventsBackfilledKey).Take(&stamp).Error; err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	if stamp.Value != harness.CanonicalHookEventsVersion() {
		t.Fatalf("stamp = %q, want the mapping's fingerprint %q", stamp.Value, harness.CanonicalHookEventsVersion())
	}
	// Clearing a row's canonical name without clearing the stamp leaves it
	// alone: the store has already applied this table, and says so.
	if err := st.write.WithContext(ctx).Model(&HarnessHookLog{}).
		Where("1 = 1").Update("canonical_event", "").Error; err != nil {
		t.Fatalf("clear canonical: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened := openStore(ctx, t, path)
	hooks, err := reopened.ListHarnessHooks(ctx, HarnessHookFilter{TerminalID: "agt_1"})
	if err != nil || len(hooks) != 1 {
		t.Fatalf("list = %+v, %v", hooks, err)
	}
	if hooks[0].CanonicalEvent != "" {
		t.Errorf("canonical = %q; an unchanged mapping must not re-scan", hooks[0].CanonicalEvent)
	}
}
