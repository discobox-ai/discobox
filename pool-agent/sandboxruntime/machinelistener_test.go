package sandboxruntime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// Allocation, activation, replacement and stop must never release the address.
// Parallel endpoints also exercise the same host's competing ephemeral binds.
func TestFakeMachineAgentOwnsItsEndpointAcrossBoots(t *testing.T) {
	for i := range 32 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			agents := &fakeMachineAgents{}
			t.Cleanup(agents.closeAll)
			service, err := agents.service(t.Context(), "machine")
			if err != nil {
				t.Fatal(err)
			}
			address := strings.TrimPrefix(service.Listen, "tcp:")
			assertOwned := func() {
				t.Helper()
				listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", address)
				if err == nil {
					listener.Close()
					t.Fatal("another listener stole the agent endpoint")
				}
			}
			assertOwned()
			cfg := sandboxconfig.Config{SandboxID: "sbx_1"}
			cfg.AgentRuntime.ListenAddress = address
			client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
			defer client.CloseIdleConnections()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, service.URL+"/healthz", nil)
			if err != nil {
				t.Fatal(err)
			}
			for range 5 {
				if err := agents.start("machine", cfg); err != nil {
					t.Fatal(err)
				}
				assertOwned()
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != http.StatusOK {
					t.Fatalf("health = %s", response.Status)
				}
				agents.stop("machine")
				assertOwned()
				response, err = client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("stopped health = %s", response.Status)
				}
			}
			agents.closeAll()
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", address)
			if err != nil {
				t.Fatalf("endpoint was not released at cleanup: %v", err)
			}
			listener.Close()
		})
	}
}

// A failed fake server must fail provisioning, before the product's five-minute
// agent readiness deadline. Exercise the actual applyProcess.Wait caller.
func TestFakeMachineAgentStartFailureReturnsFromProvisioning(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := t.Context()
	if _, err := f.runtime.CreateSandbox(ctx, machineCreate("sbx_1")); err != nil {
		t.Fatal(err)
	}
	if err := f.runtime.StopSandbox(ctx, "sbx_1", nil); err != nil {
		t.Fatal(err)
	}
	f.agents.mu.Lock()
	endpoint := f.agents.endpoints["sbx-1"]
	f.agents.mu.Unlock()
	endpoint.server.Close()
	<-endpoint.done
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := f.runtime.StartSandbox(ctx, "sbx_1", nil)
	if err == nil || !strings.Contains(err.Error(), "fake agent of sbx-1 cannot start") {
		t.Fatalf("start = %v, want fake-agent startup failure", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("startup waited for the deadline: %v", ctx.Err())
	}
}
