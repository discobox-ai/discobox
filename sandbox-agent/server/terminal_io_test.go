package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	agentstore "github.com/discobox-ai/discobox/sandbox-agent/store"
	"github.com/discobox-ai/discobox/sandbox-agent/terminal"
)

// A wait on hook events answers with the first hook the caller named, recorded
// after the wait began or after the resume point it was given, and every
// answer carries a resume point from which nothing is missed (ADR 0137 §3).
func TestWaitAnswersWithTheNamedHook(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	st := openAgentStore(t)
	runner := &sandboxAgentFakeRunner{}
	cfg := testConfigWithRunner(publicKey, runner)
	cfg.Store = st
	cfg.ExecAuditRecorder = nil
	cfg.Resources.SampleInterval = time.Hour
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: cfg.WorkingRoot,
		RuntimeDir:  cfg.RuntimeDir,
		Units:       runner,
		Audit:       st,
		Env:         map[string]string{"PATH": "/usr/bin"},
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	service, err := terminal.NewService(terminal.ServiceConfig{
		Execs:       execManager,
		Harness:     cfg.Harness,
		WorkingRoot: cfg.WorkingRoot,
		RuntimeDir:  cfg.RuntimeDir,
		Env:         map[string]string{"PATH": "/usr/bin"},
		Units:       runner,
		Installer:   cfg.Installer,
	})
	if err != nil {
		t.Fatalf("new terminal service: %v", err)
	}
	term, err := service.Create(context.Background(), terminal.CreateRequest{})
	if err != nil {
		t.Fatalf("create terminal: %v", err)
	}
	record := func(event string) agentstore.HarnessHookRecord {
		t.Helper()
		hook, err := st.RecordHarnessHook(context.Background(), agentstore.HarnessHookRecord{TerminalID: term.ID, Provider: "claude", Event: event})
		if err != nil {
			t.Fatalf("record hook: %v", err)
		}
		return hook
	}
	type result struct {
		Reason      string `json:"reason"`
		ResumeAfter string `json:"resumeAfter"`
		Hook        struct {
			ID    string `json:"id"`
			Event string `json:"event"`
		} `json:"hook"`
	}
	wait := func(body string) (int, result) {
		t.Helper()
		resp := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/projects/project-1/sandboxes/sandbox-1/execs/"+term.ID+"/wait", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeExecRead))
		router.ServeHTTP(resp, req)
		var out result
		_ = json.Unmarshal(resp.Body.Bytes(), &out)
		return resp.Code, out
	}

	// Recorded while the wait is under way: a hook not named is passed over.
	go func() {
		time.Sleep(50 * time.Millisecond)
		record("Notification")
		record("Stop")
	}()
	code, got := wait(`{"until":{"hookEvents":["Stop"]},"timeoutSeconds":5}`)
	if code != http.StatusOK || got.Reason != "hook" || got.Hook.Event != "Stop" {
		t.Fatalf("wait = %d %+v, want the Stop hook", code, got)
	}
	stop := got.Hook.ID

	// A hook recorded between two waits is not missed by a caller who resumes
	// where the last one said to, and the hook it resumes after is not found
	// again.
	record("Stop")
	code, got = wait(`{"until":{"hookEvents":["Stop"],"after":"` + got.ResumeAfter + `"},"timeoutSeconds":5}`)
	if code != http.StatusOK || got.Reason != "hook" || got.Hook.ID == stop {
		t.Fatalf("wait after %s = %d %+v, want the later Stop hook", stop, code, got)
	}

	// A wait that times out resumes from where it began, so a hook recorded as
	// it returned is found by the next.
	code, timedOut := wait(`{"until":{"hookEvents":["Stop"],"after":"` + got.ResumeAfter + `"},"timeoutSeconds":1}`)
	if code != http.StatusOK || timedOut.Reason != "timeout" || timedOut.ResumeAfter != got.ResumeAfter {
		t.Fatalf("wait with nothing recorded = %d %+v, want timeout resuming from %s", code, timedOut, got.ResumeAfter)
	}
	late := record("Stop")
	code, got = wait(`{"until":{"hookEvents":["Stop"],"after":"` + timedOut.ResumeAfter + `"},"timeoutSeconds":5}`)
	if code != http.StatusOK || got.Hook.ID != late.ID {
		t.Fatalf("wait after a timeout = %d %+v, want the Stop recorded between the calls", code, got)
	}

	if code, _ := wait(`{"until":{"hookEvents":["Stop"],"after":"evt_1"},"timeoutSeconds":1}`); code != http.StatusBadRequest {
		t.Fatalf("wait after a hook ID = %d, want 400: after takes a resume point", code)
	}
	if code, _ := wait(`{"until":{},"timeoutSeconds":1}`); code != http.StatusBadRequest {
		t.Fatalf("wait for nothing = %d, want 400", code)
	}
}
