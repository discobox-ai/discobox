package sandboxes

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// owedFixture is useFixture's failed sandbox made one whose source a client
// pushes and nobody has reported delivered: what the source-await timeout
// leaves behind (ADR 26-11-03-618).
func owedFixture(t *testing.T) (*Service, *containerProvider) {
	t.Helper()
	service, provider, _ := useFixture(t)
	sb := storedSandbox(t, service)
	sb.Source = &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryPush}
	if err := service.store.UpdateSandbox(useContext(t), sb); err != nil {
		t.Fatalf("make the source push-delivered: %v", err)
	}
	return service, provider
}

func requireOwedRefusal(t *testing.T, err error) {
	t.Helper()
	var status apperrors.StatusError
	if !errors.As(err, &status) || status.Status != http.StatusConflict {
		t.Fatalf("err = %v, want a 409", err)
	}
	if !strings.Contains(err.Error(), "its source") || !strings.Contains(err.Error(), "discobox push sb-1") {
		t.Fatalf("err = %q, want it to say the source is owed and name the way out", err)
	}
}

// An action on a failed discobox owed its delivery is refused before the pool
// is asked to run it: neither started, whether it kept its container, nor
// repaired automatically, whether it lost it — a repair would only park it
// again, and the action's wait would run out (ADR 26-11-03-618 §2).
func TestAnActionOnAFailedSandboxOwedItsSourceIsRefused(t *testing.T) {
	for _, container := range []bool{true, false} {
		service, provider := owedFixture(t)
		provider.container.Store(container)
		ctx := useContext(t)

		lease, _, err := service.AcquireSandboxHTTPClient(ctx, "project-1", "sb-1", nil, services.SandboxAction("exec attach"))
		if lease != nil {
			lease.Release()
		}
		requireOwedRefusal(t, err)
		if n := provider.ensures.Load(); container && n != 0 {
			t.Fatalf("container kept: ensures = %d, want 0", n)
		}
		if sb := storedSandbox(t, service); sb.Generation != 3 || sb.AutomaticRepair != nil {
			t.Fatalf("container %v: generation %d, automatic repair %+v; want no repair recorded", container, sb.Generation, sb.AutomaticRepair)
		}
		if provider.archives.Load() != 0 || provider.creates.Load() != 0 || provider.starts.Load() != 0 {
			t.Fatalf("container %v: the owed sandbox was rebuilt or started", container)
		}
	}
}

// A start or a restart is refused the same way, and a stop is not a use.
func TestAStartOrRestartOfASandboxOwedItsSourceIsRefused(t *testing.T) {
	service, provider := owedFixture(t)
	provider.container.Store(true)
	ctx := useContext(t)

	_, err := service.StartSandbox(ctx, "project-1", "sb-1", services.StartSandboxBody{})
	requireOwedRefusal(t, err)
	_, err = service.RestartSandbox(ctx, "project-1", "sb-1", services.RestartSandboxBody{})
	requireOwedRefusal(t, err)
	if n := provider.starts.Load(); n != 0 {
		t.Fatalf("starts = %d, want 0", n)
	}
	if _, err := service.StopSandbox(ctx, "project-1", "sb-1", services.StopSandboxBody{}); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// A parked sandbox is refused a start as well: the instruction waits for
// nothing, so nothing else would hold it back from the pool.
func TestAStartOfAParkedSandboxOwedItsSourceIsRefused(t *testing.T) {
	service, provider := owedFixture(t)
	provider.container.Store(true)
	ctx := useContext(t)
	sb := storedSandbox(t, service)
	sb.State, sb.ErrorMessage = model.SandboxStateAwaitingSource, nil
	if err := service.store.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("park the sandbox: %v", err)
	}

	_, err := service.StartSandbox(ctx, "project-1", "sb-1", services.StartSandboxBody{})
	requireOwedRefusal(t, err)
	if !strings.Contains(err.Error(), "waiting for its source") {
		t.Fatalf("err = %q, want it to say the sandbox is waiting for its source", err)
	}
	if n := provider.starts.Load(); n != 0 {
		t.Fatalf("starts = %d, want 0: a parked sandbox was started before its delivery", n)
	}
}

// Once delivered, the same failed sandbox is an ordinary one again.
func TestADeliveredFailedSandboxIsNotRefusedAsOwed(t *testing.T) {
	service, _ := owedFixture(t)
	sb := storedSandbox(t, service)
	at := sb.CreatedAt
	sb.SourceDeliveredAt = &at
	if err := owedSourceRefusal(sb); err != nil {
		t.Fatalf("delivered: %v", err)
	}
}
