package providers_test

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
	resourceproviders "github.com/discobox-ai/discobox/server/internal/resources/providers"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/providers/digitalocean"
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
	manager := sandbox.NewProviderManager(st)
	return resourceproviders.NewService(st, catalog{manager: manager}, nil), st, manager
}

// newKeyedFixture is a provider type with a secret field, whose factory
// records the config it is built from.
func newKeyedFixture(t *testing.T) (*resourceproviders.Service, *store.Store, *[]string) {
	t.Helper()
	svc, appStore, manager := newTestService(t)
	var built []string
	manager.RegisterFactory("keyed", func(_ context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		built = append(built, string(instance.Config))
		return nil, errors.New("not resolved in this test")
	})
	manager.RegisterProviderDefinition("keyed", sandbox.ProviderDefinition{ConfigFields: []sandbox.ProviderConfigField{
		{Key: "driver", Label: "Driver", Type: "string"},
		{Key: "apiKey", Label: "API Key", Type: sandbox.ProviderConfigFieldSecret},
	}})
	instance := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "keyed", Name: "keyed", Config: model.RawJSON(`{"driver":"boxd"}`)}
	if err := store.SetSandboxProviderSecrets(instance, map[string]json.RawMessage{"apiKey": json.RawMessage(`"key-1"`)}); err != nil {
		t.Fatal(err)
	}
	if err := appStore.CreateSandboxProviderInstance(context.Background(), instance); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return svc, appStore, &built
}

func sealedKeys(t *testing.T, appStore *store.Store) map[string]string {
	t.Helper()
	row, err := appStore.GetSandboxProviderInstance(context.Background(), "project-1", "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(row.Config), "apiKey") {
		t.Errorf("config column holds the key: %s", row.Config)
	}
	sealed, err := appStore.OpenSandboxProviderSecrets(context.Background(), row)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for key, raw := range sealed {
		var value string
		_ = json.Unmarshal(raw, &value)
		out[key] = value
	}
	return out
}

// A read answers with the config and the names of the secret fields that are
// set, never a value (ADR 26-10-31-694 §2).
func TestReadsNameTheSetSecretFieldsAndReturnNoValue(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newKeyedFixture(t)

	got, err := svc.GetSandboxProviderInstance(ctx, "project-1", "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := svc.ListSandboxProviderInstances(ctx, "project-1")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %v, %v", listed, err)
	}
	for name, instance := range map[string]*model.SandboxProviderInstance{"get": got, "list": &listed[0]} {
		body, err := json.Marshal(instance)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "key-1") {
			t.Errorf("%s answer carries the key: %s", name, body)
		}
		if strings.Join(instance.SecretFields, ",") != "apiKey" || string(instance.Config) != `{"driver":"boxd"}` {
			t.Errorf("%s answer = config %s, secretFields %v; want the config alone and [apiKey]", name, instance.Config, instance.SecretFields)
		}
	}
}

// An update leaving a secret field out keeps its sealed value, a value
// replaces it, and null clears it; the rest of the config is replaced as
// given (ADR 26-10-31-694 §2).
func TestUpdateKeepsReplacesAndClearsASecretField(t *testing.T) {
	ctx := context.Background()
	svc, appStore, _ := newKeyedFixture(t)

	if err := update(svc, `{"driver":"vz"}`); err != nil {
		t.Fatalf("update without the key: %v", err)
	}
	if got := sealedKeys(t, appStore); got["apiKey"] != "key-1" {
		t.Errorf("after an update without it the key is %q, want it kept", got["apiKey"])
	}
	updated, err := svc.UpdateSandboxProviderInstance(ctx, "project-1", "provider-1", services.UpdateSandboxProviderInstanceBody{Config: []byte(`{"driver":"vz","apiKey":"key-2"}`)})
	if err != nil {
		t.Fatalf("update with a new key: %v", err)
	}
	if strings.Contains(string(updated.Config), "key-2") || strings.Join(updated.SecretFields, ",") != "apiKey" {
		t.Errorf("update answered config %s, secretFields %v", updated.Config, updated.SecretFields)
	}
	if got := sealedKeys(t, appStore); got["apiKey"] != "key-2" {
		t.Errorf("after an update with a new key it is %q, want key-2", got["apiKey"])
	}
	if err := update(svc, `{"driver":"vz","apiKey":null}`); err != nil {
		t.Fatalf("update clearing the key: %v", err)
	}
	if got := sealedKeys(t, appStore); len(got) != 0 {
		t.Errorf("after clearing it the sealed fields are %v, want none", got)
	}
}

// A create's secret field is sealed apart from its config, and the provider
// is built from the two joined.
func TestCreateSealsTheSecretFieldAndBuildsTheProviderWithIt(t *testing.T) {
	ctx := context.Background()
	svc, _, built := newKeyedFixture(t)
	_, err := svc.CreateSandboxProviderInstance(ctx, "project-1", services.CreateSandboxProviderInstanceBody{Type: "keyed", Name: "second", Config: []byte(`{"driver":"boxd","apiKey":"key-9"}`)})
	if err == nil {
		t.Fatal("create resolved a provider the fixture does not build")
	}
	if len(*built) != 1 || (*built)[0] != `{"apiKey":"key-9","driver":"boxd"}` {
		t.Errorf("provider built from %v, want the config with its key joined in", *built)
	}
}

// An environment reference is refused by name rather than ignored (ADR
// 26-10-31-694 §6).
func TestCreateRefusesAnEnvironmentReference(t *testing.T) {
	ctx := context.Background()
	svc, _, manager := newTestService(t)
	manager.RegisterFactory("digitalocean", func(context.Context, *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return nil, errors.New("not resolved in this test")
	})
	manager.RegisterProviderConfigValidator("digitalocean", digitalocean.Validate)
	_, err := svc.CreateSandboxProviderInstance(ctx, "project-1", services.CreateSandboxProviderInstanceBody{Type: "digitalocean", Name: "do", Config: []byte(`{"tokenEnv":"DIGITALOCEAN_ACCESS_TOKEN","sshPrivateKey":"k","sshKeys":"1"}`)})
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), "tokenEnv") {
		t.Fatalf("create = %v, want a 400 naming tokenEnv", err)
	}
}

// A read never answers with a secret field's value, even one still in the
// stored config — a row an older server wrote, or one the startup migration
// could not move — and one row whose sealed fields do not open does not take
// the project's listing down with it.
func TestReadsHideAnUnsealedKeyAndSurviveARowThatDoesNotOpen(t *testing.T) {
	ctx := context.Background()
	svc, appStore, _ := newKeyedFixture(t)
	unsealed := &model.SandboxProviderInstance{ID: "provider-2", ProjectID: "project-1", Type: "keyed", Name: "unsealed", Config: model.RawJSON(`{"driver":"boxd","apiKey":"plain-key"}`)}
	if err := appStore.CreateSandboxProviderInstance(ctx, unsealed); err != nil {
		t.Fatal(err)
	}
	broken := &model.SandboxProviderInstance{ID: "provider-3", ProjectID: "project-1", Type: "keyed", Name: "broken", EncryptedConfig: []byte("not json")}
	if err := appStore.CreateSandboxProviderInstance(ctx, broken); err != nil {
		t.Fatal(err)
	}

	listed, err := svc.ListSandboxProviderInstances(ctx, "project-1")
	if err != nil {
		t.Fatalf("list with a row that does not open: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed %d providers, want 3", len(listed))
	}
	got, err := svc.GetSandboxProviderInstance(ctx, "project-1", "provider-2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got.Config), "plain-key") || string(got.Config) != `{"driver":"boxd"}` {
		t.Errorf("get answered config %s, want the unsealed key taken out", got.Config)
	}
	for _, instance := range listed {
		if strings.Contains(string(instance.Config), "plain-key") {
			t.Errorf("list answered %s with the unsealed key", instance.ID)
		}
	}
}

// Only null clears a secret field; an empty string is a value like any other
// (ADR 26-10-31-694 §2).
func TestUpdateClearsASecretFieldOnlyWithNull(t *testing.T) {
	svc, appStore, _ := newKeyedFixture(t)
	if err := update(svc, `{"driver":"boxd","apiKey":""}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, ok := sealedKeys(t, appStore)["apiKey"]; !ok || got != "" {
		t.Errorf("after an empty value the sealed key is %q (set %v), want an empty value kept", got, ok)
	}
}
