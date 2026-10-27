package tui

import (
	"strings"
	"testing"
	"time"
)

// formatFixture is the secrets screen over one token whose sentinel shape was
// read from its value and one whose shape somebody chose.
func formatFixture(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectSecrets = []Secret{
		{ID: "sec_read", Name: "anthropic", Type: "token", MaxTTL: time.Hour, Format: "sk-ant-api03-{base64url:95}"},
		{ID: "sec_set", Name: "custom", Type: "token", MaxTTL: time.Hour, Format: "sk-custom-{hex:40}", FormatSet: true},
	}
	ds.projectBindings = nil
	ds.mu.Unlock()
	send(t, m, keyPress("r"))
	return m, ds
}

// Opening a secret says what stands in for it inside a discobox, and whether
// somebody chose that or it was read from the value — the difference being
// which one outlives a new value.
func TestOpeningASecretShowsItsSentinelFormat(t *testing.T) {
	t.Parallel()
	m, _ := formatFixture(t)

	send(t, m, keyPress("enter"))
	if body := dialogText(m); !strings.Contains(body, "sk-ant-api03-{base64url:95} · read from the value") {
		t.Fatalf("body = %q, want the format read from the value", body)
	}
	send(t, m, keyPress("esc"), keyPress("down"), keyPress("enter"))
	if body := dialogText(m); !strings.Contains(body, "sk-custom-{hex:40} · set") {
		t.Fatalf("body = %q, want the chosen format said as one", body)
	}
}

// A format read from the value opens as the row's placeholder, not its value:
// saving the card untouched must not turn it into a choice nobody made.
func TestAnUntouchedFormatIsNotSent(t *testing.T) {
	t.Parallel()
	m, ds := formatFixture(t)

	send(t, m, keyPress("e"))
	if got := formValue(t, m, "format"); got != "" {
		t.Fatalf("format row = %q, want it empty for a format nobody chose", got)
	}
	if !strings.Contains(dialogText(m), "read from the value: sk-ant-api03-{base64url:95}") {
		t.Fatalf("the row does not say what empty gives:\n%s", dialogText(m))
	}
	typeInto(t, m, "token", "sk-ant-api03-rotated")
	drain(t, m, submitForm(t, m), 0)
	if len(ds.updated) != 1 || ds.updated[0].Format != nil {
		t.Fatalf("updated = %#v, want the value replaced and the format left alone", ds.updated)
	}
}

// A format is chosen from the card, and a chosen one is cleared from it to go
// back to reading the value.
func TestAFormatIsSetAndClearedFromTheCard(t *testing.T) {
	t.Parallel()
	m, ds := formatFixture(t)

	send(t, m, keyPress("e"))
	typeInto(t, m, "format", "sk-ant-oat01-{base64url:95}")
	drain(t, m, submitForm(t, m), 0)
	if len(ds.updated) != 1 || ds.updated[0].Format == nil || *ds.updated[0].Format != "sk-ant-oat01-{base64url:95}" {
		t.Fatalf("updated = %#v, want the chosen format sent", ds.updated)
	}

	send(t, m, keyPress("down"), keyPress("e"))
	if got := formValue(t, m, "format"); got != "sk-custom-{hex:40}" {
		t.Fatalf("format row = %q, want the chosen format to open filled in", got)
	}
	clearRow(t, m, "format")
	drain(t, m, submitForm(t, m), 0)
	if len(ds.updated) != 2 || ds.updated[1].Format == nil || *ds.updated[1].Format != "" {
		t.Fatalf("updated = %#v, want the format cleared", ds.updated)
	}
}

// A template that does not parse is refused on the card that holds it, with
// what is wrong, rather than after the card has closed.
func TestAnUnreadableFormatKeepsTheCardUp(t *testing.T) {
	t.Parallel()
	m, ds := formatFixture(t)

	send(t, m, keyPress("e"))
	typeInto(t, m, "format", "sk-{nope:4}")
	send(t, m, keyPress("ctrl+s"))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("dialog = %s, want the card kept up", describe(m.dialog))
	}
	if !strings.Contains(m.dialog.form.err, "unknown charset") {
		t.Fatalf("err = %q, want what is wrong with the template", m.dialog.form.err)
	}
	if len(ds.updated) != 0 {
		t.Fatalf("updated = %#v, want nothing sent", ds.updated)
	}
}

// A new secret may be stored with a format of its own.
func TestANewSecretTakesAFormat(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"))
	typeInto(t, m, "name", "claude")
	typeInto(t, m, "token", "sk-ant-oat01-real")
	typeInto(t, m, secretRouteKey+"0", "api.anthropic.com")
	typeInto(t, m, "format", "sk-ant-oat01-{base64url:95}")
	drain(t, m, submitForm(t, m), 0)
	if len(ds.createdSecrets) != 1 || ds.createdSecrets[0].Format != "sk-ant-oat01-{base64url:95}" {
		t.Fatalf("created = %#v, want the format sent with it", ds.createdSecrets)
	}
}
