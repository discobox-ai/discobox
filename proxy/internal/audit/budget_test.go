package audit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeSizedSpool(t *testing.T, path string, size int, modTime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
		t.Fatalf("write spool: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

func TestSpoolBudgetIsTheLesserTerm(t *testing.T) {
	const total = 1000 << 30
	for _, tc := range []struct {
		name   string
		budget SpoolBudget
		want   int64
	}{
		{"percent under ceiling", SpoolBudget{MaxBytes: 100 << 30, MaxPercent: 5}, 50 << 30},
		{"ceiling under percent", SpoolBudget{MaxBytes: 10 << 30, MaxPercent: 5}, 10 << 30},
		{"ceiling alone", SpoolBudget{MaxBytes: 10 << 30}, 10 << 30},
		{"percent alone", SpoolBudget{MaxPercent: 5}, 50 << 30},
		{"neither", SpoolBudget{}, 0},
	} {
		if got := tc.budget.limit(total); got != tc.want {
			t.Errorf("%s: limit = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := (SpoolBudget{MaxBytes: 10 << 30, MaxPercent: 5}).limit(0); got != 10<<30 {
		t.Errorf("unknown filesystem size: limit = %d, want the ceiling alone", got)
	}
}

// One large file never costs a small one its existence: the large file's tail
// goes, its head stays with a marker saying what it was, and its modification
// time is unchanged so the age sweep still pairs it with its row.
func TestBudgetTruncatesTheLargestFileBeforeDeletingAny(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	recorder.ConfigureSpoolBudget(SpoolBudget{MaxBytes: 10_000, HeadBytes: 100})
	now := time.Now().UTC().Truncate(time.Second)

	large := filepath.Join(dir, "bodies", "bodies", "sbx-1", "response-large.bin")
	writeSizedSpool(t, large, 20_000, now.Add(-time.Minute))
	var small []string
	for i := range 10 {
		path := filepath.Join(dir, "bodies", "bodies", "sbx-1", "request-"+string(rune('a'+i))+".bin")
		writeSizedSpool(t, path, 50, now.Add(-time.Hour))
		small = append(small, path)
	}

	result, err := recorder.EnforceBudget(context.Background())
	if err != nil {
		t.Fatalf("EnforceBudget() error = %v", err)
	}
	if result.Truncated != 1 || result.Deleted != 0 {
		t.Fatalf("result = %+v, want one truncation and no deletion", result)
	}
	if got := fileSize(t, large); got != 100 {
		t.Fatalf("large file size = %d, want its 100-byte head", got)
	}
	marker, err := os.ReadFile(large + truncatedSuffix)
	if err != nil || string(marker) != "20000" {
		t.Fatalf("marker = %q, %v; want the original size", marker, err)
	}
	if info, _ := os.Stat(large); !info.ModTime().Equal(now.Add(-time.Minute)) {
		t.Fatalf("large file mtime = %s, want it kept at %s", info.ModTime(), now.Add(-time.Minute))
	}
	for _, path := range small {
		if got := fileSize(t, path); got != 50 {
			t.Fatalf("small file %s size = %d, want it untouched", path, got)
		}
	}

	opened, err := recorder.OpenBody(HTTPExchange{ResponseBodyFile: "bodies/sbx-1/response-large.bin"}, BodyKindResponse)
	if err != nil {
		t.Fatalf("OpenBody() error = %v", err)
	}
	_ = opened.Close()
	if opened.TruncatedFrom != 20_000 {
		t.Fatalf("OpenBody().TruncatedFrom = %d, want 20000", opened.TruncatedFrom)
	}
}

// With no tail left to cut, whole files go oldest first, each taking its
// marker with it, down to the low watermark.
func TestBudgetDeletesOldestFirstOnceNoTailsRemain(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	recorder.ConfigureSpoolBudget(SpoolBudget{MaxBytes: 500, HeadBytes: 100})
	now := time.Now().UTC()

	var paths []string
	for i := range 10 {
		path := filepath.Join(dir, "bodies", "bodies", "sbx-1", "response-"+string(rune('a'+i))+".bin")
		writeSizedSpool(t, path, 100, now.Add(-time.Duration(10-i)*time.Minute))
		paths = append(paths, path)
	}
	// The oldest was truncated by an earlier pass.
	if err := os.WriteFile(paths[0]+truncatedSuffix, []byte("9999"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := recorder.EnforceBudget(context.Background())
	if err != nil {
		t.Fatalf("EnforceBudget() error = %v", err)
	}
	// 1004 bytes against a 450-byte watermark: the six oldest go.
	if result.Deleted != 6 || result.Truncated != 0 || result.Remaining > 450 {
		t.Fatalf("result = %+v, want six deletions down to the watermark", result)
	}
	for i, path := range paths {
		_, err := os.Stat(path)
		if gone := errors.Is(err, os.ErrNotExist); gone != (i < 6) {
			t.Fatalf("file %d gone = %v, want the oldest six gone", i, gone)
		}
	}
	if _, err := os.Stat(paths[0] + truncatedSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker of a deleted file survived: %v", err)
	}
	if _, err := recorder.OpenBody(HTTPExchange{RequestBodyFile: "bodies/sbx-1/response-a.bin"}, BodyKindRequest); !errors.Is(err, ErrSpoolReclaimed) {
		t.Fatalf("OpenBody() of a deleted file = %v, want ErrSpoolReclaimed", err)
	}
}

// An upload still being written cannot be cut yet, and its tail is no reason to
// delete what is closed: it counts as its head until it closes, and the first
// pass after that cuts it.
func TestBudgetDoesNotDeleteToMakeRoomForAnOpenSpool(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	recorder.ConfigureSpoolBudget(SpoolBudget{MaxBytes: 1000, HeadBytes: 100})
	now := time.Now().UTC()
	var small []string
	for i := range 5 {
		path := filepath.Join(dir, "bodies", "bodies", "sbx-1", "request-"+string(rune('a'+i))+".bin")
		writeSizedSpool(t, path, 50, now.Add(-time.Hour))
		small = append(small, path)
	}
	_, spool, err := recorder.BeginBody("sbx-1", BodyKindRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Write(bytes.Repeat([]byte("x"), 50_000)); err != nil {
		t.Fatal(err)
	}
	result, err := recorder.EnforceBudget(context.Background())
	if err != nil {
		t.Fatalf("EnforceBudget() error = %v", err)
	}
	if result.Deleted != 0 || result.Truncated != 0 || result.Remaining != 50_250 {
		t.Fatalf("result = %+v, want nothing reclaimed while the large spool is open", result)
	}
	for _, path := range small {
		if got := fileSize(t, path); got != 50 {
			t.Fatalf("%s size = %d, want it kept", path, got)
		}
	}
	_ = spool.Close()
	result, err = recorder.EnforceBudget(context.Background())
	if err != nil || result.Truncated != 1 || result.Deleted != 0 || result.Remaining != 350+int64(len("50000")) {
		t.Fatalf("after close: %+v, %v; want the closed upload cut to its head and nothing deleted", result, err)
	}
}

func TestBudgetLeavesOpenSpoolsAlone(t *testing.T) {
	recorder, _ := openSweptRecorder(t)
	recorder.ConfigureSpoolBudget(SpoolBudget{MaxBytes: 1000, HeadBytes: 100})

	record, spool, err := recorder.BeginBody("sbx-1", BodyKindResponse)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Write(bytes.Repeat([]byte("x"), 5000)); err != nil {
		t.Fatal(err)
	}
	result, err := recorder.EnforceBudget(context.Background())
	if err != nil {
		t.Fatalf("EnforceBudget() error = %v", err)
	}
	if result.Bytes != 5000 || result.Truncated != 0 || result.Deleted != 0 {
		t.Fatalf("result = %+v, want the open spool counted and untouched", result)
	}
	_ = spool.Close()
	result, err = recorder.EnforceBudget(context.Background())
	if err != nil || result.Truncated != 1 {
		t.Fatalf("EnforceBudget() after close = %+v, %v; want the closed spool truncated", result, err)
	}
	opened, err := recorder.OpenBody(HTTPExchange{ResponseBodyFile: record.File}, BodyKindResponse)
	if err != nil {
		t.Fatal(err)
	}
	_ = opened.Close()
	if opened.TruncatedFrom != 5000 {
		t.Fatalf("TruncatedFrom = %d, want 5000", opened.TruncatedFrom)
	}
}

// A stream is cut where a frame ends, so what is left still parses.
func TestBudgetCutsAStreamOnAFrameBoundary(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	recorder.ConfigureSpoolBudget(SpoolBudget{MaxBytes: 1000, HeadBytes: 500})
	// Deep enough that no chunk below is dropped.
	recorder.ConfigureStreamSpool(filepath.Join(dir, "streams"), 64)

	record, session, err := recorder.BeginUpgradeStream("sbx-1", "websocket")
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("p"), 100)
	for range 30 {
		session.RecordChunk(StreamClientToServer, payload)
	}
	_ = session.Close()
	path := filepath.Join(dir, "streams", filepath.FromSlash(record.File))
	original := fileSize(t, path)

	if _, err := recorder.EnforceBudget(context.Background()); err != nil {
		t.Fatalf("EnforceBudget() error = %v", err)
	}
	header := int64(len(upgradeStreamFileMagic) + 1 + 8 + 2 + len(record.SessionID) + 2 + len("websocket"))
	frame := int64(streamDataFrameOverhead + len(payload))
	want := header + (500-header)/frame*frame
	if got := fileSize(t, path); got != want {
		t.Fatalf("stream size = %d, want %d: the header and the whole frames that fit in 500 bytes", got, want)
	}
	opened, err := recorder.OpenStream(HTTPExchange{StreamFile: record.File})
	if err != nil {
		t.Fatal(err)
	}
	_ = opened.Close()
	if opened.TruncatedFrom != original {
		t.Fatalf("TruncatedFrom = %d, want %d", opened.TruncatedFrom, original)
	}
}

// Writes past the budget the last pass resolved wake the next one.
func TestSpoolWritesPastTheBudgetSignal(t *testing.T) {
	recorder, _ := openSweptRecorder(t)
	recorder.ConfigureSpoolBudget(SpoolBudget{MaxBytes: 1000, HeadBytes: 100})
	if _, err := recorder.EnforceBudget(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, spool, err := recorder.BeginBody("sbx-1", BodyKindRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if _, err := spool.Write([]byte(strings.Repeat("x", 900))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recorder.OverBudget():
		t.Fatal("signaled under the budget")
	default:
	}
	if _, err := spool.Write([]byte(strings.Repeat("x", 200))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recorder.OverBudget():
	default:
		t.Fatal("no signal once writes passed the budget")
	}
}

// A body cut short with its marker lost still reads as truncated: the row
// recorded every byte the spool took (ADR 26-10-08-698 §6).
func TestOpenBodyComparesTheFileWithTheRecordedBytes(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	writeSizedSpool(t, filepath.Join(dir, "bodies", "bodies", "sbx-1", "request-a.bin"), 100, time.Now())
	row := HTTPExchange{RequestBodyFile: "bodies/sbx-1/request-a.bin", RequestBodyBytes: 5000}
	opened, err := recorder.OpenBody(row, BodyKindRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = opened.Close()
	if opened.TruncatedFrom != 5000 {
		t.Fatalf("TruncatedFrom = %d, want the recorded 5000", opened.TruncatedFrom)
	}
	row.RequestBodyBytes = 100
	if opened, err = recorder.OpenBody(row, BodyKindRequest); err != nil {
		t.Fatal(err)
	}
	_ = opened.Close()
	if opened.TruncatedFrom != 0 {
		t.Fatalf("TruncatedFrom = %d for a whole body", opened.TruncatedFrom)
	}
}

// A large file the pass cannot cut keeps its tail, so it goes before any whole
// small file does, the marker the failed cut wrote is taken back, and the
// total still matches the disk.
func TestBudgetDeletesAnUncuttableLargeFileBeforeSmallOnes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only file")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot delete a read-only file either")
	}
	recorder, dir := openSweptRecorder(t)
	recorder.ConfigureSpoolBudget(SpoolBudget{MaxBytes: 1000, HeadBytes: 100})
	now := time.Now().UTC()
	var small []string
	for i := range 5 {
		path := filepath.Join(dir, "bodies", "bodies", "sbx-1", "request-"+string(rune('a'+i))+".bin")
		writeSizedSpool(t, path, 50, now.Add(-time.Hour))
		small = append(small, path)
	}
	large := filepath.Join(dir, "bodies", "bodies", "sbx-1", "response-large.bin")
	writeSizedSpool(t, large, 5000, now)
	if err := os.Chmod(large, 0o400); err != nil {
		t.Fatal(err)
	}

	result, err := recorder.EnforceBudget(context.Background())
	if err == nil {
		t.Fatal("EnforceBudget() reported no error for a file it could not cut")
	}
	if result.Truncated != 0 || result.Deleted != 1 || result.Remaining != 250 {
		t.Fatalf("result = %+v, want the uncuttable file deleted and nothing else", result)
	}
	if _, err := os.Stat(large); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("large file survived: %v", err)
	}
	if _, err := os.Stat(large + truncatedSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the failed cut left its marker: %v", err)
	}
	for _, path := range small {
		if got := fileSize(t, path); got != 50 {
			t.Fatalf("%s size = %d, want it kept", path, got)
		}
	}
}
