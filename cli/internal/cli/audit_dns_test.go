package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	idpkg "github.com/discobox-ai/x/id"
)

func TestAuditDNSSendsItsFiltersAndShowsWhatIsMissing(t *testing.T) {
	sandboxID, err := idpkg.New("sbx")
	if err != nil {
		t.Fatal(err)
	}
	poolID, err := idpkg.New("pool")
	if err != nil {
		t.Fatal(err)
	}
	var query url.Values
	stdout, stderr, err := runAudit(context.Background(), t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/project-1/audit/dns" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"queries":[
			{"poolId":"pool-a","id":"dns_2","createdAt":"2026-09-24T10:01:00Z","sandboxId":"sbx_1","name":"evil\u001b[2J.example","type":"A","rcode":"NXDOMAIN","answers":[]},
			{"poolId":"pool-a","id":"dns_1","createdAt":"2026-09-24T10:00:00Z","sandboxId":"sbx_1","name":"api.github.com","type":"AAAA","rcode":"","answers":[],"error":"upstream timed out"}
		],"unavailablePools":[{"poolId":"pool-c","reason":"it did not answer within 20s"}]}`))
	}, "dns", "--discobox-id", sandboxID, "--pool", poolID, "--name", "API.GitHub.com.", "--since", "30m", "--limit", "7")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// The name is sent the way the pool records it: lowercased, no root dot.
	if query.Get("sandboxId") != sandboxID || query.Get("poolId") != poolID || query.Get("name") != "api.github.com" ||
		query.Get("limit") != "7" || query.Get("since") == "" {
		t.Fatalf("query = %v, want every filter", query)
	}
	for _, want := range []string{"dns_2", "NXDOMAIN", "failed", "upstream timed out"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("table missing %q:\n%s", want, stdout)
		}
	}
	// The name is what the discobox asked, and is shown as data.
	if strings.ContainsRune(stdout, 0x1b) {
		t.Fatalf("table carries a raw escape:\n%q", stdout)
	}
	if !strings.Contains(stderr, "pool-c") || !strings.Contains(stderr, "lookups are missing") {
		t.Fatalf("stderr = %q, want pool-c named as missing lookups", stderr)
	}

	stdout, _, err = runAudit(context.Background(), t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"queries":[],"unavailablePools":[{"poolId":"pool-c","reason":"gone"}]}`))
	}, "dns", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Queries          []any `json:"queries"`
		UnavailablePools []any `json:"unavailablePools"`
	}
	if err := json.Unmarshal([]byte(stdout), &body); err != nil || body.Queries == nil || len(body.UnavailablePools) != 1 {
		t.Fatalf("-o json = %s (%v), want the list and the missing pool together", stdout, err)
	}
}

// A dns_ ID is read from its pool through the list filtered to that ID, and a
// row that is not that one is not found rather than shown in its place.
func TestAuditGetReadsADNSRecordFromItsPool(t *testing.T) {
	sandboxID, err := idpkg.New("sbx")
	if err != nil {
		t.Fatal(err)
	}
	var query url.Values
	answer := `{"queries":[{"poolId":"pool-a","id":"dns_5","createdAt":"2026-09-24T10:00:00Z","sandboxId":"` + sandboxID +
		`","name":"api.github.com","type":"A","rcode":"NOERROR","answers":["192.0.2.1","192.0.2.2"],"durationMillis":3}],"unavailablePools":[]}`
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/project-1/audit/dns" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}
	stdout, _, err := runAudit(context.Background(), t, handler, "get", sandboxID, "dns_5", "--pool", "pool-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if query.Get("poolId") != "pool-a" || query.Get("sandboxId") != sandboxID || query.Get("id") != "dns_5" || query.Has("after") || query.Has("order") {
		t.Fatalf("query = %v, want the record's ID on its pool", query)
	}
	for _, want := range []string{"dns_5", "api.github.com", "NOERROR", "192.0.2.1, 192.0.2.2"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("record missing %q:\n%s", want, stdout)
		}
	}
	if _, _, err := runAudit(context.Background(), t, handler, "get", sandboxID, "dns_9", "--pool", "pool-a"); err == nil {
		t.Fatal("dns_9 was answered with dns_5")
	}
}
