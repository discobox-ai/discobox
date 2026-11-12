package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// A reprovision reaches the running sandbox's agent with exec:write, marked
// a use that never starts a sandbox, and the agent's answer is relayed
// (ADR 26-11-11-560 §5).
func TestReprovisionReachesARunningSandbox(t *testing.T) {
	router, stubs, reached := inventoryReadFixture(t, model.SandboxRuntimeStateRunning, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/sandboxes/sandbox-1/reprovision") {
			http.Error(w, "wrong route "+r.Method+" "+r.URL.Path, http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"state":"provisioning","ready":true,"features":{}}`))
	})
	resp := getSandboxRoute(router, http.MethodPost, "/reprovision", poolagentauth.ScopeExecWrite)
	if resp.Code != http.StatusAccepted || !strings.Contains(resp.Body.String(), `"state":"provisioning"`) {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if *reached != 1 {
		t.Fatalf("pool reached %d times", *reached)
	}
	if !slices.Equal(stubs.sandboxScopes, []string{poolagentauth.ScopeExecWrite}) {
		t.Fatalf("scopes = %v", stubs.sandboxScopes)
	}
	if !slices.Equal(stubs.sandboxUses, []services.SandboxUse{services.SandboxInspect}) {
		t.Fatalf("uses = %+v; want an inspect, which never starts a sandbox", stubs.sandboxUses)
	}
}

// A sandbox that is not running is refused with a conflict that says why,
// whether its pool refused it or could not be asked: a reprovision never
// starts one.
func TestReprovisionRefusesASandboxThatIsNotRunning(t *testing.T) {
	for _, unreachable := range []bool{false, true} {
		router, stubs, reached := inventoryReadFixture(t, model.SandboxRuntimeStateStopped, notRunningAnswer)
		stubs.poolUnreachable = unreachable
		resp := getSandboxRoute(router, http.MethodPost, "/reprovision", poolagentauth.ScopeExecWrite)
		if resp.Code != http.StatusConflict || !strings.Contains(resp.Body.String(), "discobox sandbox-1 is not running") || !strings.Contains(resp.Body.String(), "without starting one") {
			t.Fatalf("unreachable %v: status = %d, body = %s", unreachable, resp.Code, resp.Body.String())
		}
		if len(stubs.inventoryReads) != 0 {
			t.Fatalf("unreachable %v: answered from the inventory", unreachable)
		}
		if want := map[bool]int{false: 1, true: 0}[unreachable]; *reached != want {
			t.Fatalf("unreachable %v: pool reached %d times, want %d", unreachable, *reached, want)
		}
	}
}
