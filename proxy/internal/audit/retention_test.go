package audit

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openSweptRecorder returns a recorder whose spool directories are laid out the
// way the pool proxy lays them out, with the writer already drained.
func openSweptRecorder(t *testing.T) (*Recorder, string) {
	t.Helper()
	dir := t.TempDir()
	recorder, err := Open(context.Background(), filepath.Join(dir, "audit.db"), 8, true)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	recorder.ConfigureStreamSpool(filepath.Join(dir, "streams"), 8)
	recorder.ConfigureBodySpool(filepath.Join(dir, "bodies"))
	t.Cleanup(func() { _ = recorder.Close() })
	return recorder, dir
}

func writeSpool(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatalf("write spool: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestSweepDropsRowsAndSpoolsPastTheWindow(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	now := time.Now().UTC()

	recorder.RecordHTTP(HTTPEvent{Time: now.Add(-72 * time.Hour), ClientID: "sbx-old", Method: http.MethodGet, Host: "old.example.com"})
	recorder.RecordHTTP(HTTPEvent{Time: now.Add(-time.Hour), ClientID: "sbx-new", Method: http.MethodGet, Host: "new.example.com"})
	recorder.RecordSOCKS(SOCKSEvent{Time: now.Add(-72 * time.Hour), ClientID: "sbx-old", Destination: "old.example.com"})
	recorder.RecordSOCKS(SOCKSEvent{Time: now.Add(-time.Hour), ClientID: "sbx-new", Destination: "new.example.com"})
	drainRecorder(t, recorder)

	oldBody := filepath.Join(dir, "bodies", "bodies", "sbx-old", "response-1.bin")
	newBody := filepath.Join(dir, "bodies", "bodies", "sbx-new", "response-2.bin")
	oldStream := filepath.Join(dir, "streams", "streams", "sbx-old", "stream-1.bin")
	writeSpool(t, oldBody, now.Add(-72*time.Hour))
	writeSpool(t, newBody, now.Add(-time.Hour))
	writeSpool(t, oldStream, now.Add(-72*time.Hour))

	result, err := recorder.Sweep(context.Background(), now.Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if result.HTTPRows != 1 || result.SOCKSRows != 1 {
		t.Fatalf("Sweep() rows = http %d socks %d, want 1 and 1", result.HTTPRows, result.SOCKSRows)
	}
	if result.Files != 2 {
		t.Fatalf("Sweep() files = %d, want the two spools past the window", result.Files)
	}
	for _, path := range []string{oldBody, oldStream} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived the sweep", path)
		}
	}
	if _, err := os.Stat(newBody); err != nil {
		t.Fatalf("spool inside the window was reclaimed: %v", err)
	}

	var httpRows, socksRows int64
	recorder.db.Model(&HTTPExchange{}).Count(&httpRows)
	recorder.db.Model(&SOCKSConnect{}).Count(&socksRows)
	if httpRows != 1 || socksRows != 1 {
		t.Fatalf("rows after sweep = http %d socks %d, want the recent one of each", httpRows, socksRows)
	}
}

// A spool whose audit event was dropped — the recorder drops rather than blocks
// when its queue is full — is named by no row at all. Sweeping only what rows
// name would leak it forever, which is the case that made these trees unbounded.
func TestSweepReclaimsSpoolsNoRowNames(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	now := time.Now().UTC()
	orphan := filepath.Join(dir, "bodies", "bodies", "sbx-dropped", "request-9.bin")
	writeSpool(t, orphan, now.Add(-72*time.Hour))

	if _, err := recorder.Sweep(context.Background(), now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("a spool no row names survived the sweep")
	}
	// The per-sandbox directory goes with it: they are named by sandbox ID, so
	// leaving them accumulates one empty directory per sandbox ever hosted.
	if _, err := os.Stat(filepath.Dir(orphan)); !os.IsNotExist(err) {
		t.Fatal("the emptied per-sandbox directory survived the sweep")
	}
}

// An upgraded stream can sit open and idle for longer than the window, and it
// has no row until it closes. Reclaiming it would delete a live capture.
func TestSweepSkipsASpoolStillBeingWritten(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	now := time.Now().UTC()
	live := filepath.Join(dir, "streams", "streams", "sbx-live", "stream-live.bin")
	writeSpool(t, live, now.Add(-72*time.Hour))

	release := recorder.trackSpool("streams/sbx-live/stream-live.bin")
	if _, err := recorder.Sweep(context.Background(), now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("open spool was reclaimed out from under its stream: %v", err)
	}

	release()
	if _, err := recorder.Sweep(context.Background(), now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatal("spool survived the sweep after its stream closed")
	}
}

func TestBeginSpoolHoldsTheFileOpenUntilClose(t *testing.T) {
	recorder, _ := openSweptRecorder(t)
	record, spool, err := recorder.BeginBody("sbx-1", BodyKindResponse)
	if err != nil {
		t.Fatalf("BeginBody() error = %v", err)
	}
	if !recorder.spoolIsOpen(record.File) {
		t.Fatal("body spool is not held open while it is being written")
	}
	if err := spool.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if recorder.spoolIsOpen(record.File) {
		t.Fatal("body spool is still held open after Close")
	}
}

func TestSweepIsANoOpForARecorderThatRecordsNothing(t *testing.T) {
	var recorder *Recorder
	if _, err := recorder.Sweep(context.Background(), time.Now()); err != nil {
		t.Fatalf("Sweep() on a nil recorder error = %v", err)
	}
	disabled := &Recorder{}
	result, err := disabled.Sweep(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Sweep() on a disabled recorder error = %v", err)
	}
	if !result.Empty() {
		t.Fatalf("Sweep() on a disabled recorder reclaimed %+v", result)
	}
}

// drainRecorder flushes the asynchronous writer so the rows are on disk.
func drainRecorder(t *testing.T, recorder *Recorder) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(recorder.ch) == 0 {
			// The last event may still be mid-write; a short settle is enough
			// because the writer holds no locks between rows.
			time.Sleep(20 * time.Millisecond)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the audit writer to drain")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
