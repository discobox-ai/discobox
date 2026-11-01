package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/secrets"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func newSealer(t *testing.T) secrets.Sealer {
	t.Helper()
	key, err := secrets.GenerateBase64Key()
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewAESGCMSealerFromBase64Key(key)
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}

// rawProviderRow writes a provider instance row as a server from before ADR
// 26-10-31-694 did: its config as given, with nothing sealed.
func rawProviderRow(t *testing.T, db *database.DB, id, providerType, config string) {
	t.Helper()
	if err := db.Write.Create(&model.SandboxProviderInstance{ID: id, ProjectID: "project-1", Type: providerType, Name: id, Config: model.RawJSON(config)}).Error; err != nil {
		t.Fatalf("write provider row %s: %v", id, err)
	}
}

func providerRow(t *testing.T, db *database.DB, id string) model.SandboxProviderInstance {
	t.Helper()
	var row model.SandboxProviderInstance
	if err := db.Read.Where("id = ?", id).First(&row).Error; err != nil {
		t.Fatalf("read provider row %s: %v", id, err)
	}
	return row
}

func openedSecrets(t *testing.T, st *store.Store, row model.SandboxProviderInstance) map[string]string {
	t.Helper()
	sealed, err := st.OpenSandboxProviderSecrets(context.Background(), &row)
	if err != nil {
		t.Fatalf("open provider %s secrets: %v", row.ID, err)
	}
	out := map[string]string{}
	for key, raw := range sealed {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("decode %s: %v", key, err)
		}
		out[key] = value
	}
	return out
}

func configFields(t *testing.T, row model.SandboxProviderInstance) map[string]any {
	t.Helper()
	fields := map[string]any{}
	if len(row.Config) > 0 {
		if err := json.Unmarshal(row.Config, &fields); err != nil {
			t.Fatalf("decode config of %s: %v", row.ID, err)
		}
	}
	return fields
}

// Every credential a provider kept in its config moves into the sealed
// column, an environment reference is resolved once and removed, and the row
// keeps its ID and the rest of its config (ADR 26-10-31-694 §4).
func TestSealProviderCredentialsMovesStoredKeysIntoTheSealedColumn(t *testing.T) {
	ctx := context.Background()
	st, db := newTestStoreWithDB(t, newSealer(t))
	const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n"
	rawProviderRow(t, db, "prov-vm", "vm", `{"driver":"boxd","apiKey":"bxd_key","deliverProviderKey":true}`)
	rawProviderRow(t, db, "prov-do", "digitalocean", `{"tokenEnv":"DO_TOKEN_FOR_TEST","sshPrivateKey":`+string(mustJSON(t, pem))+`,"region":"sfo3"}`)
	rawProviderRow(t, db, "prov-exec", "exec", `{"command":"vm.sh","sshPrivateKeyEnv":"UNSET_KEY_FOR_TEST"}`)
	rawProviderRow(t, db, "prov-docker", "docker", `{"socketPath":"/var/run/docker.sock"}`)
	before := providerRow(t, db, "prov-docker")

	env := map[string]string{"DO_TOKEN_FOR_TEST": "do-token\n"}
	lookup := func(name string) (string, bool) { value, ok := env[name]; return value, ok }
	if err := st.SealProviderCredentials(ctx, lookup); err != nil {
		t.Fatalf("seal provider credentials: %v", err)
	}

	vm := providerRow(t, db, "prov-vm")
	if got := configFields(t, vm); got["apiKey"] != nil || got["driver"] != "boxd" || got["deliverProviderKey"] != true {
		t.Errorf("vm config = %v, want the key gone and the rest kept", got)
	}
	if !secrets.IsSealed(vm.EncryptedConfig) || bytes.Contains(vm.EncryptedConfig, []byte("bxd_key")) {
		t.Errorf("vm encrypted_config is not sealed: %q", vm.EncryptedConfig)
	}
	if got := openedSecrets(t, st, vm); got["apiKey"] != "bxd_key" {
		t.Errorf("vm sealed fields = %v, want apiKey bxd_key", got)
	}

	do := providerRow(t, db, "prov-do")
	if got := configFields(t, do); got["tokenEnv"] != nil || got["sshPrivateKey"] != nil || got["region"] != "sfo3" {
		t.Errorf("digitalocean config = %v, want the key and the reference gone and the rest kept", got)
	}
	// A value moves byte for byte; one read from the environment is trimmed,
	// as the provider read it.
	if got := openedSecrets(t, st, do); got["token"] != "do-token" || got["sshPrivateKey"] != pem {
		t.Errorf("digitalocean sealed fields = %q, want the token from the environment and the key as stored", got)
	}

	// A reference to a variable this process does not have is removed, and
	// the provider is left without the key, to fail naming the field to set.
	exec := providerRow(t, db, "prov-exec")
	if got := configFields(t, exec); got["sshPrivateKeyEnv"] != nil || got["command"] != "vm.sh" {
		t.Errorf("exec config = %v, want the reference gone", got)
	}
	if got := openedSecrets(t, st, exec); len(got) != 0 {
		t.Errorf("exec sealed fields = %v, want none", got)
	}

	// A provider type with no credentials is not written.
	if after := providerRow(t, db, "prov-docker"); !after.UpdatedAt.Equal(before.UpdatedAt) || string(after.Config) != string(before.Config) {
		t.Errorf("docker row was rewritten: %s at %v, was %s at %v", after.Config, after.UpdatedAt, before.Config, before.UpdatedAt)
	}

	// Run again, as at every start, it leaves a moved row alone.
	vmUpdated := vm.UpdatedAt
	if err := st.SealProviderCredentials(ctx, lookup); err != nil {
		t.Fatalf("seal provider credentials again: %v", err)
	}
	if again := providerRow(t, db, "prov-vm"); !again.UpdatedAt.Equal(vmUpdated) || !bytes.Equal(again.EncryptedConfig, vm.EncryptedConfig) {
		t.Error("a second run rewrote a row it had already moved")
	}
}

// A value an older server wrote in the clear beside one already sealed
// replaces it: it is the newer of the two.
func TestSealProviderCredentialsMergesOverWhatIsSealed(t *testing.T) {
	ctx := context.Background()
	st, db := newTestStoreWithDB(t, newSealer(t))
	instance := &model.SandboxProviderInstance{ProjectID: "project-1", Type: "digitalocean", Name: "do", Config: model.RawJSON(`{"region":"sfo3"}`)}
	if err := store.SetSandboxProviderSecrets(instance, map[string]json.RawMessage{"token": mustJSON(t, "old"), "sshPrivateKey": mustJSON(t, "key")}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSandboxProviderInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if err := db.Write.Model(&model.SandboxProviderInstance{}).Where("id = ?", instance.ID).Update("config", `{"region":"sfo3","token":"new"}`).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.SealProviderCredentials(ctx, func(string) (string, bool) { return "", false }); err != nil {
		t.Fatal(err)
	}
	row := providerRow(t, db, instance.ID)
	if got := openedSecrets(t, st, row); got["token"] != "new" || got["sshPrivateKey"] != "key" {
		t.Errorf("sealed fields = %v, want token new and sshPrivateKey kept", got)
	}
	if got := configFields(t, row); got["token"] != nil {
		t.Errorf("config still holds the token: %v", got)
	}
}

// With no sealer configured, the secret fields are kept as a secret's value
// is: in the clear, and opened all the same.
func TestProviderSecretsWithoutASealerAreKeptInTheClear(t *testing.T) {
	ctx := context.Background()
	st, db := newTestStoreWithDB(t, nil)
	instance := &model.SandboxProviderInstance{ProjectID: "project-1", Type: "vm", Name: "vm"}
	if err := store.SetSandboxProviderSecrets(instance, map[string]json.RawMessage{"apiKey": mustJSON(t, "k")}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSandboxProviderInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	row := providerRow(t, db, instance.ID)
	if secrets.IsSealed(row.EncryptedConfig) {
		t.Error("sealed with no sealer")
	}
	if got := openedSecrets(t, st, row); got["apiKey"] != "k" {
		t.Errorf("opened %v, want apiKey k", got)
	}
}

func mustJSON(t *testing.T, value string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
