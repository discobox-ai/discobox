package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourceproviders "github.com/discobox-ai/discobox/server/internal/resources/providers"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// newDriverFixture is a provider type shaped as a disco-vm driver's (ADR
// 26-11-11-080 §2): its secret and fields under driver, one of them a
// Privileged endpoint, every one fixed while the provider has pools. Its
// factory records the config it is built from. The operator allows
// boxd.internal.
func newDriverFixture(t *testing.T) (*resourceproviders.Service, *store.Store, *[]string) {
	t.Helper()
	svc, appStore, manager := newTestService(t)
	manager.SetEndpointPolicy(sandbox.EndpointPolicy{Allowed: []string{"boxd.internal"}})
	var built []string
	manager.RegisterFactory("driverish", func(_ context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		built = append(built, string(instance.Config))
		return nil, errors.New("not resolved in this test")
	})
	manager.RegisterProviderDefinition("driverish", sandbox.ProviderDefinition{ConfigFields: []sandbox.ProviderConfigField{
		{Key: "driver.apiKey", Label: "API Key", Type: sandbox.ProviderConfigFieldSecret, Required: true},
		{Key: "driver.tokenUrl", Label: "Token URL", Type: "string", Immutable: true, Privileged: true},
		{Key: "driver.region", Label: "Region", Type: "string", Immutable: true},
		{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string"},
	}})
	instance := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "driverish", Name: "driverish",
		Config:           model.RawJSON(`{"driver":{"tokenUrl":"https://boxd.internal/token","region":"fra"}}`),
		CarriedEndpoints: []string{"driver.tokenUrl"},
	}
	if err := store.SetSandboxProviderSecrets(instance, map[string]json.RawMessage{"driver.apiKey": json.RawMessage(`"key-1"`)}); err != nil {
		t.Fatal(err)
	}
	if err := appStore.CreateSandboxProviderInstance(context.Background(), instance); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return svc, appStore, &built
}

func statusOf(err error) int {
	var status apperrors.StatusError
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 0
}

// A secret under driver is sealed apart from the config, kept by an update
// that leaves it out, joined back at its place for the provider, and never
// answered.
func TestADriversSecretIsSealedAtItsPlace(t *testing.T) {
	ctx := context.Background()
	svc, appStore, built := newDriverFixture(t)
	if err := update(svc, `{"driver":{"tokenUrl":"https://boxd.internal/token","region":"fra"},"controlPlaneUrl":"https://d.example.com"}`); err != nil {
		t.Fatalf("an update leaving the key out: %v", err)
	}
	row, err := appStore.GetSandboxProviderInstance(ctx, "project-1", "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(row.Config), "key-1") || strings.Contains(string(row.Config), "apiKey") {
		t.Fatalf("config column holds the key: %s", row.Config)
	}
	if got := sealedKeys(t, appStore); got["driver.apiKey"] != "key-1" || len(got) != 1 {
		t.Fatalf("sealed fields = %v, want driver.apiKey kept", got)
	}
	got, err := svc.GetSandboxProviderInstance(ctx, "project-1", "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.SecretFields, []string{"driver.apiKey"}) || strings.Contains(string(got.Config), "key-1") {
		t.Fatalf("read = config %s, secretFields %v", got.Config, got.SecretFields)
	}
	_, _ = svc.CreateSandboxProviderInstance(ctx, "project-1", services.CreateSandboxProviderInstanceBody{Type: "driverish", Name: "second", Config: []byte(`{"driver":{"apiKey":"key-9","region":"nyc"}}`)})
	if len(*built) != 1 || (*built)[0] != `{"driver":{"apiKey":"key-9","region":"nyc"}}` {
		t.Fatalf("provider built from %v, want the key joined inside driver", *built)
	}
	if err := update(svc, `{"driver":{"tokenUrl":"https://boxd.internal/token","region":"fra","apiKey":null}}`); err != nil {
		t.Fatalf("an update clearing the key: %v", err)
	}
	if got := sealedKeys(t, appStore); len(got) != 0 {
		t.Fatalf("sealed fields after a clear = %v", got)
	}
}

// A field under driver is fixed while the provider has pools, as a
// top-level one is (ADR 26-11-11-080 §5).
func TestADriverFieldIsFixedWhileTheProviderHasPools(t *testing.T) {
	svc, appStore, _ := newDriverFixture(t)
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: "provider-1"}}
	pool.DesiredState = model.DesiredStatePresent
	if err := appStore.CreatePool(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	err := update(svc, `{"driver":{"tokenUrl":"https://boxd.internal/token","region":"nyc"}}`)
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "driver.region") {
		t.Fatalf("changing driver.region with a pool = %v, want 409 naming it", err)
	}
	if err := update(svc, `{"driver":{"tokenUrl":"https://boxd.internal/token","region":"fra"},"controlPlaneUrl":"https://d.example.com"}`); err != nil {
		t.Fatalf("changing a field outside driver with a pool = %v", err)
	}
}

// A write that sets or changes a Privileged endpoint carries every secret,
// and names an endpoint the server's policy allows; one that leaves it as it
// is does neither (ADR 26-11-11-080 §§3–4). Writing a carried endpoint drops
// its mark.
func TestAPrivilegedEndpointNeedsTheKeyAndTheEndpointPolicy(t *testing.T) {
	ctx := context.Background()
	svc, appStore, _ := newDriverFixture(t)
	for _, tc := range []struct {
		config string
		status int
		want   string
	}{
		{`{"driver":{"tokenUrl":"https://203.0.113.9/token","region":"fra"}}`, http.StatusBadRequest, "driver.apiKey"},
		{`{"driver":{"region":"fra"}}`, http.StatusBadRequest, "driver.apiKey"},
		{`{"driver":{"tokenUrl":"http://203.0.113.9/token","region":"fra","apiKey":"key-2"}}`, http.StatusBadRequest, "https"},
		{`{"driver":{"tokenUrl":"https://10.0.0.5/token","region":"fra","apiKey":"key-2"}}`, http.StatusBadRequest, "not a public address"},
		{`{"driver":{"tokenUrl":"https://127.0.0.1:8443/token","region":"fra","apiKey":"key-2"}}`, http.StatusBadRequest, "not a public address"},
		{`{"driver":{"tokenUrl":"https://boxd.internal/token","region":"fra"},"controlPlaneUrl":"https://d.example.com"}`, 0, ""},
	} {
		err := update(svc, tc.config)
		if statusOf(err) != tc.status || tc.want != "" && !strings.Contains(err.Error(), tc.want) || tc.status == 0 && err != nil {
			t.Errorf("update(%s) = %v, want %d naming %q", tc.config, err, tc.status, tc.want)
		}
	}
	if got := sealedKeys(t, appStore); got["driver.apiKey"] != "key-1" {
		t.Fatalf("a refused write changed the key: %v", got)
	}
	row, _ := appStore.GetSandboxProviderInstance(ctx, "project-1", "provider-1")
	if !slices.Equal(row.CarriedEndpoints, []string{"driver.tokenUrl"}) {
		t.Fatalf("an unchanged carried endpoint lost its mark: %v", row.CarriedEndpoints)
	}
	if err := update(svc, `{"driver":{"tokenUrl":"https://203.0.113.9/token","region":"fra","apiKey":"key-2"}}`); err != nil {
		t.Fatalf("a public endpoint written with the key = %v", err)
	}
	row, _ = appStore.GetSandboxProviderInstance(ctx, "project-1", "provider-1")
	if len(row.CarriedEndpoints) != 0 || sealedKeys(t, appStore)["driver.apiKey"] != "key-2" {
		t.Fatalf("after writing the endpoint: carried %v, sealed %v; want no mark and the new key", row.CarriedEndpoints, sealedKeys(t, appStore))
	}
	// A create sets the endpoint, so it carries the key too, and is checked
	// as any write.
	_, err := svc.CreateSandboxProviderInstance(ctx, "project-1", services.CreateSandboxProviderInstanceBody{Type: "driverish", Name: "third", Config: []byte(`{"driver":{"apiKey":"k","tokenUrl":"https://192.168.1.1/t"}}`)})
	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("a create naming a private endpoint = %v, want 400", err)
	}
}
