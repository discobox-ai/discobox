package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const reprovisionSandboxID = "sbx_reprovision0000000000000"

// reprovisionServer answers a reprovision with started, and each read of the
// discobox with the next of recorded as its agent's last provisioning,
// holding the last.
func reprovisionServer(t *testing.T, started string, recorded ...string) *httptest.Server {
	t.Helper()
	poll := reprovisionPoll
	reprovisionPoll = time.Millisecond
	t.Cleanup(func() { reprovisionPoll = poll })
	var mu sync.Mutex
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/project-1/sandboxes/"+reprovisionSandboxID+"/reprovision":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(started))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes":
			_, _ = w.Write([]byte(`{"sandboxes":[` + reprovisionSandbox(`{}`) + `]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes/"+reprovisionSandboxID:
			mu.Lock()
			doc := recorded[min(reads, len(recorded)-1)]
			reads++
			mu.Unlock()
			_, _ = w.Write([]byte(reprovisionSandbox(doc)))
		default:
			http.Error(w, `{"error":"unexpected `+r.Method+` `+r.URL.Path+`"}`, http.StatusTeapot)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// reprovisionSandbox is the running discobox whose agent last reported doc.
func reprovisionSandbox(doc string) string {
	return `{"id":"` + reprovisionSandboxID + `","projectId":"project-1","createdByUserId":"user-1","poolId":"pool-1",` +
		`"displayName":"box","config":{"name":"box","image":""},` +
		`"runtime":{"state":"ready","runtimeState":"running","displayState":"running","desiredState":"present","generation":1,"observedGeneration":1,` +
		`"agentStatus":{"provision":` + doc + `}},` +
		`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
}

// The command follows the run in the discobox's record: a record from
// before the run is passed over, each feature it runs is narrated as it
// moves, and it ends once every one is ready (ADR 26-11-11-560 §5).
func TestReprovisionFollowsTheRunToReady(t *testing.T) {
	server := reprovisionServer(t,
		`{"state":"provisioning","ready":true,"updatedAt":"2026-11-12T10:00:00Z","features":{"core":{"state":"ready"},"browser":{"state":"pending","async":true},"desktop":{"state":"pending","async":true}}}`,
		`{"state":"failed","ready":true,"updatedAt":"2026-11-12T09:00:00Z","features":{"core":{"state":"ready"},"browser":{"state":"failed","error":"stale"},"desktop":{"state":"failed"}}}`,
		`{"state":"provisioning","ready":true,"updatedAt":"2026-11-12T10:00:01Z","features":{"core":{"state":"ready"},"browser":{"state":"provisioning","step":"install"},"desktop":{"state":"pending"}}}`,
		`{"state":"ready","ready":true,"updatedAt":"2026-11-12T10:00:05Z","features":{"core":{"state":"ready"},"browser":{"state":"ready"},"desktop":{"state":"ready"}}}`,
	)
	out := runCLI(t, server.URL, "admin", "box", "reprovision", reprovisionSandboxID)
	for _, want := range []string{"reprovisioning browser, desktop\n", "browser: provisioning\n", "browser: ready\n", "desktop: ready\n", "every feature is provisioned\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"stale", "core"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output names %q:\n%s", unwanted, out)
		}
	}
}

// A feature that fails again fails the command, named with its new error
// and its log.
func TestReprovisionFailsNamingWhatFailedAgain(t *testing.T) {
	server := reprovisionServer(t,
		`{"state":"provisioning","ready":true,"updatedAt":"2026-11-12T10:00:00Z","features":{"browser":{"state":"pending","async":true}}}`,
		`{"state":"failed","ready":true,"updatedAt":"2026-11-12T10:00:03Z","features":{"browser":{"state":"failed","error":"step \"install\": exit status 1","log":"/var/log/discobox/provision/browser.log"}}}`,
	)
	refused := runCLIRefused(t, server.URL, "admin", "box", "reprovision", reprovisionSandboxID)
	if !strings.Contains(refused, "provisioning failed: browser") {
		t.Fatalf("refused with %q", refused)
	}
}

// Nothing failed: nothing runs, and the command says so at once. With
// --no-wait it returns once the run has started.
func TestReprovisionWithNothingFailedOrNoWait(t *testing.T) {
	server := reprovisionServer(t, `{"state":"ready","ready":true,"updatedAt":"2026-11-12T10:00:00Z","features":{"core":{"state":"ready"}}}`, `{}`)
	if out := runCLI(t, server.URL, "admin", "box", "reprovision", reprovisionSandboxID); !strings.Contains(out, "nothing to reprovision") {
		t.Errorf("output:\n%s", out)
	}
	server = reprovisionServer(t, `{"state":"provisioning","ready":false,"updatedAt":"2026-11-12T10:00:00Z","features":{"core":{"state":"pending"}}}`, `{}`)
	if out := runCLI(t, server.URL, "admin", "box", "reprovision", "--no-wait", reprovisionSandboxID); out != "reprovisioning core\n" {
		t.Errorf("output:\n%q", out)
	}
}

// A joined run (the boot's async run, still going) may already have failed a
// feature that nothing then runs again: the command does not exit 0 over it.
func TestReprovisionJoiningARunCountsWhatItAlreadyFailed(t *testing.T) {
	server := reprovisionServer(t,
		`{"state":"provisioning","ready":true,"updatedAt":"2026-11-12T10:00:00Z","features":{"go":{"state":"failed","error":"download: EOF"},"nix":{"state":"provisioning","async":true}}}`,
		`{"state":"failed","ready":true,"updatedAt":"2026-11-12T10:01:00Z","features":{"go":{"state":"failed","error":"download: EOF"},"nix":{"state":"ready"}}}`,
	)
	refused := runCLIRefused(t, server.URL, "admin", "box", "reprovision", reprovisionSandboxID)
	if !strings.Contains(refused, "provisioning failed: go") || !strings.Contains(refused, "did not run again: reprovision again") {
		t.Fatalf("refused with %q", refused)
	}
}

// Every string the box wrote is escaped before it reaches the terminal,
// feature names included (cli/REVIEW.md).
func TestReprovisionEscapesWhatTheBoxWrote(t *testing.T) {
	server := reprovisionServer(t,
		`{"state":"provisioning","ready":true,"updatedAt":"2026-11-12T10:00:00Z","features":{"x\u001b]0;pwned\u0007":{"state":"pending"}}}`,
		`{"state":"ready","ready":true,"updatedAt":"2026-11-12T10:00:05Z","features":{"x\u001b]0;pwned\u0007":{"state":"ready"}}}`,
	)
	out := runCLI(t, server.URL, "admin", "box", "reprovision", reprovisionSandboxID)
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Fatalf("a control character reached the terminal: %q", out)
	}
}
