package programstatus

import "testing"

func TestPrimaryPosted(t *testing.T) {
	sessions := []byte(`[
		{"primary": true, "startedAt": "2026-01-01T00:00:00Z", "programStatus": {"state": "error"}},
		{"primary": true, "startedAt": "2026-02-01T00:00:00Z", "programStatus": {"state": "blocked", "kind": "permission", "app": "claude-code", "msg": "may I?", "newField": 1}},
		{"primary": true, "startedAt": "2026-04-01T00:00:00Z", "programStatus": {"state": "dancing"}},
		{"primary": true, "startedAt": "2026-05-01T00:00:00Z"},
		{"primary": false, "startedAt": "2026-03-01T00:00:00Z", "programStatus": {"state": "working"}}
	]`)
	got, ok := PrimaryPosted(sessions)
	if !ok {
		t.Fatal("found no status")
	}
	if got.State != Blocked || got.Kind != Permission || got.App != "claude-code" || got.Msg != "may I?" {
		t.Fatalf("status = %+v, want the newest primary session with a known state", got)
	}

	got, ok = PrimaryPosted([]byte(`[{"primary": true, "programStatus": {"state": "working", "kind": "telepathy"}}]`))
	if !ok || got.State != Working || got.Kind != "" {
		t.Fatalf("status = %+v, %v; want working with the unknown kind left out", got, ok)
	}
	for _, sessions := range []string{``, `[]`, `{"not": "a list"}`, `[{"primary": false, "programStatus": {"state": "idle"}}]`} {
		if got, ok := PrimaryPosted([]byte(sessions)); ok {
			t.Fatalf("%q reported %+v", sessions, got)
		}
	}
}
