package audit

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/x/gormdb"

	"github.com/discobox-ai/discobox/pkg/auditid"
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
	recorder.RecordHTTP(HTTPEvent{Time: time.Now().UTC(), ClientID: "sandbox-3", Host: "api.example.com:443", Method: http.MethodConnect, Blocked: true})
	recorder.RecordHTTP(HTTPEvent{Time: time.Now().UTC(), ClientID: "sandbox-3", Host: "api.example.com.evil:443", Method: http.MethodConnect, Blocked: true})
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

	// A host named without a port keeps the CONNECT the policy refused, which
	// is recorded by its authority, and nothing that only begins with it.
	for host, want := range map[string]int{"api.example.com": 1, "api.example.com:443": 1, "api.example.com:8443": 0} {
		rows, err := reader.ListHTTP(context.Background(), QueryOptions{ClientID: "sandbox-3", Host: host, Limit: 10})
		if err != nil || len(rows) != want {
			t.Fatalf("host %q: %d rows (err %v), want %d", host, len(rows), err, want)
		}
	}

	socksRows, err := reader.ListSOCKS(context.Background(), QueryOptions{Host: "api.example.com", Limit: 1})
	if err != nil {
		t.Fatalf("ListSOCKS() error = %v", err)
	}
	if len(socksRows) != 1 || socksRows[0].Destination != "api.example.com" {
		t.Fatalf("SOCKS rows = %#v", socksRows)
	}
}

// The cursor reads along the write order, which is the order rows become
// readable. Reading forward by time loses a row stamped earlier and written
// later, which is what the recorder's queue produces under load; reading
// forward by id cannot.
func TestListHTTPCursorReadsRowsWrittenOutOfTimeOrder(t *testing.T) {
	recorder, err := Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"), 8, true)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = recorder.Close() })

	base := time.Now().UTC().Add(-time.Hour)
	// Written in this order; the slow request started first and ended last.
	recorder.RecordHTTP(HTTPEvent{Time: base, ClientID: "sandbox-1", URL: "https://first.example.com"})
	recorder.RecordHTTP(HTTPEvent{Time: base.Add(2 * time.Minute), ClientID: "sandbox-1", URL: "https://fast.example.com"})
	recorder.RecordHTTP(HTTPEvent{Time: base.Add(time.Minute), ClientID: "sandbox-1", URL: "https://slow.example.com"})
	drainRecorder(t, recorder)

	read := recorder.ListHTTP
	forward, err := read(context.Background(), QueryOptions{Ascending: true, Limit: 2})
	if err != nil {
		t.Fatalf("ListHTTP() error = %v", err)
	}
	if len(forward) != 2 || forward[1].URL != "https://slow.example.com" {
		t.Fatalf("time-ordered read = %+v", forward)
	}
	// A follower that had printed up to the fast row and asked again by time
	// would never be given the slow row: it is older than what it has seen.
	byTime, err := read(context.Background(), QueryOptions{Since: base.Add(2 * time.Minute), Ascending: true})
	if err != nil {
		t.Fatalf("ListHTTP() error = %v", err)
	}
	for _, row := range byTime {
		if row.URL == "https://slow.example.com" {
			t.Fatal("reading forward by time returned the late row; this test no longer proves anything")
		}
	}
	// Paging back is by time, newest first and inclusive of its bound.
	back, err := read(context.Background(), QueryOptions{Until: base.Add(time.Minute), Limit: 1})
	if err != nil {
		t.Fatalf("ListHTTP() error = %v", err)
	}
	if len(back) != 1 || back[0].URL != "https://slow.example.com" {
		t.Fatalf("read back from the slow row = %+v", back)
	}

	// By cursor it is the next row, because it was written next.
	all, err := read(context.Background(), QueryOptions{Ascending: true})
	if err != nil {
		t.Fatalf("ListHTTP() error = %v", err)
	}
	var fastID auditid.ExchangeID
	for _, row := range all {
		if row.URL == "https://fast.example.com" {
			fastID = row.ID
		}
	}
	if fastID == 0 {
		t.Fatalf("rows = %+v, want the fast row's id", all)
	}
	byCursor, err := read(context.Background(), QueryOptions{AfterID: fastID})
	if err != nil {
		t.Fatalf("ListHTTP() error = %v", err)
	}
	if len(byCursor) != 1 || byCursor[0].URL != "https://slow.example.com" {
		t.Fatalf("cursor read after the fast row = %+v, want the row written after it", byCursor)
	}
	// And the cursor is where reading stops repeating itself: nothing is
	// returned twice, and there is no window to re-walk.
	if rows, err := read(context.Background(), QueryOptions{AfterID: byCursor[0].ID}); err != nil || len(rows) != 0 {
		t.Fatalf("cursor read after the last row = %+v, %v", rows, err)
	}
}

func TestRecorderPersistsDNSEvent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "audit.db")
	recorder, err := Open(context.Background(), dsn, 8, true)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	recorder.RecordDNS(DNSEvent{
		Time:          time.Now().UTC(),
		ClientID:      "sandbox-1",
		ClientSubject: "CN=sandbox-1",
		Name:          "api.example.com",
		Type:          "A",
		RCode:         "NOERROR",
		Answers:       []string{"192.0.2.1", "192.0.2.2"},
		Duration:      1500 * time.Microsecond,
	})
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	pools, err := gormdb.Open(gormdb.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer pools.Close()
	var query DNSQuery
	if err := pools.Read.First(&query).Error; err != nil {
		t.Fatalf("read dns query: %v", err)
	}
	if query.ID.String() != "dns_1" || query.ClientID != "sandbox-1" || query.Name != "api.example.com" || query.Type != "A" || query.RCode != "NOERROR" {
		t.Fatalf("row = %+v", query)
	}
	if query.Answers != "192.0.2.1,192.0.2.2" || query.DurationMicros != 1500 || query.DurationMillis != 1 {
		t.Fatalf("answers %q, duration %dus/%dms", query.Answers, query.DurationMicros, query.DurationMillis)
	}
	if query.EnqueuedAt.IsZero() || query.WrittenAt.IsZero() {
		t.Fatalf("expected queue/write timestamps, got enqueued=%s written=%s", query.EnqueuedAt, query.WrittenAt)
	}
}

func TestRecorderListsDNSByClientNameAndCursor(t *testing.T) {
	recorder, err := Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"), 8, true)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer recorder.Close()
	base := time.Now().UTC()
	for i, event := range []DNSEvent{
		{ClientID: "sandbox-1", Name: "a.example.com"},
		{ClientID: "sandbox-2", Name: "a.example.com"},
		{ClientID: "sandbox-1", Name: "b.example.com"},
		{ClientID: "sandbox-1", Name: "a.example.com"},
	} {
		event.Time = base.Add(time.Duration(i) * time.Second)
		recorder.RecordDNS(event)
	}
	waitForDNSRows(t, recorder, 4)

	rows, err := recorder.ListDNS(context.Background(), DNSQueryOptions{ClientID: "sandbox-1", Name: "a.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != 4 || rows[1].ID != 1 {
		t.Fatalf("filtered rows newest first = %+v", rows)
	}
	rows, err = recorder.ListDNS(context.Background(), DNSQueryOptions{AfterID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != 3 || rows[1].ID != 4 {
		t.Fatalf("rows after dns_2 in write order = %+v", rows)
	}
}

func waitForDNSRows(t *testing.T, recorder *Recorder, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := recorder.ListDNS(context.Background(), DNSQueryOptions{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("recorder wrote %d dns rows, want %d", len(rows), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// One sandbox's flood of lookups spends its own budget, not the trail every
// sandbox's lookups share.
func TestRecorderBoundsEachSandboxsDNS(t *testing.T) {
	recorder, err := Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"), 100000, true)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	for range dnsAuditBurst + 200 {
		recorder.RecordDNS(DNSEvent{ClientID: "flooder", Name: "x.example"})
	}
	if dropped := recorder.DNSDropped(); dropped < 100 {
		t.Fatalf("DNSDropped = %d, want the flood past the burst dropped", dropped)
	}
	recorder.RecordDNS(DNSEvent{ClientID: "neighbor", Name: "y.example"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := recorder.ListDNS(context.Background(), DNSQueryOptions{ClientID: "neighbor"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the neighbor's lookup was never written")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// DNS has a queue of its own: lookups dropped there are not HTTP rows lost,
// and do not stop an HTTP event from being taken.
func TestRecorderKeepsDNSOffTheHTTPQueue(t *testing.T) {
	recorder, err := Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close()
	for i := range 2000 {
		recorder.RecordDNS(DNSEvent{ClientID: fmt.Sprintf("sandbox-%d", i), Name: "x.example"})
	}
	recorder.RecordHTTP(HTTPEvent{ClientID: "sandbox-1", Host: "api.example.com", Method: http.MethodGet})
	if dropped := recorder.Dropped(); dropped != 0 {
		t.Fatalf("Dropped = %d HTTP/SOCKS events after a DNS flood, want 0", dropped)
	}
	if recorder.DNSDropped() == 0 {
		t.Fatal("a one-slot DNS queue took 2000 lookups without dropping any")
	}
}

// A request's secrets are recorded (ADR 26-10-01-240), and a pool's database written before the column existed is upgraded in place: its
// rows read the new column as empty rather than failing the read.
func TestRecorderRecordsSecretsAndUpgradesAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.db")
	recorder, err := Open(context.Background(), path, 8, true)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	recorder.RecordHTTP(HTTPEvent{ClientID: "sandbox-1", URL: "https://old.example.com"})
	drainRecorder(t, recorder)
	// The database as a pool wrote it before this column: the row is there,
	// the column is not.
	if err := recorder.db.Exec(`ALTER TABLE http_exchanges DROP COLUMN swapped_secret_ids`).Error; err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	recorder, err = Open(context.Background(), path, 8, true)
	if err != nil {
		t.Fatalf("reopening an older database: %v", err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	recorder.RecordHTTP(HTTPEvent{ClientID: "sandbox-1", URL: "https://new.example.com", SwappedSecretIDs: []string{"sec_a", "sec_b"}})
	drainRecorder(t, recorder)

	rows, err := recorder.ListHTTP(context.Background(), QueryOptions{})
	if err != nil {
		t.Fatalf("ListHTTP() error = %v", err)
	}
	got := map[string]string{}
	for _, row := range rows {
		got[row.URL] = row.SwappedSecretIDs
	}
	if secrets, ok := got["https://old.example.com"]; !ok || secrets != "" {
		t.Fatalf("the older row = %q (present %v), want it read with no secrets", secrets, ok)
	}
	if got["https://new.example.com"] != "sec_a,sec_b" {
		t.Fatalf("the new row's secrets = %q, want sec_a,sec_b", got["https://new.example.com"])
	}
}

// A database an earlier proxy wrote holds swapped_use_ids, which nothing reads
// or writes any more (ADR 26-10-27-880). Opening it drops the column and keeps
// the rows.
func TestOpenDropsTheRetiredUseIDColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.db")
	recorder, err := Open(context.Background(), path, 8, true)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	recorder.RecordHTTP(HTTPEvent{ClientID: "sandbox-1", URL: "https://old.example.com"})
	drainRecorder(t, recorder)
	if err := recorder.db.Exec(`ALTER TABLE http_exchanges ADD COLUMN swapped_use_ids text`).Error; err != nil {
		t.Fatalf("add column: %v", err)
	}
	if err := recorder.db.Exec(`UPDATE http_exchanges SET swapped_use_ids = 'use_old'`).Error; err != nil {
		t.Fatalf("fill column: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	recorder, err = Open(context.Background(), path, 8, true)
	if err != nil {
		t.Fatalf("reopening an older database: %v", err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	if recorder.db.Migrator().HasColumn(&HTTPExchange{}, "swapped_use_ids") {
		t.Fatal("swapped_use_ids is still there after Open")
	}
	recorder.RecordHTTP(HTTPEvent{ClientID: "sandbox-1", URL: "https://new.example.com"})
	drainRecorder(t, recorder)
	rows, err := recorder.ListHTTP(context.Background(), QueryOptions{})
	if err != nil {
		t.Fatalf("ListHTTP() error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the older row kept beside the new one", len(rows))
	}
}
