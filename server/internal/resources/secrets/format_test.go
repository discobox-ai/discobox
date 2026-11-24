package secrets_test

import (
	"net/http"
	"strings"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// A format a person sets is the one sentinels are minted from, and it outlives
// the value it was set against; clearing it goes back to reading the value.
func TestSecretFormatSetOutlivesTheValue(t *testing.T) {
	svc := newTestService(t)
	ctx := testPrincipalContext()
	apiKey := "sk-ant-api03-" + strings.Repeat("a", 95)

	sec, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:   "claude",
		Type:   apigen.CreateSecretBodyTypeToken,
		Value:  apigen.SecretValue{Token: apigen.NewOptString(apiKey)},
		Format: apigen.NewOptString("sk-custom-{hex:40}"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sec.Format != "sk-custom-{hex:40}" || !sec.FormatSet {
		t.Fatalf("created format = %q set=%t, want the one given", sec.Format, sec.FormatSet)
	}

	sec, err = svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{
		Value: apigen.NewOptSecretValue(apigen.SecretValue{Token: apigen.NewOptString("sk-ant-api03-" + strings.Repeat("b", 95))}),
	})
	if err != nil {
		t.Fatalf("replace value: %v", err)
	}
	if sec.Format != "sk-custom-{hex:40}" || !sec.FormatSet {
		t.Fatalf("format after a new value = %q set=%t, want it kept", sec.Format, sec.FormatSet)
	}

	sec, err = svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{Format: apigen.NewOptString("")})
	if err != nil {
		t.Fatalf("clear format: %v", err)
	}
	if !strings.HasPrefix(sec.Format, "sk-ant-api03-") || sec.FormatSet {
		t.Fatalf("cleared format = %q set=%t, want it read from the value", sec.Format, sec.FormatSet)
	}
}

// A template that does not parse is refused rather than stored, where the
// minter would have quietly fallen back to the default shape.
func TestSecretFormatMustParse(t *testing.T) {
	svc := newTestService(t)
	ctx := testPrincipalContext()
	for _, format := range []string{"sk-{nope:4}", "sk-{hex}", "{alnum:5000}"} {
		_, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
			Name:   "bad",
			Type:   apigen.CreateSecretBodyTypeToken,
			Value:  apigen.SecretValue{Token: apigen.NewOptString("x")},
			Format: apigen.NewOptString(format),
		})
		if statusOf(err) != http.StatusBadRequest {
			t.Fatalf("format %q err = %v, want 400", format, err)
		}
	}
}

// A format is stored in the clear, so one that copies the value's bytes past
// its prefix is refused, the way the console refuses it, whichever client
// sets it.
func TestSecretFormatMayNotCopyTheValue(t *testing.T) {
	svc := newTestService(t)
	ctx := testPrincipalContext()
	_, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:   "copied",
		Type:   apigen.CreateSecretBodyTypeToken,
		Value:  apigen.SecretValue{Token: apigen.NewOptString("dbx_fake_ABCDEFGHIJKLMNOPQRSTUVWX")},
		Format: apigen.NewOptString("dbx_fake_ABCDEFGH{alnum:16}"),
	})
	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "copied from the value") {
		t.Fatalf("create err = %v, want 400 naming the copied text", err)
	}

	sec, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name:  "plain",
		Type:  apigen.CreateSecretBodyTypeToken,
		Value: apigen.SecretValue{Token: apigen.NewOptString("dbx_fake_ABCDEFGHIJKLMNOPQRSTUVWX")},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{Format: apigen.NewOptString("dbx_fake_ABCDEFGH{alnum:16}")})
	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "copied from the value") {
		t.Fatalf("update --format err = %v, want 400 naming the copied text", err)
	}

	// With a new value in the same update, the format is held to that value,
	// not the one it replaces.
	_, err = svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{
		Value:  apigen.NewOptSecretValue(apigen.SecretValue{Token: apigen.NewOptString("dbx_fake_ZYXWVUTSRQPONMLKJIHGFEDC")}),
		Format: apigen.NewOptString("dbx_fake_ZYXWVUTS{alnum:16}"),
	})
	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "copied from the value") {
		t.Fatalf("update --token --format err = %v, want 400 naming the copied text", err)
	}
	updated, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{
		Value:  apigen.NewOptSecretValue(apigen.SecretValue{Token: apigen.NewOptString("dbx_fake_ZYXWVUTSRQPONMLKJIHGFEDC")}),
		Format: apigen.NewOptString("dbx_fake_ABCDEFGH{alnum:16}"),
	})
	if err != nil {
		t.Fatalf("update with a format copying only the replaced value: %v", err)
	}
	if updated.Format != "dbx_fake_ABCDEFGH{alnum:16}" {
		t.Fatalf("format = %q, want the one given", updated.Format)
	}
}
