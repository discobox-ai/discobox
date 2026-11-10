package console

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// A binding with seconds left says so, rather than rounding to zero minutes
// and reading as one that never expires.
func TestABindingAboutToLapseDoesNotReadAsNever(t *testing.T) {
	t.Parallel()
	now := time.Now()
	b := SecretBinding{Expires: now.Add(20 * time.Second)}
	if got := BindingExpiry(b, now); got != "expires in 20s" {
		t.Fatalf("expiry = %q, want the seconds left", got)
	}
	if got := BindingLifeLeft(b, now); got != "in 20s" {
		t.Fatalf("life left = %q, want the seconds left", got)
	}
}

// A row counts only the live bindings, says a secret with no routes is sent
// nowhere, and says a refused credential is refused whatever is bound to it.
func TestASecretsRowSaysWhereAndWhatStandsOnIt(t *testing.T) {
	t.Parallel()
	now := time.Now()
	bindings := []SecretBinding{
		{ID: "b1", SecretID: "s1"},
		{ID: "b2", SecretID: "s1", Expires: now.Add(time.Hour)},
		{ID: "b3", SecretID: "s1", Expires: now.Add(-time.Minute)},
		{ID: "b4", SecretID: "s2"},
	}
	if live := LiveBindings(bindings, "s1", now); len(live) != 2 || live[0].ID != "b1" || live[1].ID != "b2" {
		t.Fatalf("live = %+v, want the two that have not lapsed", live)
	}
	if where, nowhere := SecretWhere(Secret{}); where != "sent nowhere" || !nowhere {
		t.Fatalf("no routes = %q %v", where, nowhere)
	}
	route := secretroute.Route{Hosts: []string{"api.github.com"}}
	if where, nowhere := SecretWhere(Secret{Routes: []secretroute.Route{route}}); where != RoutesText([]secretroute.Route{route}) || nowhere {
		t.Fatalf("routed = %q %v", where, nowhere)
	}
	for _, tc := range []struct {
		live     int
		refused  bool
		text     string
		standing SecretStanding
	}{
		{0, false, "—", SecretUnbound},
		{1, false, "1 binding", SecretBound},
		{2, false, "2 bindings", SecretBound},
		{2, true, "refused", SecretRefused},
	} {
		if text, standing := SecretStandingOf(tc.live, tc.refused); text != tc.text || standing != tc.standing {
			t.Errorf("%d refused=%v = %q %q, want %q %q", tc.live, tc.refused, text, standing, tc.text, tc.standing)
		}
	}
	session := Session{Servers: []string{"home", "work"}}
	refused := RefusedSecrets(session, "", []SecretRejection{{SecretID: "s1", Host: "a"}, {Server: "home", SecretID: "s1", Host: "b"}, {Server: "work", SecretID: "s2"}})
	if len(refused) != 1 || refused["s1"].Host != "a" {
		t.Fatalf("refused = %+v, want the first rejection of each, on this server alone", refused)
	}
	boxes := BindableBoxes(session, "work", []Sandbox{{ID: "a"}, {ID: "b", Server: "work"}, {ID: "c", Server: "home"}})
	if len(boxes) != 1 || boxes[0].ID != "b" {
		t.Fatalf("bindable = %+v, want the server's boxes alone", boxes)
	}
}

// Reading a credential never shows its value, says a secret with no routes
// goes nowhere, and says loudly that an oauth credential cannot renew.
func TestDescribingASecretSaysWhatItIs(t *testing.T) {
	t.Parallel()
	now := time.Now()
	text := func(sections []DetailSection) string {
		var b strings.Builder
		for _, s := range sections {
			fmt.Fprintln(&b, s.Label)
			for _, f := range s.Fields {
				fmt.Fprintln(&b, f.Label, f.Value, f.Tone)
			}
			for _, l := range s.Lines {
				fmt.Fprintln(&b, l.Text, l.Tone)
			}
		}
		return b.String()
	}
	token := text(DescribeSecret(Secret{Type: "token"}, now))
	for _, want := range []string{"kind token", "none — bindings of it may last forever", "nowhere — it has no routes alert"} {
		if !strings.Contains(token, want) {
			t.Errorf("token card lacks %q:\n%s", want, token)
		}
	}
	// Where it renews is set apart from the literal: a URL beside "Token" in
	// one reads to gosec as a credential written into the code.
	renews := &SecretOAuth{Scopes: []string{"a", "b"}}
	renews.TokenURL = "https://t"
	oauth := text(DescribeSecret(Secret{Type: "oauth", MaxTTL: 24 * time.Hour, OAuth: renews}, now))
	for _, want := range []string{"at most 1 day", "renews at https://t", "may do a b", "cannot renew itself", "alert"} {
		if !strings.Contains(oauth, want) {
			t.Errorf("oauth card lacks %q:\n%s", want, oauth)
		}
	}
}

// A binding is named for its target the way a person says it, read with
// what it is for, and taking it back asks first with No the quick answer.
func TestABindingIsReadAndTakenBackInItsOwnWords(t *testing.T) {
	t.Parallel()
	harnesses := []Harness{{ID: "hc1", Name: "Claude"}}
	boxes := []Sandbox{{ID: "sbx1", Name: "fixer"}}
	for _, tc := range []struct {
		b    SecretBinding
		want string
	}{
		{SecretBinding{TargetKind: BindingProject}, "every discobox in the project"},
		{SecretBinding{TargetKind: BindingHarnessConfig, TargetID: "hc1"}, "harness Claude"},
		{SecretBinding{TargetKind: BindingHarnessConfig, TargetID: "hc9"}, "harness hc9"},
		{SecretBinding{TargetKind: BindingSandbox, TargetID: "sbx1"}, "discobox fixer"},
	} {
		if got := BindingTarget(tc.b, harnesses, boxes); got != tc.want {
			t.Errorf("target = %q, want %q", got, tc.want)
		}
	}
	use := SecretBinding{Use: true, EnvVar: "GH_TOKEN"}
	given := SecretBinding{Delegate: true}
	if BindingLabel(use) != "GH_TOKEN" || BindingLabel(given) != "given on" {
		t.Fatalf("labels = %q, %q", BindingLabel(use), BindingLabel(given))
	}
	q := UnbindQuestion("gh", use, "discobox fixer")
	if !q.DefaultNo || !strings.Contains(q.Body, "gh to discobox fixer as GH_TOKEN") || !strings.Contains(q.Body, "stops resolving") {
		t.Fatalf("unbind = %+v", q)
	}
	if q := UnbindQuestion("gh", given, "harness Claude"); strings.Contains(q.Body, "stops resolving") || !strings.Contains(q.Body, "given on under it") {
		t.Fatalf("unbinding a delegation = %+v", q)
	}
	if q := DeleteSecretQuestion("gh", 2); !q.DefaultNo || !strings.Contains(q.Body, "live bindings (2)") {
		t.Fatalf("delete = %+v", q)
	}
}

// An edit says each part it changed, and one that changed nothing is not
// written.
func TestASecretEditSaysWhatChangedAndWritesNothingUnchanged(t *testing.T) {
	t.Parallel()
	src := &secretSource{listed: []Secret{{ID: "s1", Name: "gh"}}}
	if note, err := SaveSecret(t.Context(), src, "", "s1", SecretUpdate{}); err != nil || note != "gh is unchanged" {
		t.Fatalf("unchanged = %q, %v", note, err)
	}
	name, none := "github", []secretroute.Route{}
	note, err := SaveSecret(t.Context(), src, "", "s1", SecretUpdate{Name: &name, Routes: &none})
	if err != nil || note != "renamed gh to github, gh is sent nowhere" {
		t.Fatalf("edit = %q, %v", note, err)
	}
	if !slices.Equal(src.did, []string{"update s1"}) {
		t.Fatalf("did %v, want one write", src.did)
	}
}

// An action reads what it acts on again: a secret no longer listed, or a
// binding that is gone or has lapsed, is refused, and nothing is written.
func TestSecretActionsReadWhatTheyActOnAgain(t *testing.T) {
	t.Parallel()
	now := time.Now()
	src := &secretSource{
		listed:   []Secret{{ID: "s1", Name: "gh"}},
		bindings: []SecretBinding{{ID: "b1", SecretID: "s1"}, {ID: "b2", SecretID: "s1", Expires: now.Add(-time.Second)}},
	}
	if s, err := OfferedSecret(t.Context(), src, "", "s1"); err != nil || s.Name != "gh" {
		t.Fatalf("offered = %+v, %v", s, err)
	}
	if _, err := OfferedSecret(t.Context(), src, "", "gone"); err == nil {
		t.Fatal("a secret no longer listed was offered")
	}
	if b, err := OfferedBinding(t.Context(), src, "", "s1", "b1", now); err != nil || b.ID != "b1" {
		t.Fatalf("binding = %+v, %v", b, err)
	}
	for _, id := range []string{"b2", "b9"} {
		if _, err := OfferedBinding(t.Context(), src, "", "s1", id, now); err == nil || err.Error() != "that binding is no longer there" {
			t.Fatalf("%s: %v, want it refused", id, err)
		}
	}
	for _, refused := range []func() error{
		func() error { _, err := Unbind(t.Context(), src, "", "s1", "b2"); return err },
		func() error { _, err := DeleteSecret(t.Context(), src, "", "gone"); return err },
		func() error { _, err := Bind(t.Context(), src, "", NewBinding{SecretID: "gone"}); return err },
		func() error { _, err := SaveSecret(t.Context(), src, "", "gone", SecretUpdate{}); return err },
	} {
		if refused() == nil {
			t.Fatal("an action on what is gone was taken")
		}
	}
	if len(src.did) != 0 {
		t.Fatalf("did %v on what is gone", src.did)
	}
	src.handedOn = 2
	if note, err := Unbind(t.Context(), src, "", "s1", "b1"); err != nil || note != "unbound, and 2 bindings handed on under it" {
		t.Fatalf("unbind = %q, %v", note, err)
	}
	if note, err := Bind(t.Context(), src, "", NewBinding{SecretID: "s1", Use: true, EnvVar: "GH_TOKEN"}); err != nil || note != "bound gh as GH_TOKEN" {
		t.Fatalf("bind = %q, %v", note, err)
	}
}

type secretSource struct {
	DataSource
	listed   []Secret
	bindings []SecretBinding
	handedOn int
	did      []string
}

func (s *secretSource) Secrets(context.Context, string) ([]Secret, error) { return s.listed, nil }

func (s *secretSource) Bindings(_ context.Context, _ string, f BindingFilter) ([]SecretBinding, error) {
	var out []SecretBinding
	for _, b := range s.bindings {
		if f.SecretID == "" || b.SecretID == f.SecretID {
			out = append(out, b)
		}
	}
	return out, nil
}

func (s *secretSource) UpdateSecret(_ context.Context, _, id string, _ SecretUpdate) error {
	s.did = append(s.did, "update "+id)
	return nil
}

func (s *secretSource) DeleteBinding(_ context.Context, _, id string) (int, error) {
	s.did = append(s.did, "unbind "+id)
	return s.handedOn, nil
}

func (s *secretSource) CreateBinding(_ context.Context, _ string, b NewBinding) (SecretBinding, error) {
	s.did = append(s.did, "bind "+b.SecretID)
	return SecretBinding{ID: "b9", SecretID: b.SecretID}, nil
}
