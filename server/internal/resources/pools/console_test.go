package pools

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// refusingConsoleProvider is a backend whose pool host has no console, as a
// pool agent running natively on the user's machine has none.
type refusingConsoleProvider struct {
	stubPoolProvider
}

func (refusingConsoleProvider) OpenConsole(context.Context, *model.SandboxProviderInstance, *model.Pool, sandbox.ConsoleOptions) (sandbox.PTY, error) {
	return nil, fmt.Errorf("this pool's host is the user's own machine: %w", sandbox.ErrPoolConsoleUnsupported)
}

// A backend with no console to open is a settled answer about the backend,
// served as 501 with its reason, not a failure to reach the pool.
func TestOpenPoolConsoleAnswersNotImplementedForABackendWithNoConsole(t *testing.T) {
	ctx := context.Background()
	appStore, _ := newPoolReconcilerTestStore(t)
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("hosted", refusingConsoleProvider{})
	instance := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "hosted", Name: "hosted"}
	if err := appStore.CreateSandboxProviderInstance(ctx, instance); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: instance.ID}}
	pool.DesiredState = model.DesiredStatePresent
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	svc := NewService(appStore, manager, NewControlPlane(appStore, nil))

	_, err := svc.OpenPoolConsole(ctx, "project-1", "pool-1", sandbox.ConsoleOptions{})
	if status := statusOf(err); status != http.StatusNotImplemented {
		t.Fatalf("OpenPoolConsole() status = %d (%v), want 501", status, err)
	}
	if !strings.Contains(err.Error(), "user's own machine") {
		t.Fatalf("OpenPoolConsole() error = %q, want the backend's reason", err)
	}
}
