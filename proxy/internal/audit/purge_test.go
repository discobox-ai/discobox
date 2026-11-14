package audit

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A purge takes everything one client's audit holds, rows and spools, and
// nothing of any other client's (ADR 26-11-14-896 §5). The rows still queued
// when it is asked for are written before it runs, so none lands after it.
func TestPurgeClientRemovesOnlyThatClientsRowsAndSpools(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	now := time.Now().UTC()

	for _, client := range []string{"sbx_gone", "sbx_kept"} {
		recorder.RecordHTTP(HTTPEvent{Time: now, ClientID: client, Method: http.MethodGet, Host: "example.com"})
		recorder.RecordSOCKS(SOCKSEvent{Time: now, ClientID: client, Destination: "example.com"})
		recorder.RecordDNS(DNSEvent{Time: now, ClientID: client, Name: "example.com"})
	}
	goneBody := filepath.Join(dir, "bodies", "bodies", "sbx_gone", "response-1.bin")
	goneStream := filepath.Join(dir, "streams", "streams", "sbx_gone", "stream-1.bin")
	keptBody := filepath.Join(dir, "bodies", "bodies", "sbx_kept", "response-2.bin")
	for _, path := range []string{goneBody, goneStream, keptBody} {
		writeSpool(t, path, now)
	}

	// No drain first: the purge's place in the writer's queue is what
	// keeps a row queued before it from being written after it.
	result, err := recorder.PurgeClient(context.Background(), "sbx_gone")
	if err != nil {
		t.Fatalf("PurgeClient() error = %v", err)
	}
	if result.HTTPRows != 1 || result.SOCKSRows != 1 || result.DNSRows != 1 || result.Files != 2 {
		t.Errorf("PurgeClient() = %+v, want one row of each kind and two files", result)
	}
	for _, path := range []string{filepath.Dir(goneBody), filepath.Dir(goneStream)} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survived the purge (stat error %v)", path, err)
		}
	}
	if _, err := os.Stat(keptBody); err != nil {
		t.Errorf("another client's spool was removed: %v", err)
	}
	drainRecorder(t, recorder)
	for _, model := range []any{&HTTPExchange{}, &SOCKSConnect{}, &DNSQuery{}} {
		var gone, kept int64
		recorder.db.Model(model).Where("client_id = ?", "sbx_gone").Count(&gone)
		recorder.db.Model(model).Where("client_id = ?", "sbx_kept").Count(&kept)
		if gone != 0 || kept != 1 {
			t.Errorf("%T rows after the purge: purged client %d, other client %d; want 0 and 1", model, gone, kept)
		}
	}

	// A purge is repeated until it is confirmed, so one that finds nothing
	// succeeds.
	again, err := recorder.PurgeClient(context.Background(), "sbx_gone")
	if err != nil {
		t.Fatalf("a repeated PurgeClient() error = %v", err)
	}
	if again != (PurgeResult{}) {
		t.Errorf("a repeated PurgeClient() = %+v, want nothing removed", again)
	}
}

// A spool of the client's still being written is closed before its file is
// deleted, so nothing writes into it afterwards and its hold is released.
func TestPurgeClientClosesTheClientsOpenSpools(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	record, spool, err := recorder.BeginBody("sbx_gone", BodyKindRequest)
	if err != nil {
		t.Fatalf("BeginBody() error = %v", err)
	}
	if _, err := spool.Write([]byte("in flight")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, err := recorder.PurgeClient(context.Background(), "sbx_gone"); err != nil {
		t.Fatalf("PurgeClient() error = %v", err)
	}
	if recorder.spoolIsOpen(record.File) {
		t.Error("the purged client's spool is still tracked open")
	}
	if _, err := spool.Write([]byte("after")); err == nil {
		t.Error("a write into the purged client's spool succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "bodies", filepath.FromSlash(record.File))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the open spool's file survived the purge (stat error %v)", err)
	}
}

// A client ID names a directory of each spool tree, so one that is not a
// single path element is refused rather than resolved.
func TestPurgeClientRefusesAPath(t *testing.T) {
	recorder, _ := openSweptRecorder(t)
	for _, client := range []string{"", "..", "a/b", "/"} {
		if _, err := recorder.PurgeClient(context.Background(), client); err == nil {
			t.Errorf("PurgeClient(%q) succeeded", client)
		}
	}
}

// Closing a spool does not end its exchange: its row is written when the
// exchange ends, which for a purged sandbox's upgraded stream can be long
// after the purge. That row is dropped, and the spool it names with it, so
// nothing of the client's lands after its purge.
func TestPurgeClientDropsRowsOfExchangesThatEndAfterIt(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	record, spool, err := recorder.BeginBody("sbx_gone", BodyKindResponse)
	if err != nil {
		t.Fatalf("BeginBody() error = %v", err)
	}
	if _, err := recorder.PurgeClient(context.Background(), "sbx_gone"); err != nil {
		t.Fatalf("PurgeClient() error = %v", err)
	}
	// The exchange ends now, and its owner records it as it always does.
	_ = spool.Close()
	recorder.RecordHTTP(HTTPEvent{Time: time.Now(), ClientID: "sbx_gone", Method: http.MethodGet, Host: "example.com", ResponseBodyFile: record.File})
	recorder.RecordDNS(DNSEvent{Time: time.Now(), ClientID: "sbx_gone", Name: "example.com"})
	recorder.RecordHTTP(HTTPEvent{Time: time.Now(), ClientID: "sbx_kept", Method: http.MethodGet, Host: "example.com"})
	drainRecorder(t, recorder)

	var gone, kept int64
	recorder.db.Model(&HTTPExchange{}).Where("client_id = ?", "sbx_gone").Count(&gone)
	recorder.db.Model(&HTTPExchange{}).Where("client_id = ?", "sbx_kept").Count(&kept)
	if gone != 0 || kept != 1 {
		t.Errorf("rows after the purge: purged client %d, other client %d; want 0 and 1", gone, kept)
	}
	var dns int64
	recorder.db.Model(&DNSQuery{}).Where("client_id = ?", "sbx_gone").Count(&dns)
	if dns != 0 {
		t.Errorf("a lookup of the purged client's was written after its purge")
	}
	if _, err := os.Stat(filepath.Join(dir, "bodies", filepath.FromSlash(record.File))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the late row's spool survived (stat error %v)", err)
	}
}
