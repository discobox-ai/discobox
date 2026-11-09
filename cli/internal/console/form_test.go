package console

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// answers are what a form opens on, with some answered.
func answers[T any](q Form[T], set map[string]string) Answers {
	a := q.Opening()
	for k, v := range set {
		a[k] = v
	}
	return a
}

// A required field refuses only while it applies: a field the answers above
// it have made irrelevant is not asked, and so cannot be missing.
func TestARequiredFieldIsAskedOnlyWhileItApplies(t *testing.T) {
	t.Parallel()
	q := Form[string]{
		Fields: []Field{
			{Key: "kind", Choices: []Choice{{Key: "a"}, {Key: "b"}}},
			{Key: "detail", Required: "say more", When: func(a Answers) bool { return a.Get("kind") == "b" }},
		},
		Submit: func(a Answers) (string, *Refusal) { return a.Get("detail"), nil },
	}
	if _, refused := q.Answer(answers(q, nil)); refused != nil {
		t.Fatalf("refused %v, want the field that does not apply left alone", refused)
	}
	_, refused := q.Answer(answers(q, map[string]string{"kind": "b", "detail": "  "}))
	if refused == nil || refused.Field != "detail" || refused.Why != "say more" {
		t.Fatalf("refusal = %+v, want the empty field named", refused)
	}
	if got, refused := q.Answer(answers(q, map[string]string{"kind": "b", "detail": " x "})); refused != nil || got != "x" {
		t.Fatalf("answer = %q %v, want the trimmed text", got, refused)
	}
}

// A picker opens on its value when it names an option, and on the first when
// it does not; a typed field opens on its value as one line.
func TestAFormOpensOnItsValues(t *testing.T) {
	t.Parallel()
	q := Form[struct{}]{Fields: []Field{
		{Key: "p", Choices: []Choice{{Key: "a"}, {Key: "b"}}, Value: "b"},
		{Key: "q", Choices: []Choice{{Key: "a"}, {Key: "b"}}, Value: "gone"},
		{Key: "t", Value: "one\ttwo\r\nthree\x07\uFFFD\xff"},
	}}
	a := q.Opening()
	if a["p"] != "b" || a["q"] != "a" || a["t"] != "one two  three" {
		t.Fatalf("opening = %#v", a)
	}
}

func TestHostListSplitsOnCommasAndSpaces(t *testing.T) {
	t.Parallel()
	if got := HostList(" a.com,b.com  c.com, ,"); !slices.Equal(got, []string{"a.com", "b.com", "c.com"}) {
		t.Fatalf("HostList = %q", got)
	}
	if got := HostList(""); got == nil || len(got) != 0 {
		t.Fatalf("empty = %#v, want an empty list", got)
	}
}

// Settings answer only what changed, and the harness only on a stopped box.
func TestSettingsAnswerOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	was := BoxSettings{Name: "odd\x07name", HarnessID: "hc_a", Egress: Egress{Allow: []string{"a.com"}}}
	running := SettingsForm(Sandbox{State: StateRunning}, was, []Harness{{ID: "hc_a"}, {ID: "hc_b"}})
	update, refused := running.Answer(answers(running, nil))
	if refused != nil || !update.Empty() {
		t.Fatalf("untouched = %+v %v, want nothing to save", update, refused)
	}
	update, _ = running.Answer(answers(running, map[string]string{"harness": "hc_b", "allow": "a.com, b.com"}))
	if update.Harness != nil || update.EgressAllow == nil || !slices.Equal(*update.EgressAllow, []string{"a.com", "b.com"}) || update.Name != nil {
		t.Fatalf("running = %+v, want the allow list alone", update)
	}
	stopped := SettingsForm(Sandbox{State: StateStopped}, was, nil)
	if update, _ := stopped.Answer(answers(stopped, map[string]string{"harness": "hc_a"})); update.Harness != nil {
		t.Fatalf("stopped = %+v, want the box's own harness to change nothing", update)
	}
	if _, refused := stopped.Answer(answers(stopped, map[string]string{"name": ""})); refused == nil || refused.Field != "name" {
		t.Fatalf("refusal = %v, want an empty name refused", refused)
	}
}

// Rename asks for nothing when the answer is what is shown, or empty on a box
// with no given name.
func TestRenameOfWhatIsShownIsUnchanged(t *testing.T) {
	t.Parallel()
	q := RenameForm(Sandbox{}, "title")
	for _, typed := range []string{"title", " title ", ""} {
		if r, _ := q.Answer(Answers{"name": typed}); !r.Unchanged {
			t.Fatalf("%q = %+v, want unchanged", typed, r)
		}
	}
	if r, _ := RenameForm(Sandbox{GivenName: "x"}, "x").Answer(Answers{"name": ""}); r.Unchanged || r.Name != "" {
		t.Fatalf("clearing = %+v, want the given name taken away", r)
	}
	if r, _ := q.Answer(Answers{"name": "new"}); r.Unchanged || r.Name != "new" {
		t.Fatalf("new = %+v", r)
	}
}

// A new secret is refused with no route, and a lifetime or format that does
// not read is refused rather than meaning something else.
func TestANewSecretIsRefusedWhatItCannotStore(t *testing.T) {
	t.Parallel()
	q := NewSecretForm(NewSecretAsk{Name: "github"})
	base := map[string]string{"token": "t"}
	cases := []struct {
		set  map[string]string
		want string
	}{
		{map[string]string{}, "say where it may be sent"},
		{map[string]string{SecretRouteKey + "0": "not a route ==="}, ""},
		{map[string]string{SecretRouteKey + "0": "*", "ttl": LifetimeCustom, "ttlCustom": "a while"}, "a limit is"},
		{map[string]string{SecretRouteKey + "0": "*", "format": "{nope:1}"}, "the sentinel format does not read"},
		{map[string]string{SecretRouteKey + "0": "*", "source": "command", "command": ""}, "needs the command"},
	}
	for _, tc := range cases {
		set := map[string]string{}
		for k, v := range base {
			set[k] = v
		}
		for k, v := range tc.set {
			set[k] = v
		}
		_, refused := q.Answer(answers(q, set))
		if refused == nil || !strings.Contains(refused.Why, tc.want) {
			t.Fatalf("%v: refusal = %v, want %q", tc.set, refused, tc.want)
		}
	}
	secret, refused := q.Answer(answers(q, map[string]string{"token": "t", SecretRouteKey + "0": "api.github.com"}))
	if refused != nil || secret.Name != "github" || secret.Value.Token != "t" || len(secret.Routes) != 1 || secret.MaxTTLSeconds != 0 {
		t.Fatalf("secret = %+v %v", secret, refused)
	}
}

// Stored to approve a request, a binding limit shorter than the binding being
// made is refused before the secret is stored.
func TestANewSecretForAnApprovalKeepsItsLimitAboveTheBinding(t *testing.T) {
	t.Parallel()
	q := NewSecretForm(NewSecretAsk{Name: "github", Approving: true, BindFor: 24 * time.Hour})
	set := map[string]string{"token": "t", SecretRouteKey + "0": "*", "ttl": "3600"}
	if _, refused := q.Answer(answers(q, set)); refused == nil || !strings.Contains(refused.Why, "binding limit is shorter") {
		t.Fatalf("refusal = %v", refused)
	}
	set["ttl"] = LifetimeNever
	if _, refused := q.Answer(answers(q, set)); refused != nil {
		t.Fatalf("refusal = %v, want no limit accepted", refused)
	}
}

// Editing an oauth credential replaces its value whole or not at all.
func TestAnOAuthEditReplacesTheWholeValue(t *testing.T) {
	t.Parallel()
	//nolint:gosec // G101: where a credential renews, not a credential.
	was := Secret{Name: "claude", Type: "oauth", Routes: []secretroute.Route{{Hosts: []string{"api.anthropic.com"}}}, OAuth: &SecretOAuth{TokenURL: "https://t"}}
	q := EditSecretForm(was)
	update, refused := q.Answer(answers(q, nil))
	if refused != nil || update.Value != nil || update.Name != nil || update.Routes != nil {
		t.Fatalf("untouched = %+v %v, want nothing changed", update, refused)
	}
	if _, refused := q.Answer(answers(q, map[string]string{"access": "a"})); refused == nil || !strings.Contains(refused.Why, "refresh token too") {
		t.Fatalf("refusal = %v, want the refresh token asked for", refused)
	}
	update, refused = q.Answer(answers(q, map[string]string{"access": "a", "refresh": "r"}))
	if refused != nil || update.Value == nil || update.Value.RefreshToken != "r" {
		t.Fatalf("update = %+v %v", update, refused)
	}
}

// A binding to use a credential needs its variable; one only given on does not.
func TestABindingToUseNeedsAVariable(t *testing.T) {
	t.Parallel()
	q := BindingForm(Secret{ID: "sec", Name: "github"}, nil, nil)
	if b, _ := q.Answer(answers(q, nil)); b.EnvVar != "GITHUB" || !b.Use || b.TargetKind != BindingProject {
		t.Fatalf("binding = %+v, want the variable named for the secret", b)
	}
	if _, refused := q.Answer(answers(q, map[string]string{"envVar": ""})); refused == nil {
		t.Fatal("a binding to use with no variable was accepted")
	}
	if b, refused := q.Answer(answers(q, map[string]string{"envVar": "", "purpose": "delegate"})); refused != nil || b.Use || !b.Delegate {
		t.Fatalf("delegate = %+v %v", b, refused)
	}
}

// The supplied CA is offered first when the agent gave one, since it is what
// the server pins by default.
func TestASuppliedCAIsOfferedFirst(t *testing.T) {
	t.Parallel()
	ca := TrustCertificate{Subject: "CN=cluster-ca", SHA256: strings.Repeat("c", 64), IsCA: true}
	leaf := TrustCertificate{Subject: "CN=kube-apiserver", SHA256: strings.Repeat("a", 64), SPKISHA256: strings.Repeat("b", 64)}
	supplied := TrustCertificate{Subject: "CN=gke-ca", SHA256: strings.Repeat("f", 64), IsCA: true}
	ask := TrustAsk{Chain: []TrustCertificate{leaf, ca}, SuppliedCA: &supplied, DefaultPin: TrustPin{Kind: TrustPinCA, SHA256: supplied.SHA256}}
	options := trustPinOptions(ask)
	if options[0].pin.SHA256 != supplied.SHA256 || len(options) != 3 {
		t.Fatalf("options = %+v", options)
	}
}

// A refresh by paste needs the pasted value.
func TestARefreshByPasteNeedsTheValue(t *testing.T) {
	t.Parallel()
	q := RefreshForm(Secret{})
	if _, refused := q.Answer(answers(q, nil)); refused == nil {
		t.Fatal("a paste with nothing pasted was accepted")
	}
	if r, refused := q.Answer(answers(q, map[string]string{"value": " v "})); refused != nil || !r.Manual || r.Value != "v" {
		t.Fatalf("refresh = %+v %v", r, refused)
	}
}

// An addition stacks only on the secret already delivering the variable.
func TestAnApprovalAdditionStacksOnlyOnTheHolder(t *testing.T) {
	t.Parallel()
	req := CredentialRequest{EnvVar: "GH_TOKEN", Use: &RequestPart{Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}}}
	secrets := []Secret{{ID: "sec_a", Name: "a"}, {ID: "sec_b", Name: "b"}}
	held := []HeldGrant{{SecretID: "sec_a", SecretName: "a", From: BindingProject}}
	q := ApprovalPartForm(ApprovalPartAsk{Request: req, Secrets: secrets, Held: held, As: bindingplan.Add})
	if _, refused := q.Answer(answers(q, map[string]string{"credential": "sec_b"})); refused == nil || !strings.Contains(refused.Why, "an addition stacks only on that credential") {
		t.Fatalf("refusal = %v", refused)
	}
	if p, refused := q.Answer(answers(q, map[string]string{"credential": "sec_a"})); refused != nil || p.Secret.ID != "sec_a" || p.As != bindingplan.Add {
		t.Fatalf("part = %+v %v", p, refused)
	}
}

// An edit of one part of a secret leaves the rest as stored, even where a
// field holds a stored value otherwise than it is stored.
func TestASecretEditLeavesUntouchedFieldsAlone(t *testing.T) {
	t.Parallel()
	token := Secret{Name: "gh", Type: "token", RefreshCommand: []string{"sh", "-c", "a\nb"}}
	q := EditSecretForm(token)
	if update, refused := q.Answer(answers(q, map[string]string{"name": "gh2"})); refused != nil || update.RefreshCommand != nil || update.Name == nil {
		t.Fatalf("update = %+v %v, want the name alone", update, refused)
	}
	//nolint:gosec // G101: where a credential renews, not a credential.
	oauth := Secret{Name: "claude", Type: "oauth", OAuth: &SecretOAuth{TokenURL: " https://t\x07", ClientID: "c\t", Scopes: []string{"a\nb"}}}
	q2 := EditSecretForm(oauth)
	if update, refused := q2.Answer(answers(q2, map[string]string{"name": "claude2"})); refused != nil || update.Value != nil {
		t.Fatalf("update = %+v %v, want the oauth value left alone", update, refused)
	}
}
