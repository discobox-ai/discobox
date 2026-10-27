package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/secretformat"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/secrets"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Every value write records the shape its sentinels take, whichever writer
// made it and whatever type the secret is. The harness configure flow writes
// raw rows with no format of its own, and a Claude Code subscription login is
// an oauth secret; its sentinel has to read as an OAuth access token, or
// Claude Code takes it for something else.
func TestValueWriteRecordsTheSentinelFormat(t *testing.T) {
	ctx := context.Background()
	key, err := secrets.GenerateBase64Key()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealerFromBase64Key(key)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	s, _ := newTestStoreWithDB(t, sealer)

	oauth := &model.Secret{
		ID: "sec-oauth", ProjectID: "project-1", Name: "claude", Type: model.SecretTypeOAuth,
		EncryptedValue: []byte(`{"token":"sk-ant-oat01-` + strings.Repeat("a", 95) + `","refreshToken":"sk-ant-ort01-x"}`),
	}
	if err := s.CreateSecret(ctx, oauth); err != nil {
		t.Fatalf("create: %v", err)
	}
	stored, err := s.GetSecret(ctx, "project-1", "sec-oauth")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.HasPrefix(stored.Format, "sk-ant-oat01-") {
		t.Fatalf("oauth format = %q, want an sk-ant-oat01- template", stored.Format)
	}

	// Replacing the value re-reads its shape; a write that keeps the value
	// (here, a rename handing back the sealed row) keeps the format.
	stored.EncryptedValue = []byte(`{"token":"sk-ant-api03-` + strings.Repeat("b", 95) + `"}`)
	if err := s.UpdateSecret(ctx, stored); err != nil {
		t.Fatalf("update: %v", err)
	}
	stored, _ = s.GetSecret(ctx, "project-1", "sec-oauth")
	if !strings.HasPrefix(stored.Format, "sk-ant-api03-") {
		t.Fatalf("replaced format = %q, want an sk-ant-api03- template", stored.Format)
	}
	stored.Name = "renamed"
	if err := s.UpdateSecret(ctx, stored); err != nil {
		t.Fatalf("rename: %v", err)
	}
	stored, _ = s.GetSecret(ctx, "project-1", "sec-oauth")
	if !strings.HasPrefix(stored.Format, "sk-ant-api03-") {
		t.Fatalf("renamed format = %q, want it kept", stored.Format)
	}

	// A format a person set survives a new value; clearing it, with the value
	// left sealed as it is, reads the shape from that value again.
	stored.Format, stored.FormatSet = "custom-{hex:8}", true
	if err := s.UpdateSecret(ctx, stored); err != nil {
		t.Fatalf("set format: %v", err)
	}
	stored, _ = s.GetSecret(ctx, "project-1", "sec-oauth")
	stored.EncryptedValue = []byte(`{"token":"sk-ant-oat01-` + strings.Repeat("c", 95) + `"}`)
	if err := s.UpdateSecret(ctx, stored); err != nil {
		t.Fatalf("replace under a set format: %v", err)
	}
	stored, _ = s.GetSecret(ctx, "project-1", "sec-oauth")
	if stored.Format != "custom-{hex:8}" || !stored.FormatSet {
		t.Fatalf("set format after a new value = %q set=%t, want it kept", stored.Format, stored.FormatSet)
	}
	stored.Format, stored.FormatSet = "", false
	if err := s.UpdateSecret(ctx, stored); err != nil {
		t.Fatalf("clear format: %v", err)
	}
	stored, _ = s.GetSecret(ctx, "project-1", "sec-oauth")
	if !strings.HasPrefix(stored.Format, "sk-ant-oat01-") || stored.FormatSet {
		t.Fatalf("cleared format = %q set=%t, want it read from the sealed value", stored.Format, stored.FormatSet)
	}
}

// A row written before every value write recorded a format still mints a
// sentinel shaped like its value, on every path that asks the store.
func TestSentinelFormatReadsARowWithoutOne(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)
	createSecret(t, s, "sec-legacy", "legacy")
	if err := db.Write.WithContext(ctx).Model(&model.Secret{}).Where("id = ?", "sec-legacy").
		Updates(map[string]any{"format": "", "encrypted_value": []byte(`{"token":"sk-ant-oat01-` + strings.Repeat("c", 95) + `"}`)}).Error; err != nil {
		t.Fatalf("age row: %v", err)
	}
	legacy, err := s.GetSecret(ctx, "project-1", "sec-legacy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if legacy.Format != "" {
		t.Fatalf("row still carries format %q", legacy.Format)
	}
	format := s.SentinelFormat(ctx, legacy)
	sentinel, err := secretformat.MintSentinel(format)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(sentinel, "sk-ant-oat01-") {
		t.Fatalf("sentinel %q from format %q, want sk-ant-oat01-", sentinel, format)
	}
}

// A shape read under an older provider table is brought up to date at start,
// without anybody replacing the value: an Anthropic key stored before the kind
// marker was kept went on minting sk-ant-XXXXX- sentinels otherwise. A format a
// person set is theirs and is left alone, and so is updated_at, which an OAuth
// refresh uses as its generation guard.
func TestRefreshSecretFormatsBringsStoredShapesUpToDate(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)
	createSecret(t, s, "sec-old", "old")
	createSecret(t, s, "sec-set", "set")
	apiKey := `{"token":"sk-ant-api03-` + strings.Repeat("a", 95) + `"}`
	for id, cols := range map[string]map[string]any{
		"sec-old": {"format": "sk-ant-{alnum:5}-{base64url:95}", "encrypted_value": []byte(apiKey)},
		"sec-set": {"format": "mine-{hex:8}", "format_set": true, "encrypted_value": []byte(apiKey)},
	} {
		if err := db.Write.WithContext(ctx).Model(&model.Secret{}).Where("id = ?", id).Updates(cols).Error; err != nil {
			t.Fatalf("age %s: %v", id, err)
		}
	}
	before, _ := s.GetSecret(ctx, "project-1", "sec-old")

	if err := s.RefreshSecretFormats(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	old, _ := s.GetSecret(ctx, "project-1", "sec-old")
	if old.Format != "sk-ant-api03-{base64url:95}" {
		t.Fatalf("old format = %q, want the current shape of its value", old.Format)
	}
	if !old.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("updated_at moved from %v to %v", before.UpdatedAt, old.UpdatedAt)
	}
	set, _ := s.GetSecret(ctx, "project-1", "sec-set")
	if set.Format != "mine-{hex:8}" {
		t.Fatalf("set format = %q, want the one a person chose", set.Format)
	}
}

// An OAuth refresh is a value write like any other, and the shape rides with
// it rather than being left to whatever an older write stored.
func TestAnOAuthRefreshWritesTheFormat(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)
	createSecret(t, s, "sec-oauth", "claude")
	if err := db.Write.WithContext(ctx).Model(&model.Secret{}).Where("id = ?", "sec-oauth").
		Updates(map[string]any{"type": model.SecretTypeOAuth, "format": ""}).Error; err != nil {
		t.Fatalf("age row: %v", err)
	}
	sec, _ := s.GetSecret(ctx, "project-1", "sec-oauth")
	sec.EncryptedValue = []byte(`{"token":"sk-ant-oat01-` + strings.Repeat("d", 95) + `","refreshToken":"r"}`)
	if err := s.UpdateSecretValueIfUnchanged(ctx, sec, sec.Revision); err != nil {
		t.Fatalf("refresh write: %v", err)
	}
	got, _ := s.GetSecret(ctx, "project-1", "sec-oauth")
	if !strings.HasPrefix(got.Format, "sk-ant-oat01-") {
		t.Fatalf("format after a refresh = %q, want the access token's shape", got.Format)
	}
}

// createSecret is one project secret.
func createSecret(t *testing.T, s *store.Store, id, name string) *model.Secret {
	t.Helper()
	secret := &model.Secret{
		ID: id, ProjectID: "project-1", Name: name, Type: model.SecretTypeToken,
		EncryptedValue: []byte(`{"token":"x"}`),
	}
	if err := s.CreateSecret(context.Background(), secret); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return secret
}

// Every write that moves updated_at advances the revision, whatever the clock
// says: Windows stamps two writes in one tick alike, and a refresh guarded on
// the timestamp would then overwrite a rotation it never saw.
func TestSecretWritesAdvanceTheRevision(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createSecret(t, s, "sec-rev", "rev")
	stale, _ := s.GetSecret(ctx, "project-1", "sec-rev")
	if stale.Revision != 1 {
		t.Fatalf("created revision = %d, want 1", stale.Revision)
	}
	edited := *stale
	edited.Name = "rev-renamed"
	if err := s.UpdateSecret(ctx, &edited); err != nil {
		t.Fatal(err)
	}
	if edited.Revision != 2 {
		t.Fatalf("revision after an update = %d, want 2", edited.Revision)
	}
	edited.MaxBindingTTL = 3600
	if err := s.UpdateSecret(ctx, &edited); err != nil {
		t.Fatal(err)
	}
	stale.EncryptedValue = []byte(`{"token":"y"}`)
	if err := s.UpdateSecretValueIfUnchanged(ctx, stale, stale.Revision); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("refresh from a stale revision err = %v, want ErrGenerationConflict", err)
	}
	got, _ := s.GetSecret(ctx, "project-1", "sec-rev")
	if got.Revision != 3 {
		t.Fatalf("revision after a second update = %d, want 3", got.Revision)
	}
	if err := s.UpdateSecretValueIfUnchanged(ctx, got, got.Revision); err != nil {
		t.Fatal(err)
	}
	if got.Revision != 4 {
		t.Fatalf("revision after a refresh = %d, want 4", got.Revision)
	}
}
