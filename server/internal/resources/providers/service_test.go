package providers_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
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

// A field that decides what made a pool's host cannot change under the
// pools it made, and the refusal names it; everything else still can.
func TestUpdateRefusesAnImmutableFieldWhileTheProviderHasPools(t *testing.T) {
	svc, appStore := newImmutableFixture(t)
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: "provider-1"}}
	pool.DesiredState = model.DesiredStatePresent
	if err := appStore.CreatePool(context.Background(), pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	err := update(svc, `{"driver":"vz","size":"s"}`)
	var status apperrors.StatusError
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict || !strings.Contains(err.Error(), "driver") {
		t.Fatalf("changing driver with a pool = %v, want 409 naming driver", err)
	}
	if err := update(svc, `{"size":"s"}`); err == nil {
		t.Fatal("dropping driver with a pool was accepted; absent is a change too")
	}
	if err := update(svc, `{ "driver" : "boxd", "size": "m" }`); err != nil {
		t.Fatalf("changing only a mutable field, with the immutable one reformatted, = %v", err)
	}
}

func TestUpdateAllowsAnImmutableFieldOnceThereAreNoPools(t *testing.T) {
	svc, _ := newImmutableFixture(t)
	if err := update(svc, `{"driver":"vz","size":"s"}`); err != nil {
		t.Fatalf("changing driver with no pools = %v", err)
	}
}

// newImmutableFixture is a provider instance of a type whose "driver" field is
// immutable and whose "size" field is not.
func newImmutableFixture(t *testing.T) (*resourceproviders.Service, *store.Store) {
	t.Helper()
	svc, appStore, manager := newTestService(t)
	// A definition alone does not make a type resolvable, and an update
	// refuses a type that is not; nothing here resolves the instance.
	manager.RegisterFactory("machines", func(context.Context, *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return nil, errors.New("not resolved in this test")
	})
	manager.RegisterProviderDefinition("machines", sandbox.ProviderDefinition{ConfigFields: []sandbox.ProviderConfigField{
		{Key: "driver", Label: "Driver", Type: "string", Immutable: true},
		{Key: "size", Label: "Size", Type: "string"},
	}})
	instance := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "machines", Name: "machines", Config: model.RawJSON(`{"driver":"boxd","size":"s"}`)}
	if err := appStore.CreateSandboxProviderInstance(context.Background(), instance); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return svc, appStore
}

func update(svc *resourceproviders.Service, config string) error {
	_, err := svc.UpdateSandboxProviderInstance(context.Background(), "project-1", "provider-1", services.UpdateSandboxProviderInstanceBody{Config: []byte(config)})
	return err
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
