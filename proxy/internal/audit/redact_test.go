package audit

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const redactSecret = "RAWSECRET-0123456789"

// fixedRedactor replaces one value.
type fixedRedactor struct{}

func (fixedRedactor) Redact(text string) string {
	return strings.ReplaceAll(text, redactSecret, "[REDACTED]")
}
func (fixedRedactor) Patterns() [][]byte { return [][]byte{[]byte(redactSecret)} }
func (fixedRedactor) MaxLen() int        { return len(redactSecret) }

func redactFor(clientID string) Redactor {
	if clientID == "sb-raw" {
		return fixedRedactor{}
	}
	return nil
}

// A client's raw secret never reaches its row: not in the URL, a header, or
// the reason, and the live request's headers are left as they were.
func TestRecordHTTPRedactsTheClientsRawSecrets(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "audit.db")
	recorder, err := Open(context.Background(), dsn, 8, true)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	recorder.SetRedactor(redactFor)
	live := http.Header{"X-Key": {"k=" + redactSecret}}
	recorder.RecordHTTP(HTTPEvent{
		Time: time.Now().UTC(), ClientID: "sb-raw", Host: "evil.example", Method: http.MethodGet,
		URL: "https://evil.example/?k=" + redactSecret, BlockedReason: "raw: sbd_1: " + redactSecret,
		RequestHeaders: live,
	})
	// A passthrough tunnel's ClientHello is the client's own text, and it may
	// put a raw value in the SNI or an ALPN entry (ADR 26-10-31-819 §4).
	recorder.RecordHTTP(HTTPEvent{
		Time: time.Now().UTC(), ClientID: "sb-raw", Host: "pinned.example:443", Method: http.MethodConnect,
		Tunnel: Tunnel{ID: "tun_1", Phase: TunnelPhaseOpen, ServerName: redactSecret + ".example", ALPN: "h2," + redactSecret, Error: "dial " + redactSecret},
	})
	recorder.RecordHTTP(HTTPEvent{
		Time: time.Now().UTC(), ClientID: "sb-other", Host: "evil.example", Method: http.MethodGet,
		URL: "https://evil.example/?k=" + redactSecret,
	})
	if err := recorder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if live.Get("X-Key") != "k="+redactSecret {
		t.Fatalf("the live request's header was changed: %q", live.Get("X-Key"))
	}
	reader, err := Open(context.Background(), dsn, 8, true)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reader.Close()
	rows, err := reader.ListHTTP(context.Background(), QueryOptions{ClientID: "sb-raw", Limit: 10})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v (%v)", rows, err)
	}
	tunnel := rows[0].Tunnel
	for name, text := range map[string]string{"server name": tunnel.ServerName, "alpn": tunnel.ALPN, "error": tunnel.Error} {
		if strings.Contains(text, redactSecret) || !strings.Contains(text, "[REDACTED]") {
			t.Errorf("tunnel %s = %q, want the secret redacted", name, text)
		}
	}
	row := rows[1]
	for name, text := range map[string]string{"url": row.URL, "headers": row.RequestHeaders, "reason": row.BlockedReason} {
		if strings.Contains(text, redactSecret) || !strings.Contains(text, "[REDACTED]") {
			t.Errorf("%s = %q, want the secret redacted", name, text)
		}
	}
	other, err := reader.ListHTTP(context.Background(), QueryOptions{ClientID: "sb-other", Limit: 10})
	if err != nil || len(other) != 1 || !strings.Contains(other[0].URL, redactSecret) {
		t.Fatalf("another client's row = %+v (%v), want it untouched", other, err)
	}
}

// A body spool is redacted however the secret is split across writes.
func TestBodySpoolRedactsAcrossWrites(t *testing.T) {
	dir := t.TempDir()
	recorder := &Recorder{enabled: true, bodyDir: dir, openSpool: map[string]struct{}{}}
	recorder.SetRedactor(redactFor)
	body := []byte(`{"password":"` + redactSecret + `","again":"` + redactSecret + `"}`)
	for split := 1; split < len(body); split += 3 {
		record, spool, err := recorder.BeginBody("sb-raw", BodyKindRequest)
		if err != nil || spool == nil {
			t.Fatalf("BeginBody() = %v, %v", spool, err)
		}
		for start := 0; start < len(body); start += split {
			end := min(start+split, len(body))
			if n, err := spool.Write(body[start:end]); err != nil || n != end-start {
				t.Fatalf("Write() = %d, %v", n, err)
			}
		}
		if err := spool.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, record.File))
		if err != nil {
			t.Fatal(err)
		}
		want := bytes.ReplaceAll(body, []byte(redactSecret), []byte("[REDACTED]"))
		if !bytes.Equal(got, want) {
			t.Fatalf("split %d: spooled %q, want %q", split, got, want)
		}
	}
}
