package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// check posts to the instance's check route and prints what the server
// answered. A check that did not pass exits non-zero, naming its outcome,
// with the provider's message in what it printed.
func TestProviderCheckPrintsTheOutcomeAndFailsUnlessOK(t *testing.T) {
	const providerID = "prov_dfzx0123456789ab"
	for _, tc := range []struct {
		name    string
		answer  map[string]any
		wantErr string
		wantOut []string
	}{
		{"ok", map[string]any{"outcome": "ok", "checkedAt": "2026-11-15T10:00:00Z"}, "", []string{"ok"}},
		{"unreachable", map[string]any{
			"outcome":   "unreachable",
			"message":   "boxd: dial tcp 1.2.3.4:443: connect: network is unreachable\ndial tcp 5.6.7.8:443: connect: network is unreachable\x1b[2J",
			"checkedAt": "2026-11-15T10:00:00Z",
		}, "provider " + providerID + " check: unreachable", []string{"unreachable", `network is unreachable\ndial tcp 5.6.7.8`, `\x1b[2J`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/providers/"+providerID+"/check") {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(tc.answer)
			}))
			defer server.Close()

			cmd := NewRootCommand()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"--server", server.URL, "admin", "provider", "check", providerID})
			err := cmd.Execute()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("check: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("check err = %v, want %q", err, tc.wantErr)
			}
			// The message stays in its one cell, its control runes shown,
			// not obeyed.
			if strings.ContainsAny(out.String(), "\x1b") || strings.Contains(out.String(), "unreachable\ndial") {
				t.Fatalf("output = %q, want the message on one line with its control runes escaped", out.String())
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("output = %q, want %q", out.String(), want)
				}
			}
		})
	}
}

// The JSON form escapes what a terminal would act on too, C1 controls and
// bidirectional overrides included, and still decodes to the same message.
func TestProviderCheckJSONEscapesTheMessage(t *testing.T) {
	const providerID = "prov_dfzx0123456789ab"
	message := "boxd refused \u009b2J and \u202etxt"
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"outcome":"refused","message":"` + message + `","checkedAt":"2026-11-15T10:00:00Z"}`))
	}))
	defer server.Close()

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--server", server.URL, "-o", "json", "admin", "provider", "check", providerID})
	_ = cmd.Execute()
	if strings.ContainsAny(out.String(), "\u009b\u202e") {
		t.Fatalf("output = %q, want C1 controls and bidi overrides escaped", out.String())
	}
	var decoded struct{ Message string }
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || decoded.Message != "boxd refused \u009b2J and \u202etxt" {
		t.Fatalf("decoded message = %q (err %v), want the provider's words unchanged", decoded.Message, err)
	}
}
