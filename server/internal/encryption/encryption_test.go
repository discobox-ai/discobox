package encryption

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/envelope/pkg/envelope"
	"github.com/discobox-ai/envelope/pkg/envelopetest"

	"github.com/discobox-ai/discobox/server/internal/config"
)

func localSettings(t *testing.T) (config.EncryptionSettings, []byte) {
	t.Helper()
	encoded, err := GenerateLocalKey()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := base64.StdEncoding.DecodeString(encoded)
	return config.EncryptionSettings{
		Provider: config.EncryptionLocal,
		Local:    config.LocalEncryptionSettings{Key: encoded},
	}, key
}

func newKeyring(t *testing.T, cfg config.EncryptionSettings, keys envelope.KeyStore) *Keyring {
	t.Helper()
	k, err := New(context.Background(), cfg, keys)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return k
}

// legacySeal is how an earlier release sealed a value with encryptionKey.
func legacySeal(t *testing.T, key []byte, f Field, plaintext []byte) []byte {
	t.Helper()
	aead, err := newLegacyAEAD(key)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	out := append([]byte(LegacyPrefix), nonce...)
	return aead.Seal(out, nonce, plaintext, f.legacyAAD())
}

func TestNoProviderIsANilKeyringThatStoresValuesUnsealed(t *testing.T) {
	k := newKeyring(t, config.EncryptionSettings{}, &envelopetest.MemoryStore{})
	if k != nil {
		t.Fatal("New with no provider returned a keyring")
	}
	f := SecretValue("proj_a", "sec_1")
	sealed, err := k.SealForWrite(context.Background(), f, []byte("v"))
	if err != nil || string(sealed) != "v" {
		t.Fatalf("SealForWrite = %q, %v; want the value as it is", sealed, err)
	}
	if _, err := k.Open(context.Background(), f, []byte(LegacyPrefix+"xxxxxxxxxxxxxxxxxxxxxxxxxxxxx")); !errors.Is(err, ErrLegacyKeyMissing) {
		t.Fatalf("Open of a legacy value with no keyring = %v, want ErrLegacyKeyMissing", err)
	}
}

func TestAValueOpensOnlyAsItsOwnField(t *testing.T) {
	ctx := context.Background()
	cfg, _ := localSettings(t)
	k := newKeyring(t, cfg, &envelopetest.MemoryStore{})
	f := SecretValue("proj_a", "sec_1")
	sealed, err := k.Seal(ctx, f, []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	if !Sealed(sealed) || bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatalf("Seal = %q", sealed)
	}
	if got, err := k.Open(ctx, f, sealed); err != nil || string(got) != "hunter2" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	for _, other := range []Field{
		SecretValue("proj_a", "sec_2"),
		SandboxSecretState("proj_a", "sec_1"),
		SecretValue("proj_b", "sec_1"),
	} {
		if _, err := k.Open(ctx, other, sealed); err == nil {
			t.Fatalf("a value sealed for %+v opened as %+v", f, other)
		}
		if _, err := k.SealForWrite(ctx, other, sealed); err == nil {
			t.Fatalf("SealForWrite kept a value sealed for %+v as %+v", f, other)
		}
	}
	kept, err := k.SealForWrite(ctx, f, sealed)
	if err != nil || !bytes.Equal(kept, sealed) {
		t.Fatalf("SealForWrite of its own sealed value = %v, want it kept", err)
	}
}

// A value an earlier release sealed with encryptionKey opens with that key as
// the local key, or as a retired one, and is sealed again on its next write.
func TestALegacyValueOpensWithTheLocalKeyAndIsSealedAgain(t *testing.T) {
	ctx := context.Background()
	cfg, oldKey := localSettings(t)
	f := SandboxAccessIssuerKey("proj_a", "user-1")
	legacy := legacySeal(t, oldKey, f, []byte("private"))

	k := newKeyring(t, cfg, &envelopetest.MemoryStore{})
	if got, err := k.Open(ctx, f, legacy); err != nil || string(got) != "private" {
		t.Fatalf("Open legacy = %q, %v", got, err)
	}
	if _, err := k.Open(ctx, SandboxAccessIssuerKey("proj_a", "user-2"), legacy); err == nil {
		t.Fatal("a legacy value opened as another field")
	}
	resealed, err := k.SealForWrite(ctx, f, legacy)
	if err != nil || !Sealed(resealed) {
		t.Fatalf("SealForWrite of a legacy value = %q, %v; want it sealed again", resealed, err)
	}

	rotated, _ := localSettings(t)
	rotated.Local.RetiredKeys = []string{cfg.Local.Key}
	k = newKeyring(t, rotated, &envelopetest.MemoryStore{})
	if got, err := k.Open(ctx, f, legacy); err != nil || string(got) != "private" {
		t.Fatalf("Open legacy with its key retired = %q, %v", got, err)
	}

	other, _ := localSettings(t)
	k = newKeyring(t, other, &envelopetest.MemoryStore{})
	if _, err := k.Open(ctx, f, legacy); !errors.Is(err, ErrLegacyKeyMissing) {
		t.Fatalf("Open legacy with another key = %v, want ErrLegacyKeyMissing", err)
	}
}

// A retired local key still unwraps, and still opens what an earlier release
// sealed with it, until a rewrap moves its data keys to the new key.
func TestARetiredKeyUnwrapsUntilItsKeysAreRewrapped(t *testing.T) {
	ctx := context.Background()
	keys := &envelopetest.MemoryStore{}
	oldCfg, oldKey := localSettings(t)
	old := newKeyring(t, oldCfg, keys)
	f := SecretValue("proj_a", "sec_1")
	sealed, err := old.Seal(ctx, f, []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacySeal(t, oldKey, f, []byte("old"))

	newCfg, _ := localSettings(t)
	newCfg.Local.RetiredKeys = []string{oldCfg.Local.Key}
	moved := newKeyring(t, newCfg, keys)
	if got, err := moved.Open(ctx, f, sealed); err != nil || string(got) != "v" {
		t.Fatalf("Open after the move = %q, %v", got, err)
	}
	if got, err := moved.Open(ctx, f, legacy); err != nil || string(got) != "old" {
		t.Fatalf("Open legacy after the move = %q, %v", got, err)
	}
	if n, err := moved.Rewrap(ctx, "proj_a"); err != nil || n != 1 {
		t.Fatalf("Rewrap = %d, %v; want 1", n, err)
	}
	stored, _ := keys.List(ctx, "proj_a")
	if _, keyID := moved.Provider(); stored[0].ProviderKeyID != keyID {
		t.Fatalf("the data key is wrapped by %s, want %s", stored[0].ProviderKeyID, keyID)
	}
}

func TestNewRefusesAnUnusableLocalKey(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte(base64.StdEncoding.EncodeToString([]byte("short"))), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string]string{
		"not base64":   "not base64!",
		"short":        base64.StdEncoding.EncodeToString([]byte("short")),
		"missing file": "file:" + filepath.Join(dir, "missing"),
		"short file":   "file:" + short,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(context.Background(), config.EncryptionSettings{
				Provider: config.EncryptionLocal,
				Local:    config.LocalEncryptionSettings{Key: key},
			}, &envelopetest.MemoryStore{})
			if err == nil || !strings.Contains(err.Error(), "encryption.local.key") {
				t.Fatalf("New = %v, want an error naming encryption.local.key", err)
			}
		})
	}
}

func TestALocalKeyIsReadFromAFile(t *testing.T) {
	ctx := context.Background()
	cfg, _ := localSettings(t)
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(cfg.Local.Key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile := cfg
	fromFile.Local.Key = "file:" + path
	keys := &envelopetest.MemoryStore{}
	a := newKeyring(t, cfg, keys)
	b := newKeyring(t, fromFile, keys)
	if _, idA := a.Provider(); func() string { _, id := b.Provider(); return id }() != idA {
		t.Fatal("a key read from a file is another key")
	}
	sealed, err := a.Seal(ctx, SecretValue("proj_a", "s"), []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.Open(ctx, SecretValue("proj_a", "s"), sealed); err != nil || string(got) != "v" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestAWSRegionComesFromTheKeyARN(t *testing.T) {
	for key, want := range map[string]string{
		"arn:aws:kms:eu-west-2:111122223333:key/1234":       "eu-west-2",
		"arn:aws:kms:us-east-1:111122223333:alias/discobox": "us-east-1",
		"alias/discobox":                       "",
		"1234abcd-12ab-34cd-56ef-1234567890ab": "",
	} {
		if got := awsRegion(config.AWSKMSEncryptionSettings{Key: key}); got != want {
			t.Errorf("awsRegion(%q) = %q, want %q", key, got, want)
		}
	}
	if got := awsRegion(config.AWSKMSEncryptionSettings{Key: "arn:aws:kms:eu-west-2:1:key/1", Region: "us-west-1"}); got != "us-west-1" {
		t.Errorf("a configured region = %q, want it to win", got)
	}
}
