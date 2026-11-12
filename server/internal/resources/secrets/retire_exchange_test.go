package secrets_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretformat"
	"github.com/discobox-ai/discobox/server/internal/config"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/encryption"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A stored exchange secret becomes a token secret holding the key a person
// stored, sent nowhere, with nothing it held widened (ADR 26-11-12-957 §§2-3):
// no routes, no raw bindings, no lifetime of the token's, no rejection of the
// old value. Its plain bindings and route constraints stay. A secret with more
// than one field keeps them all, as a JSON object. The value is sealed again,
// and a second run changes nothing. A value that cannot be read is dropped
// rather than stopping the server.
func TestAnExchangeSecretBecomesATokenSentNowhere(t *testing.T) {
	ctx := testPrincipalContext()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Write.Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatal(err)
	}
	key, err := encryption.GenerateLocalKey()
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := encryption.New(ctx, config.EncryptionSettings{
		Provider: config.EncryptionLocal,
		Local:    config.LocalEncryptionSettings{Key: key},
	}, store.NewEncryptionKeys(db.Write, db.CurrentRead()))
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(db.Write, db.Read, store.WithKeyring(keyring))
	if _, err := st.PrepareEncryption(ctx); err != nil {
		t.Fatal(err)
	}
	svc := resourcesecrets.NewService(st)
	marks := &sentinelMarks{}
	svc.SetSecretHolders(marks)

	expires := time.Now().Add(time.Hour).UTC()
	exchange := func(name, value string) *model.Secret {
		t.Helper()
		sec := &model.Secret{
			ProjectID:        "project-1",
			Name:             name,
			Type:             "exchange",
			MaxBindingTTL:    3600,
			TTL:              900,
			AllowRaw:         true,
			Routes:           []model.SecretRoute{{Hosts: []string{"app.boxd.sh"}}},
			RouteConstraints: [][]model.SecretRoute{{{Hosts: []string{"*.boxd.sh"}}}},
			ValueExpiresAt:   &expires,
			EncryptedValue:   []byte(value),
		}
		if err := st.CreateSecret(ctx, sec); err != nil {
			t.Fatal(err)
		}
		return sec
	}
	boxd := exchange("boxd", `{"token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln","accessTokenExpiresAt":1,"exchange":{"api_key":"bxd_0123456789abcdef0123456789abcdef"}}`)
	pair := exchange("pair", `{"token":"eyJ.e30.c2ln","exchange":{"api_key":"k","org":"acme"}}`)
	// A value nobody can read is dropped, not a reason to refuse the start.
	lost := exchange("lost", `{}`)
	if err := db.Write.Model(&model.Secret{}).Where("id = ?", lost.ID).Update("encrypted_value", []byte("not a value")).Error; err != nil {
		t.Fatal(err)
	}

	raw := mustBindAs(t, st, &model.SecretBinding{SecretID: boxd.ID, TargetKind: model.SecretBindingTargetProject, TargetID: "project-1", Use: true, EnvName: "BOXD_RAW", Raw: "env"})
	plain := mustBindAs(t, st, &model.SecretBinding{SecretID: boxd.ID, TargetKind: model.SecretBindingTargetProject, TargetID: "project-1", Use: true, EnvName: "BOXD"})
	if err := st.RecordSecretRejection(ctx, &model.SecretRejection{ProjectID: "project-1", SecretID: boxd.ID, Host: "app.boxd.sh", Reason: model.SecretRejectionReasonRefreshFailed}); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := svc.RetireExchangeSecrets(ctx); err != nil {
			t.Fatal(err)
		}
	}

	for _, want := range []struct {
		secret *model.Secret
		token  string
	}{
		{boxd, "bxd_0123456789abcdef0123456789abcdef"},
		{pair, `{"api_key":"k","org":"acme"}`},
		{lost, ""},
	} {
		got, err := st.GetSecret(ctx, "project-1", want.secret.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Type != model.SecretTypeToken || len(got.Routes) != 0 || got.AllowRaw || got.TTL != 0 || got.ValueExpiresAt != nil {
			t.Fatalf("%s = type %s routes %v raw %v ttl %d expires %v, want a token sent nowhere, never raw, never stale",
				got.Name, got.Type, got.Routes, got.AllowRaw, got.TTL, got.ValueExpiresAt)
		}
		if len(got.RouteConstraints) != 1 {
			t.Fatalf("%s constraints = %v, want them kept", got.Name, got.RouteConstraints)
		}
		if !encryption.Sealed(got.EncryptedValue) {
			t.Fatalf("%s value is not sealed", got.Name)
		}
		plaintext, err := st.OpenSecretPlaintext(ctx, got)
		if err != nil {
			t.Fatal(err)
		}
		wantValue, err := json.Marshal(model.SecretValue{Token: want.token}) //nolint:gosec // The value the test expects to find stored.
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(plaintext, wantValue) {
			t.Fatalf("%s value = %s, want %s and nothing the server made", got.Name, plaintext, wantValue)
		}
		if want.token != "" && got.Format != secretformat.Describe(want.token) {
			t.Fatalf("%s format = %q, want the key's shape %q", got.Name, got.Format, secretformat.Describe(want.token))
		}
	}

	bindings, err := st.ListSecretBindings(ctx, "project-1", store.SecretBindingFilter{SecretID: boxd.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].ID != plain.ID {
		t.Fatalf("bindings = %+v, want the plain one kept and the raw one %s gone", bindings, raw.ID)
	}
	if rejections := mustRejections(t, st); len(rejections) != 0 {
		t.Fatalf("rejections = %+v, want the old value's gone", rejections)
	}
	marked := map[string]bool{}
	for _, id := range marks.secrets {
		marked[id] = true
	}
	if !marked[boxd.ID] || !marked[pair.ID] || !marked[lost.ID] || len(marks.secrets) != 3 {
		t.Fatalf("marked %v, want each converted secret's holders once", marks.secrets)
	}
	if len(marks.bindings) != 1 || marks.bindings[0].ID != raw.ID {
		t.Fatalf("marked bindings %+v, want the deleted raw binding's holders", marks.bindings)
	}
}
