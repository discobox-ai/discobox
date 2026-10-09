package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

type catalog struct{ manager *sandbox.ProviderManager }

func (catalog) ListSandboxProviderCatalog() []SandboxProviderCatalogItem { return nil }
func (c catalog) SandboxProviderManager() *sandbox.ProviderManager       { return c.manager }

// newImmutableFixture is a provider instance of a type whose "driver" field is
// immutable and whose "size" field is not.
func newImmutableFixture(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	appStore := store.New(db.Write, db.Read)
	manager := sandbox.NewProviderManager()
	manager.RegisterProviderDefinition("machines", sandbox.ProviderDefinition{ConfigFields: []sandbox.ProviderConfigField{
		{Key: "driver", Label: "Driver", Type: "string", Immutable: true},
		{Key: "size", Label: "Size", Type: "string"},
	}})
	instance := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "machines", Name: "machines", Config: json.RawMessage(`{"driver":"boxd","size":"s"}`)}
	if err := appStore.CreateSandboxProviderInstance(ctx, instance); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return NewService(appStore, catalog{manager: manager}, nil), appStore
}

func update(svc *Service, config string) error {
	_, err := svc.UpdateSandboxProviderInstance(context.Background(), "project-1", "provider-1", services.UpdateSandboxProviderInstanceBody{Config: []byte(config)})
	return err
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
