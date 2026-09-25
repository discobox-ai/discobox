package tui

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/wellknown"
)

// Enter on a picker lists every option at once, with what each means; ←→
// still steps through them, and the card is accepted from a picker with
// ctrl+s.
func TestEnterOnAPickerListsItsOptions(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)

	send(t, m, keyPress("n"), keyPress("down")) // onto the kind
	if !strings.Contains(dialogText(m), "ctrl+s stores it") || !strings.Contains(dialogText(m), "enter lists them") {
		t.Fatalf("keys on a picker = %q, want enter to list and ctrl+s to accept", dialogText(m))
	}
	send(t, m, keyPress("enter"))
	f := m.dialog.form
	if !f.listing {
		t.Fatal("enter on the kind did not list its options")
	}
	body := dialogText(m)
	for _, want := range []string{"a token", "an OAuth credential", wellknown.GitHubAPI, "one opaque string"} {
		if !strings.Contains(body, want) {
			t.Fatalf("list = %q, want every option with what it means, missing %q", body, want)
		}
	}
	if strings.Contains(body, "ai.discobox.sandbox") {
		t.Fatalf("list = %q, offers a gate, which has no value to store", body)
	}

	// The line under the card says the whole of the highlighted option.
	send(t, m, keyPress("down"), keyPress("down"))
	if !strings.Contains(f.hint(m.st, 200), "GitHub: repositories over HTTPS") {
		t.Fatalf("hint = %q, want the highlighted option's meaning", f.hint(m.st, 200))
	}
	send(t, m, keyPress("up"), keyPress("up"))

	// Esc closes the list, not the card, and changes nothing.
	send(t, m, keyPress("down"), keyPress("esc"))
	if m.dialog == nil || f.listing || f.chosen("kind") != "token" {
		t.Fatalf("esc: dialog %s, listing %v, kind %q; want the card back as it was", describe(m.dialog), f.listing, f.chosen("kind"))
	}

	// Enter on the highlighted option takes it, as ←→ landing on it would.
	send(t, m, keyPress("enter"), keyPress("down"), keyPress("down"), keyPress("enter"))
	if f.listing || f.chosen("kind") != wellKnownKind+wellknown.GitHubAPI || f.value("name") != "github" {
		t.Fatalf("picked: listing %v, kind %q, name %q; want com.github.api filled in", f.listing, f.chosen("kind"), f.value("name"))
	}
	if len(ds.createdSecrets) != 0 {
		t.Fatal("choosing from the list stored the secret")
	}
	send(t, m, keyPress("ctrl+s"))
	if len(ds.createdSecrets) != 1 || ds.createdSecrets[0].WellKnownID != wellknown.GitHubAPI {
		t.Fatalf("created = %#v, want ctrl+s to store the card", ds.createdSecrets)
	}
}
