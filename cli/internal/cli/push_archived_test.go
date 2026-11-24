package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// archiveStateServer answers with one discobox whose runtime carries the given
// observed, desired and display states.
func archiveStateServer(t *testing.T, state, desired, display string) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id":              pushTestSandboxID,
		"projectId":       "project-1",
		"createdByUserId": "user-1",
		"displayName":     "box",
		"config":          map[string]any{"name": "box", "image": "", "source": pushTestSource(t, nil)},
		"runtime": map[string]any{
			"state":              state,
			"displayState":       display,
			"desiredState":       desired,
			"generation":         2,
			"observedGeneration": 1,
		},
		"createdAt": "2026-06-17T00:00:00Z",
		"updatedAt": "2026-06-17T00:00:01Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sandboxes/"+pushTestSandboxID) {
			http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

// push refuses a discobox archived by intent, as the server does, and judges
// that by the desired state rather than the state it is displayed in.
func TestPushJudgesArchivedByDesiredState(t *testing.T) {
	t.Run("being unarchived is not refused", func(t *testing.T) {
		err := runPushCommand(t, archiveStateServer(t, "archived", "present", "archived").URL)
		if err != nil && strings.Contains(err.Error(), "decode response") {
			t.Fatalf("fixture rejected by the client: %v", err)
		}
		if err != nil && strings.Contains(err.Error(), "unarchive it") {
			t.Fatalf("push to a discobox already asked to unarchive was refused: %v", err)
		}
	})
	t.Run("archived by intent after a failed archive is refused", func(t *testing.T) {
		err := runPushCommand(t, archiveStateServer(t, "failed", "archived", "error").URL)
		if err == nil || !strings.Contains(err.Error(), "unarchive it") {
			t.Fatalf("push to a discobox archived by intent: error = %v, want the archived refusal", err)
		}
	})
}
