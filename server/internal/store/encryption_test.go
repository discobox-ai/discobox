package store_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/config"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/encryption"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func localKeyring(t *testing.T, db *database.DB, key string, retired ...string) *encryption.Keyring {
	t.Helper()
	keyring, err := encryption.New(context.Background(), config.EncryptionSettings{
		Provider: config.EncryptionLocal,
		Local:    config.LocalEncryptionSettings{Key: key, RetiredKeys: retired},
	}, store.NewEncryptionKeys(db.Write, db.CurrentRead()))
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

func newLocalKey(t *testing.T) string {
	t.Helper()
	key, err := encryption.GenerateLocalKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// legacySeal is how a release before data keys sealed a value with
// encryptionKey.
func legacySeal(t *testing.T, key string, f encryption.Field, plaintext []byte) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	out := append([]byte(encryption.LegacyPrefix), nonce...)
	return aead.Seal(out, nonce, plaintext, []byte(f.Purpose+"\x00"+f.ResourceID))
}

func rawColumn(t *testing.T, db *database.DB, table, column, where string, args ...any) []byte {
	t.Helper()
	var value []byte
	if err := db.Write.Raw("SELECT "+column+" FROM "+table+" WHERE "+where, args...).Row().Scan(&value); err != nil {
		t.Fatalf("read %s.%s: %v", table, column, err)
	}
	return value
}

// A server upgraded from encryptionKey, or given a provider for the first
// time, seals every stored value with its project's data key at startup,
// once: a second start finds nothing left to do.
func TestPrepareEncryptionResealsEveryStoredValueOnce(t *testing.T) {
	ctx := context.Background()
	unsealed, db := newTestStoreWithDB(t, false)
	createTestPool(t, unsealed, "project-1", "pool-1")
	secretValue := []byte(`{"token":"plain-token"}`)
	if err := unsealed.CreateSecret(ctx, &model.Secret{ID: "sec-plain", ProjectID: "project-1", Name: "plain", Type: model.SecretTypeToken, EncryptedValue: secretValue}); err != nil {
		t.Fatal(err)
	}
	if err := unsealed.CreateSandbox(ctx, &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "one", SecretState: []byte("state")}); err != nil {
		t.Fatal(err)
	}
	if err := unsealed.PutSandboxInventory(ctx, "project-1", "pool-1", "sandbox-1", "sha256:one", []byte(`{"screens":{}}`), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// A value an earlier release sealed with encryptionKey.
	oldKey := newLocalKey(t)
	legacyValue := []byte(`{"token":"legacy-token"}`)
	if err := db.Write.Create(&model.Secret{ID: "sec-legacy", ProjectID: "project-1", Name: "legacy", Type: model.SecretTypeToken,
		EncryptedValue: legacySeal(t, oldKey, encryption.SecretValue("project-1", "sec-legacy"), legacyValue)}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := unsealed.CreateProjectUserKeyIfMissing(ctx, &model.ProjectUserKey{ProjectID: "project-1", UserID: "user-1", PublicKey: "pub", KeyType: "ed25519",
		EncryptedPrivateKey: legacySeal(t, oldKey, encryption.SandboxAccessIssuerKey("project-1", "user-1"), []byte("private"))}); err != nil {
		t.Fatal(err)
	}

	report, err := unsealed.PrepareEncryption(ctx)
	if err == nil || !strings.Contains(err.Error(), "encryption.provider") {
		t.Fatalf("PrepareEncryption with no provider over legacy values = %+v, %v; want it refused, naming encryption.provider", report, err)
	}

	s := store.New(db.Write, db.Read, store.WithKeyring(localKeyring(t, db, oldKey)))
	report, err = s.PrepareEncryption(ctx)
	if err != nil {
		t.Fatalf("PrepareEncryption: %v", err)
	}
	if report.Resealed != 5 {
		t.Fatalf("Resealed = %d, want 5 (two secrets, a secret state, an inventory, an access key)", report.Resealed)
	}
	for _, raw := range [][]byte{
		rawColumn(t, db, "secrets", "encrypted_value", "id = ?", "sec-plain"),
		rawColumn(t, db, "secrets", "encrypted_value", "id = ?", "sec-legacy"),
		rawColumn(t, db, "sandboxes", "secret_state", "id = ?", "sandbox-1"),
		rawColumn(t, db, "sandbox_inventories", "document", "sandbox_id = ?", "sandbox-1"),
		rawColumn(t, db, "sandbox_access_issuer_keys", "encrypted_private_key", "user_id = ?", "user-1"),
	} {
		if !encryption.Sealed(raw) {
			t.Fatalf("a value is still %q after the re-seal", raw[:min(len(raw), 12)])
		}
	}
	for id, want := range map[string]string{"sec-plain": "plain-token", "sec-legacy": "legacy-token"} {
		secret, err := s.GetSecret(ctx, "project-1", id)
		if err != nil {
			t.Fatal(err)
		}
		value, err := s.OpenSecretValue(ctx, secret)
		if err != nil || value.Token != want {
			t.Fatalf("secret %s opened to %+v, %v; want %s", id, value, err, want)
		}
	}
	sandbox, err := s.GetSandbox(ctx, "project-1", "sandbox-1")
	if err != nil {
		t.Fatal(err)
	}
	if state, err := s.OpenSandboxSecretState(ctx, sandbox); err != nil || string(state) != "state" {
		t.Fatalf("secret state opened to %q, %v", state, err)
	}

	again, err := s.PrepareEncryption(ctx)
	if err != nil || again.Resealed != 0 || again.Rewrapped != 0 {
		t.Fatalf("a second start = %+v, %v; want nothing done", again, err)
	}
}

// Moving to a new local key rewraps every data key at startup without
// touching a sealed value, after which the old key is not needed.
func TestPrepareEncryptionRewrapsDataKeysOfARetiredKey(t *testing.T) {
	ctx := context.Background()
	_, db := newTestStoreWithDB(t, false)
	oldKey, newKey := newLocalKey(t), newLocalKey(t)
	before := store.New(db.Write, db.Read, store.WithKeyring(localKeyring(t, db, oldKey)))
	createTestPool(t, before, "project-1", "pool-1")
	if err := before.CreateSecret(ctx, &model.Secret{ID: "sec-1", ProjectID: "project-1", Name: "one", Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"t"}`)}); err != nil {
		t.Fatal(err)
	}
	sealed := rawColumn(t, db, "secrets", "encrypted_value", "id = ?", "sec-1")

	moving := store.New(db.Write, db.Read, store.WithKeyring(localKeyring(t, db, newKey, oldKey)))
	report, err := moving.PrepareEncryption(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Rewrapped != 1 || report.Resealed != 0 {
		t.Fatalf("report = %+v, want project-1's key rewrapped and nothing re-sealed", report)
	}
	if after := rawColumn(t, db, "secrets", "encrypted_value", "id = ?", "sec-1"); !bytes.Equal(after, sealed) {
		t.Fatal("a rewrap changed a sealed value")
	}

	moved := store.New(db.Write, db.Read, store.WithKeyring(localKeyring(t, db, newKey)))
	secret, err := moved.GetSecret(ctx, "project-1", "sec-1")
	if err != nil {
		t.Fatal(err)
	}
	if value, err := moved.OpenSecretValue(ctx, secret); err != nil || value.Token != "t" {
		t.Fatalf("after the move, without the old key: %+v, %v", value, err)
	}
}

// A project has its data key from the moment it is made, and loses it with
// the project.
func TestAProjectsDataKeyIsMadeWithItAndDestroyedWithIt(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, true)
	count := func(scope string) int64 {
		var n int64
		if err := db.Write.Model(&model.EncryptionKey{}).Where("scope = ?", scope).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := s.CreateProject(ctx, &model.Project{ID: "proj_new", OwnerUserID: "user-1", Name: "new"}); err != nil {
		t.Fatal(err)
	}
	if n := count("proj_new"); n != 1 {
		t.Fatalf("a new project has %d data keys, want 1", n)
	}
	if err := s.CreateSecret(ctx, &model.Secret{ID: "sec-1", ProjectID: "proj_new", Name: "one", Type: model.SecretTypeToken, EncryptedValue: []byte(`{"token":"t"}`)}); err != nil {
		t.Fatal(err)
	}
	secret, err := s.GetSecret(ctx, "proj_new", "sec-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, "proj_new"); err != nil {
		t.Fatal(err)
	}
	if n := count("proj_new"); n != 0 {
		t.Fatalf("a deleted project left %d data keys", n)
	}
	if _, err := s.OpenSecretValue(ctx, secret); err == nil {
		t.Fatal("a copy of a deleted project's secret still opens")
	}
}
