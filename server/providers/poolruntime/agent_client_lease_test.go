package poolruntime

import (
	"net/http"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// The lease is what makes `https://pool` mean this pool's agent. A client built
// without one falls back to http.DefaultClient and dials that name for real,
// so a spent poolAgentClient used to report `lookup pool: no such host` for
// what is a caller reusing it.
func TestPoolClientRefusesASpentLease(t *testing.T) {
	released := 0
	lease := transport.NewHTTPClientLease(&http.Client{}, func() { released++ })
	client := &poolAgentClient{poolID: "pool-1", lease: lease}

	if _, release, err := client.poolClient(sandbox.SandboxRef{ProjectID: "project-1"}); err != nil {
		t.Fatalf("first call: %v", err)
	} else {
		release()
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}

	_, _, err := client.poolClient(sandbox.SandboxRef{ProjectID: "project-1"})
	if err == nil {
		t.Fatal("a spent client built a second pool client; it has no transport to dial with")
	}
	if !strings.Contains(err.Error(), "spent") {
		t.Fatalf("error = %v, want it to name the spent client", err)
	}
}

func TestNewWorkerAgentClientRequiresALease(t *testing.T) {
	if _, err := newWorkerAgentClient(nil); err == nil {
		t.Fatal("built a pool-agent client with no transport; it would dial the literal https://pool")
	}
}
