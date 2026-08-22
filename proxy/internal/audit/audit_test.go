package audit

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/x/gormdb"
)

func TestRecorderDropsInsteadOfBlocking(t *testing.T) {
	recorder := &Recorder{
		enabled: true,
		ch:      make(chan any),
	}
	recorder.RecordHTTP(HTTPEvent{Method: http.MethodGet})
	if recorder.Dropped() != 1 {
		t.Fatalf("Dropped() = %d, want 1", recorder.Dropped())
	}
}

func TestRecorderPersistsHTTPEvent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "audit.db")
	recorder, err := Open(context.Background(), dsn, 8, true)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	recorder.RecordHTTP(HTTPEvent{
		Time:            time.Now().UTC(),
		ClientID:        "sandbox-1",
		Method:          http.MethodGet,
		URL:             "https://api.example.com/v1",
		Host:            "api.example.com",
		Status:          http.StatusOK,
		Duration:        1500 * time.Microsecond,
		Blocked:         true,
		BlockedReason:   "host denied",
		CacheHit:        true,
		CacheKey:        "api.example.com/v1",
		ResponseBytes:   42,
		Upgrade:         true,
		UpgradeType:     "websocket",
		UpgradeC2SBytes: 7,
		UpgradeS2CBytes: 9,
		RequestHeaders:  http.Header{"Authorization": []string{"Bearer secret"}},
		ResponseHeaders: http.Header{"Set-Cookie": []string{"secret=value"}},
	})
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	pools, err := gormdb.Open(gormdb.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer pools.Close()
	var exchange HTTPExchange
	if err := pools.Read.First(&exchange).Error; err != nil {
		t.Fatalf("read exchange: %v", err)
	}
	if exchange.ClientID != "sandbox-1" {
		t.Fatalf("ClientID = %q", exchange.ClientID)
	}
	if exchange.RequestHeaders == "" || exchange.RequestHeaders == "{}" {
		t.Fatalf("expected redacted headers, got %q", exchange.RequestHeaders)
	}
	if exchange.RequestHeaders == `{"Authorization":["Bearer secret"]}` {
		t.Fatal("authorization header was not redacted")
	}
	if exchange.ResponseHeaders == `{"Set-Cookie":["secret=value"]}` {
		t.Fatal("set-cookie header was not redacted")
	}
	if exchange.EnqueuedAt.IsZero() || exchange.WrittenAt.IsZero() {
		t.Fatalf("expected queue/write timestamps, got enqueued=%s written=%s", exchange.EnqueuedAt, exchange.WrittenAt)
	}
	if exchange.DurationMicros != 1500 {
		t.Fatalf("DurationMicros = %d, want 1500", exchange.DurationMicros)
	}
	if !exchange.Blocked || exchange.BlockedReason != "host denied" {
		t.Fatalf("blocked metadata = %v %q", exchange.Blocked, exchange.BlockedReason)
	}
	if !exchange.CacheHit || exchange.CacheKey != "api.example.com/v1" {
		t.Fatalf("cache metadata = hit:%v key:%q", exchange.CacheHit, exchange.CacheKey)
	}
	if exchange.ResponseBytes != 42 {
		t.Fatalf("ResponseBytes = %d, want 42", exchange.ResponseBytes)
	}
	if !exchange.Upgrade || exchange.UpgradeType != "websocket" || exchange.UpgradeC2SBytes != 7 || exchange.UpgradeS2CBytes != 9 {
		t.Fatalf("upgrade metadata = %#v", exchange)
	}
}

func TestRecorderPersistsSOCKSEvent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "audit.db")
	recorder, err := Open(context.Background(), dsn, 8, true)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	recorder.RecordSOCKS(SOCKSEvent{
		Time:          time.Now().UTC(),
		ClientID:      "sandbox-1",
		ClientSubject: "CN=sandbox-1",
		ClientSerial:  "123",
		Destination:   "denied.example.com",
		Port:          443,
		Allowed:       false,
		BlockedReason: "host denied",
	})
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	pools, err := gormdb.Open(gormdb.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer pools.Close()
	var connect SOCKSConnect
	if err := pools.Read.First(&connect).Error; err != nil {
		t.Fatalf("read socks connect: %v", err)
	}
	if connect.ClientID != "sandbox-1" || connect.ClientSubject != "CN=sandbox-1" || connect.ClientSerial != "123" {
		t.Fatalf("client identity = %q %q %q", connect.ClientID, connect.ClientSubject, connect.ClientSerial)
	}
	if connect.Allowed || connect.BlockedReason != "host denied" {
		t.Fatalf("allowed metadata = %v %q", connect.Allowed, connect.BlockedReason)
	}
	if connect.EnqueuedAt.IsZero() || connect.WrittenAt.IsZero() {
		t.Fatalf("expected queue/write timestamps, got enqueued=%s written=%s", connect.EnqueuedAt, connect.WrittenAt)
	}
}

func TestRecorderListsAuditRows(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "audit.db")
	recorder, err := Open(context.Background(), dsn, 8, true)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	recorder.RecordHTTP(HTTPEvent{Time: time.Now().UTC(), ClientID: "sandbox-1", Host: "api.example.com", Method: http.MethodGet})
	recorder.RecordHTTP(HTTPEvent{Time: time.Now().UTC(), ClientID: "sandbox-2", Host: "other.example.com", Method: http.MethodGet})
	recorder.RecordSOCKS(SOCKSEvent{Time: time.Now().UTC(), ClientID: "sandbox-1", Destination: "api.example.com", Port: 443, Allowed: true})
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Close released the recorder's database handle, so read through a fresh
	// one. That is also how the rows are read in practice: the proxy writes
	// them, and the control endpoint serves them from a live recorder.
	reader, err := Open(context.Background(), dsn, 8, true)
	if err != nil {
		t.Fatalf("reopen recorder: %v", err)
	}
	defer reader.Close()

	httpRows, err := reader.ListHTTP(context.Background(), QueryOptions{ClientID: "sandbox-1", Host: "api.example.com", Limit: 10})
	if err != nil {
		t.Fatalf("ListHTTP() error = %v", err)
	}
	if len(httpRows) != 1 || httpRows[0].ClientID != "sandbox-1" {
		t.Fatalf("HTTP rows = %#v", httpRows)
	}

	socksRows, err := reader.ListSOCKS(context.Background(), QueryOptions{Host: "api.example.com", Limit: 1})
	if err != nil {
		t.Fatalf("ListSOCKS() error = %v", err)
	}
	if len(socksRows) != 1 || socksRows[0].Destination != "api.example.com" {
		t.Fatalf("SOCKS rows = %#v", socksRows)
	}
}
