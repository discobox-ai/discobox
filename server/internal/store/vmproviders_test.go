package store_test

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/encryption"
)

// Each vm provider becomes its driver's type, once, as ADR 26-11-11-080 §8
// says: a boxd row keeps its ID, its other fields and its key (sealed to
// sealed, as the driver's secret), and takes the endpoints the server's
// environment gave the old driver into its configuration, marked carried; a
// row of a driver this server does not link keeps its type and is disabled.
func TestMigrateVMProvidersMakesEachALinkedDriversType(t *testing.T) {
	ctx := context.Background()
	st, db := newTestStoreWithDB(t, true)
	rawProviderRow(t, db, "prov-boxd", "vm", `{"driver":"boxd","apiKey":"bxd_key","controlPlaneUrl":"https://discobox.example.com","base":"img","maxPools":2}`)
	rawProviderRow(t, db, "prov-plain", "vm", `{"driver":"boxd","apiKey":"bxd_other"}`)
	rawProviderRow(t, db, "prov-vz", "vm", `{"driver":"vz"}`)
	rawProviderRow(t, db, "prov-docker", "docker", `{"socketPath":"/var/run/docker.sock"}`)
	env := map[string]string{"BOXD_GRPC_ADDR": "boxd.internal:9443", "BOXD_TOKEN_URL": " https://boxd.internal/api/v1/auth/token "} //nolint:gosec // G101: endpoints, not credentials
	lookup := func(name string) (string, bool) { value, ok := env[name]; return value, ok }
	// The start's order: the clear key is sealed under vm first.
	if err := st.SealProviderCredentials(ctx, lookup); err != nil {
		t.Fatal(err)
	}
	if err := st.MigrateVMProviders(ctx, lookup); err != nil {
		t.Fatal(err)
	}

	boxd := providerRow(t, db, "prov-boxd")
	if boxd.Type != "boxd" || boxd.Disabled {
		t.Fatalf("prov-boxd is %q (disabled %v), want an enabled boxd provider", boxd.Type, boxd.Disabled)
	}
	got := configFields(t, boxd)
	driver, _ := got["driver"].(map[string]any)
	if driver["addr"] != "boxd.internal:9443" || driver["tokenUrl"] != "https://boxd.internal/api/v1/auth/token" || len(driver) != 2 {
		t.Errorf("prov-boxd driver = %v, want the environment's endpoints", got["driver"])
	}
	if got["controlPlaneUrl"] != "https://discobox.example.com" || got["base"] != "img" || got["maxPools"] != float64(2) || got["apiKey"] != nil {
		t.Errorf("prov-boxd config = %v, want every other field kept and no key", got)
	}
	if !slices.Equal(boxd.CarriedEndpoints, []string{"driver.addr", "driver.tokenUrl"}) {
		t.Errorf("prov-boxd carried = %v, want both endpoints", boxd.CarriedEndpoints)
	}
	if !encryption.Sealed(boxd.EncryptedConfig) || bytes.Contains(boxd.EncryptedConfig, []byte("bxd_key")) {
		t.Errorf("prov-boxd's key is not sealed: %q", boxd.EncryptedConfig)
	}
	if secrets := openedSecrets(t, st, boxd); len(secrets) != 1 || secrets["driver.apiKey"] != "bxd_key" {
		t.Errorf("prov-boxd sealed fields = %v, want the key as driver.apiKey", secrets)
	}

	vz := providerRow(t, db, "prov-vz")
	if vz.Type != "vm" || !vz.Disabled || configFields(t, vz)["driver"] != "vz" {
		t.Errorf("prov-vz = %q disabled %v config %s, want a disabled vm row as it was", vz.Type, vz.Disabled, vz.Config)
	}
	if docker := providerRow(t, db, "prov-docker"); docker.Type != "docker" || docker.Disabled {
		t.Errorf("a docker provider was touched: %+v", docker)
	}

	// Once: a second start finds no row typed vm to move but the disabled
	// one, which it leaves as it is, and nothing is read from the
	// environment again.
	env = map[string]string{"BOXD_GRPC_ADDR": "elsewhere:9443"}
	before := providerRow(t, db, "prov-plain")
	if err := st.MigrateVMProviders(ctx, lookup); err != nil {
		t.Fatal(err)
	}
	after := providerRow(t, db, "prov-plain")
	if after.Type != "boxd" || !after.UpdatedAt.Equal(before.UpdatedAt) || string(after.Config) != string(before.Config) {
		t.Errorf("prov-plain was moved again: %s %s, was %s", after.Type, after.Config, before.Config)
	}
	if again := providerRow(t, db, "prov-vz"); !again.UpdatedAt.Equal(vz.UpdatedAt) {
		t.Errorf("the disabled vm row was rewritten")
	}
}

// With no endpoint in the environment no driver object is written, and boxd
// takes the defaults it took before.
func TestMigrateVMProvidersCarriesNothingUnset(t *testing.T) {
	ctx := context.Background()
	st, db := newTestStoreWithDB(t, true)
	rawProviderRow(t, db, "prov-boxd", "vm", `{"driver":"boxd","apiKey":"bxd_key"}`)
	none := func(string) (string, bool) { return "", false }
	if err := st.SealProviderCredentials(ctx, none); err != nil {
		t.Fatal(err)
	}
	if err := st.MigrateVMProviders(ctx, none); err != nil {
		t.Fatal(err)
	}
	row := providerRow(t, db, "prov-boxd")
	if row.Type != "boxd" || len(row.Config) != 0 || len(row.CarriedEndpoints) != 0 {
		t.Fatalf("prov-boxd = %q config %s carried %v, want a boxd row with no configuration", row.Type, row.Config, row.CarriedEndpoints)
	}
	if secrets := openedSecrets(t, st, row); secrets["driver.apiKey"] != "bxd_key" {
		t.Fatalf("prov-boxd sealed fields = %v", secrets)
	}
}
