package providers_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourceproviders "github.com/discobox-ai/discobox/server/internal/resources/providers"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A create naming a provider type the server does not know, or no type at
// all, is the caller's mistake: it answers 400 and leaves nothing behind, so
// the next server start still resolves every persisted instance.
func TestCreateSandboxProviderInstanceRejectsUnknownTypeWithoutPersisting(t *testing.T) {
	for _, providerType := range []string{"no-such-type", "", "   "} {
		t.Run(providerType, func(t *testing.T) {
			ctx := context.Background()
			svc, st, _ := newTestService(t)

			_, err := svc.CreateSandboxProviderInstance(ctx, "project-1", services.CreateSandboxProviderInstanceBody{
				Type:   providerType,
				Name:   "bad",
				Config: []byte(`{}`),
			})
			if err == nil {
				t.Fatal("create succeeded, want an error")
			}
			var statusErr interface{ StatusCode() int }
			if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest {
				t.Errorf("create error = %v (no 400 status), want a 400", err)
			}

			rows, err := st.ListSandboxProviderInstances(ctx, "project-1")
			if err != nil {
				t.Fatalf("list instances: %v", err)
			}
			if len(rows) != 0 {
				t.Errorf("rows persisted after failed create = %d, want 0", len(rows))
			}
			if err := svc.EnsureExistingSandboxProviderInstances(ctx); err != nil {
				t.Errorf("startup reconciliation after failed create: %v", err)
			}
		})
	}
}

// A known type whose backend cannot come up fails the create as a 502, and the
// row it wrote is gone again.
func TestCreateSandboxProviderInstanceRemovesInstanceThatDoesNotResolve(t *testing.T) {
	ctx := context.Background()
	svc, st, manager := newTestService(t)
	manager.RegisterFactory("remote", func(context.Context, *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return nil, errors.New("host unreachable")
	})

	_, err := svc.CreateSandboxProviderInstance(ctx, "project-1", services.CreateSandboxProviderInstanceBody{
		Type:   "remote",
		Name:   "remote",
		Config: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("create succeeded, want the resolve error")
	}
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadGateway {
		t.Errorf("create error = %v (no 502 status), want a 502", err)
	}

	rows, err := st.ListSandboxProviderInstances(ctx, "project-1")
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows persisted after failed resolve = %d, want 0", len(rows))
	}
}

type catalog struct{ manager *sandbox.ProviderManager }

func (c catalog) ListSandboxProviderCatalog() []resourceproviders.SandboxProviderCatalogItem {
	return nil
}

func (c catalog) SandboxProviderManager() *sandbox.ProviderManager { return c.manager }

func newTestService(t *testing.T) (*resourceproviders.Service, *store.Store, *sandbox.ProviderManager) {
	t.Helper()

	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID:          "project-1",
		OwnerUserID: "user-1",
		Name:        "Project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	st := store.New(db.Write, db.Read)
	manager := sandbox.NewProviderManager()
	return resourceproviders.NewService(st, catalog{manager: manager}, nil), st, manager
}
