package console

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// The secrets page's rules (ADR 26-11-07-876 §1): what a secret's row says,
// what reading a secret, a binding or a decision says, what deleting and
// unbinding ask first, and what each action says it did. Each renderer draws
// them — the TUI's screen, the desktop's page — and binds its own keys to the
// actions; neither decides any of it. Every action is the same call the
// `discobox secret` commands make: a window that could do what the CLI could
// not would be a second policy.

// SecretAction is one thing the secrets page does.
type SecretAction string

// The actions.
const (
	// SecretActionNew stores a credential the project does not have yet
	// (NewSecretForm).
	SecretActionNew SecretAction = "new"
	// SecretActionEdit changes what a secret says about itself
	// (EditSecretForm).
	SecretActionEdit SecretAction = "edit"
	// SecretActionDelete removes a secret and every binding of it, asking
	// first (DeleteSecretQuestion).
	SecretActionDelete SecretAction = "delete"
	// SecretActionBind binds a secret by hand (BindingForm).
	SecretActionBind SecretAction = "bind"
	// SecretActionUnbind takes one of a secret's bindings back, asking first
	// (UnbindQuestion).
	SecretActionUnbind SecretAction = "unbind"
	// SecretActionHistory reads a secret's binding history.
	SecretActionHistory SecretAction = "history"
	// SecretActionRead reads the secrets again, for a page already open.
	SecretActionRead SecretAction = "read"
	// SecretActionAnswer answers one of the requests waiting on the page's
	// server: from its requests table, or a notification of one arriving.
	SecretActionAnswer SecretAction = "answer"
)

// DetailSection is a stretch of a card read rather than answered: a heading,
// its label and value rows, and the sentences under them. The TUI draws it as
// a dialog's section, the desktop as a panel's; the words and their tones are
// the core's.
type DetailSection struct {
	Label  string
	Fields []DetailField
	Lines  []DetailLine
}

// DetailField is a label and its value, in the value's tone.
type DetailField struct {
	Label, Value string
	Tone         CardTone
}

// DetailLine is a sentence under a section's fields, in its tone. A bullet
// hangs its wrapped rows under its text.
type DetailLine struct {
	Text   string
	Tone   CardTone
	Bullet bool
}

// BindingLive is whether a binding still carries anything: it never expires,
// or has not yet. One that has lapsed is gone at the server's next sweep, and
// until then it carries nothing, so no list shows it.
func BindingLive(b SecretBinding, now time.Time) bool {
	return b.Expires.IsZero() || b.Expires.After(now)
}

// LiveBindings is the live bindings of one secret, among a project's. They are
// counted when drawn rather than when a listing lands: a binding lapses
// between reads, and a count taken at the read would go on saying a
// credential is in use after nothing may use it.
func LiveBindings(bindings []SecretBinding, secretID string, now time.Time) []SecretBinding {
	var live []SecretBinding
	for _, b := range bindings {
		if b.SecretID == secretID && BindingLive(b, now) {
			live = append(live, b)
		}
	}
	return live
}

// SecretWhere is where a secret's row says it may be sent: its routes, which
// are the whole of what limits where it travels. One with none says so, and
// nowhere is true: blank reads as "not loaded", and this is a fact about the
// secret.
func SecretWhere(s Secret) (where string, nowhere bool) {
	if len(s.Routes) == 0 {
		return "sent nowhere", true
	}
	return RoutesText(s.Routes), false
}

// SecretStanding is what a secret's row says stands on it.
type SecretStanding string

// The standings.
const (
	// SecretUnbound has nothing bound to it.
	SecretUnbound SecretStanding = "unbound"
	// SecretBound has live bindings.
	SecretBound SecretStanding = "bound"
	// SecretRefused is refused by an upstream (ADR 0132).
	SecretRefused SecretStanding = "refused"
)

// SecretStandingOf is what a secret's row says stands on it, as a word and the
// standing it draws in: how many live bindings, or that it is refused. What
// stands on a credential matters less than whether it works, so a refused one
// says that whatever is bound to it.
func SecretStandingOf(live int, refused bool) (string, SecretStanding) {
	switch {
	case refused:
		return "refused", SecretRefused
	case live > 0:
		return count(live, "binding", "bindings"), SecretBound
	}
	return "—", SecretUnbound
}

// RefusedSecrets is which of one server's secrets an upstream refuses, by
// ID, the first rejection of each kept: what a secret's row and its edit say.
// A rejection names a secret on its own server (ADR 0131 §1), so another
// server's never marks this one's.
func RefusedSecrets(session Session, server string, rejections []SecretRejection) map[string]SecretRejection {
	refused := make(map[string]SecretRejection, len(rejections))
	for _, rejection := range rejections {
		if session.ServerName(rejection.Server) != session.ServerName(server) {
			continue
		}
		if _, seen := refused[rejection.SecretID]; !seen {
			refused[rejection.SecretID] = rejection
		}
	}
	return refused
}

// RejectionSummary is one line about a refused credential: where it was
// refused, and why a person is needed rather than another attempt.
func RejectionSummary(rejection SecretRejection) string {
	parts := []string{"refused at " + rejection.Host}
	switch rejection.Reason {
	case "unrefreshable":
		parts = append(parts, "nothing to renew")
	case "refresh-failed":
		parts = append(parts, "renewal refused")
	case "rejected-after-refresh":
		parts = append(parts, "refused again after renewing")
	}
	return strings.Join(parts, " · ")
}

// NoSecrets is what the page says of a project holding none. How to store
// one is each renderer's to add: its key, or where its action is.
const NoSecrets = "no secrets in this project yet"

// BindingsAnswer is what reading a secret says of what is bound to it.
func BindingsAnswer(live int) string {
	if live == 0 {
		return "nothing is bound to it"
	}
	return count(live, "binding stands on it", "bindings stand on it")
}

// BindThisSecret is the row reading a secret leads its bindings with: making
// one, so a credential with nothing bound is somewhere to decide who gets it
// rather than a dead end naming another command.
const (
	BindThisSecret       = "Bind this secret…"
	BindThisSecretDetail = "hand it over before anything asks"
)

// BindingFormNote is what the binding form is about, said above it.
const BindingFormNote = "a binding hands this credential over before anything asks for it"

// EditSecretNote is what the edit form says above it: the one thing it cannot
// change.
const EditSecretNote = "the kind is the one thing it cannot be told: a token and an oauth credential renew differently, so that is a new credential rather than an edit"

// EditSecretAbout is what a secret's edit form says above it, a line each in
// its tone: why an upstream refuses it, when one does (refused) — the reason
// a value is being replaced at all, for the form opened from a refusal —
// then the one thing an edit cannot change.
func EditSecretAbout(refused *SecretRejection) []DetailLine {
	lines := []DetailLine{{Text: EditSecretNote, Tone: CardQuiet}}
	if refused != nil {
		lines = append([]DetailLine{{Text: RejectionSummary(*refused), Tone: CardStandout}}, lines...)
	}
	return lines
}

// DelegationPurpose is what a binding to delegate is for, said the same way on
// a request and on a binding.
const DelegationPurpose = "delegation — may give it to the discoboxes it leads, never uses it"

// RawDeliveryText is what a raw binding hands over (ADR 26-10-27-840), said so
// a person approving it reads that the value itself leaves the pool.
func RawDeliveryText(delivery string) string {
	where := "in its variable"
	if delivery == "file" {
		where = "in a file in memory, its path in the variable"
	}
	return "the value itself, " + where + " — refused off its routes, but the discobox can send it anywhere"
}

// BindingLimit is a secret's ceiling on binding lifetimes, said in the words it
// was chosen in — a limit set as "1 month" reads back as one, not as 720h.
// Zero is not absence: it is the answer "no limit", so it is "forever" rather
// than ShortDuration's "never", which would read as the opposite.
func BindingLimit(d time.Duration) string { return lifetime.Label(d) }

// ShortDuration is the time left on something the way a column has room for:
// "1h", "30m", "never" for one that does not expire. It is for countdowns,
// not for a lifetime somebody chose — those are BindingLimit's.
func ShortDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "never"
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

// DescribeSecret is what a credential is, in the words that can be shown. The
// value is never among them; for an OAuth credential the renewal material is
// not either, and what is left — where it renews, whose grant it is, what it
// may do, when it goes stale — is the part somebody managing it needs.
func DescribeSecret(secret Secret, now time.Time) []DetailSection {
	limit := "none — bindings of it may last forever"
	if secret.MaxTTL > 0 {
		limit = "at most " + BindingLimit(secret.MaxTTL)
	}
	credential := DetailSection{Label: "the credential", Fields: []DetailField{
		{Label: "kind", Value: secret.Type, Tone: CardPlain},
		{Label: "binding limit", Value: limit, Tone: CardPlain},
	}}
	add := func(label, value string, tone CardTone) {
		credential.Fields = append(credential.Fields, DetailField{Label: label, Value: value, Tone: tone})
	}
	// Where it may be sent, whoever holds it (ADR 26-10-27-880 §4). None is
	// nowhere, and says so: a credential no request may carry.
	if len(secret.Routes) == 0 {
		add("may be sent to", "nowhere — it has no routes", CardAlert)
	}
	for i, route := range secret.Routes {
		label := "route"
		if i > 0 {
			label = "or route"
		}
		open := route
		open.Blocked = false
		where := secretroute.Format(open)
		if route.Blocked {
			add(label, where+" · blocked", CardStandout)
		} else {
			add(label, where+" · allowed", CardQuiet)
		}
	}
	// How the value is renewed, for a token that has a lifetime
	// (ADR 26-09-25-122).
	if len(secret.RefreshCommand) > 0 {
		add("renewed by", refreshcmd.Join(secret.RefreshCommand), CardStandout)
	}
	if secret.ValueTTL > 0 {
		add("a value lasts", lifetime.Label(secret.ValueTTL), CardPlain)
	}
	// What stands in for it inside a discobox. A chosen shape is said as one,
	// since it is the one that outlives a new value.
	if secret.Format != "" {
		if secret.FormatSet {
			add("sentinels look like", secret.Format+" · set", CardStandout)
		} else {
			add("sentinels look like", secret.Format+" · read from the value", CardQuiet)
		}
	}
	if !secret.StaleAt.IsZero() {
		stale := "stale " + Ago(secret.StaleAt, now) + "; renewed when next needed"
		if secret.StaleAt.After(now) {
			stale = "stale at " + secret.StaleAt.Local().Format("15:04")
		}
		add("value", stale, CardQuiet)
	}
	if secret.Exchange != nil {
		exchange := DetailSection{Label: "exchanged", Fields: []DetailField{
			{Label: "at", Value: secret.Exchange.URL, Tone: CardPlain},
			{Label: "holds", Value: strings.Join(secret.Exchange.Fields, " "), Tone: CardPlain},
		}}
		if !secret.Exchange.TokenExpiresAt.IsZero() {
			exchange.Fields = append(exchange.Fields, DetailField{Label: "token stale", Value: AccessTokenExpiry(secret.Exchange.TokenExpiresAt, now), Tone: CardPlain})
		}
		return []DetailSection{credential, exchange}
	}
	if secret.OAuth == nil {
		return []DetailSection{credential}
	}

	oauth := DetailSection{Label: "oauth"}
	addOAuth := func(label, value string) {
		if value != "" {
			oauth.Fields = append(oauth.Fields, DetailField{Label: label, Value: value, Tone: CardPlain})
		}
	}
	addOAuth("renews at", secret.OAuth.TokenURL)
	addOAuth("client", secret.OAuth.ClientID)
	addOAuth("may do", strings.Join(secret.OAuth.Scopes, " "))
	addOAuth("plan", secret.OAuth.SubscriptionType)
	if !secret.OAuth.AccessTokenExpiresAt.IsZero() {
		addOAuth("token stale", AccessTokenExpiry(secret.OAuth.AccessTokenExpiresAt, now))
	}
	if !secret.OAuth.Refreshable {
		// The one thing worth saying loudly about an oauth credential: it
		// cannot renew, so it will expire and stay expired.
		oauth.Lines = append(oauth.Lines, DetailLine{Text: "⚠ cannot renew itself: no refresh token or nowhere to spend it", Tone: CardAlert})
	}
	return []DetailSection{credential, oauth}
}

// AccessTokenExpiry says when an access token goes stale, and whether it
// already has — the control plane renews it on the way past, so a stale one
// is ordinary rather than broken.
func AccessTokenExpiry(at, now time.Time) string {
	if at.Before(now) {
		return "expired " + Ago(at, now) + ", renewed on next use"
	}
	return "in " + ShortDuration(at.Sub(now).Round(time.Minute))
}

// BindableBoxes is the boxes a binding of one server's secret may name: those
// listed on that server, since a box on another is not one its secret can
// ever reach.
func BindableBoxes(session Session, server string, boxes []Sandbox) []Sandbox {
	var out []Sandbox
	for _, b := range boxes {
		if session.ServerName(b.Server) == session.ServerName(server) {
			out = append(out, b)
		}
	}
	return out
}

// BindingTarget is what a binding is on, as it is said to a person: the
// project, a harness by name, or a discobox by name, from harnesses and boxes
// the window holds. The wire word for a harness is harnessConfig, because
// that is the resource; a person calls it the harness, and a window that
// says both teaches two names for one thing.
func BindingTarget(b SecretBinding, harnesses []Harness, boxes []Sandbox) string {
	switch b.TargetKind {
	case BindingProject:
		return "every discobox in the project"
	case BindingHarnessConfig:
		for _, h := range harnesses {
			if h.ID == b.TargetID {
				return "harness " + h.Name
			}
		}
		return "harness " + b.TargetID
	case BindingSandbox:
		for _, box := range boxes {
			if box.ID == b.TargetID {
				return "discobox " + box.Name
			}
		}
		return "discobox " + b.TargetID
	}
	return b.TargetKind + " " + b.TargetID
}

// BindingLabel is what a binding's row is named by: the variable it is in,
// which is what two bindings of one secret differ by most often, or that it
// is only given on.
func BindingLabel(b SecretBinding) string {
	switch {
	case b.Use && b.Delegate:
		return b.EnvVar + ", and given on"
	case b.Use && b.RawDelivery != "":
		return b.EnvVar + ", the value itself"
	case b.Use:
		return b.EnvVar
	default:
		return "given on"
	}
}

// BindingDetail is the rest of a binding's row: what it is on (target, from
// BindingTarget), where it may be sent, and how long it has left.
func BindingDetail(b SecretBinding, target string, now time.Time) string {
	parts := []string{target}
	if len(b.Routes) > 0 {
		parts = append(parts, "to "+RoutesText(b.Routes))
	}
	if b.ParentID != "" {
		parts = append(parts, "handed on")
	}
	return strings.Join(append(parts, BindingExpiry(b, now)), " · ")
}

// DescribeBinding is a binding read in full: what it is on, where it may go,
// who made it, and how long it lasts.
func DescribeBinding(secretName string, b SecretBinding, target string, now time.Time) []DetailSection {
	where, tone := "where the secret's routes send it", CardQuiet
	if len(b.Routes) > 0 {
		where, tone = RouteRows(b.Routes)+"\n(within the secret's routes)", CardStandout
	}
	covers := DetailSection{Label: "the binding", Fields: []DetailField{
		{Label: "secret", Value: secretName, Tone: CardStandout},
		{Label: "bound to", Value: target, Tone: CardPlain},
	}}
	add := func(label, value string, tone CardTone) {
		covers.Fields = append(covers.Fields, DetailField{Label: label, Value: value, Tone: tone})
	}
	if b.Use {
		add("as variable", b.EnvVar, CardStandout)
	}
	if b.RawDelivery != "" {
		add("hands over", RawDeliveryText(b.RawDelivery), CardStandout)
	}
	if b.Delegate {
		add("purpose", DelegationPurpose, CardStandout)
	}
	add("may go to", where, tone)
	if b.ParentID != "" {
		add("handed on under", b.ParentID, CardQuiet)
	}
	if b.CreatedBy != "" {
		add("bound by", b.CreatedBy, CardPlain)
	}
	add("expires", BindingLifeLeft(b, now), CardPlain)
	return []DetailSection{covers}
}

// BindingLifeLeft is how long a binding has, for a field already labeled with
// what it is: "expires  expires in 47m" is the row's phrasing put under the
// row's own heading.
func BindingLifeLeft(b SecretBinding, now time.Time) string {
	switch {
	case b.Expires.IsZero():
		return "never"
	case b.Expires.Before(now):
		return "expired"
	default:
		return "in " + ShortDuration(bindingLeft(b, now))
	}
}

// BindingExpiry is how long a binding has, for its row.
func BindingExpiry(b SecretBinding, now time.Time) string {
	switch {
	case b.Expires.IsZero():
		return "never expires"
	case b.Expires.Before(now):
		return "expired"
	default:
		return "expires in " + ShortDuration(bindingLeft(b, now))
	}
}

// bindingLeft is the time a live binding has left, to the minute while there
// are minutes of it. Rounded to the minute under one, half a minute rounds to
// zero, which ShortDuration says as "never": a binding about to lapse read as
// one that never would.
func bindingLeft(b SecretBinding, now time.Time) time.Duration {
	left := b.Expires.Sub(now)
	if left < time.Minute {
		return max(left.Round(time.Second), time.Second)
	}
	return left.Round(time.Minute)
}

// UnbindQuestion is what taking a binding back asks first, saying what stops
// working. It is a destruction: its quick answer is no.
func UnbindQuestion(secretName string, b SecretBinding, target string) Question {
	what := "the binding of " + secretName + " to " + target
	stops := "The credential stops resolving at once, wherever it is being used."
	if b.Use {
		what += " as " + b.EnvVar
	}
	if !b.Use {
		stops = "It can no longer be given on, and what was given on under it goes too."
	}
	return Question{
		Title:     "Unbind",
		Body:      "Take back " + what + "? " + stops + " The request that produced it stays approved, because it is history.",
		DefaultNo: true,
	}
}

// DeleteSecretQuestion is what deleting a secret asks first: how many live
// bindings go with it, and what is left in a discobox holding a sentinel for
// it. Its quick answer is no.
func DeleteSecretQuestion(name string, live int) Question {
	return Question{
		Title: "Delete secret",
		Body: fmt.Sprintf("Delete %s? Its live bindings (%d) are all taken back, and a discobox holding a sentinel for it keeps a placeholder that now resolves to nothing.",
			name, live),
		DefaultNo: true,
	}
}

// SecretChanges is what an edit says it did, a part each: empty for an edit
// that changed nothing, which is neither an error nor a write.
func SecretChanges(was Secret, update SecretUpdate) []string {
	var did []string
	if update.Name != nil {
		did = append(did, "renamed "+was.Name+" to "+*update.Name)
	}
	if update.MaxTTLSeconds != nil {
		limit := BindingLimit(time.Duration(*update.MaxTTLSeconds) * time.Second)
		did = append(did, "bindings of "+was.Name+" now last at most "+limit)
	}
	if update.Routes != nil {
		switch len(*update.Routes) {
		case 0:
			did = append(did, was.Name+" is sent nowhere")
		default:
			did = append(did, was.Name+" is sent to "+RoutesText(*update.Routes))
		}
	}
	if update.Value != nil {
		did = append(did, "replaced "+was.Name+"'s value")
	}
	if update.Format != nil {
		switch *update.Format {
		case "":
			did = append(did, was.Name+"'s sentinels are read from its value again")
		default:
			did = append(did, was.Name+"'s sentinels now look like "+*update.Format)
		}
	}
	if update.ValueTTLSeconds != nil {
		switch *update.ValueTTLSeconds {
		case 0:
			did = append(did, was.Name+"'s value never goes stale")
		default:
			did = append(did, "a value of "+was.Name+" now lasts "+lifetime.Label(time.Duration(*update.ValueTTLSeconds)*time.Second))
		}
	}
	if update.RefreshCommand != nil {
		switch len(*update.RefreshCommand) {
		case 0:
			did = append(did, "removed "+was.Name+"'s refresh command")
		default:
			did = append(did, was.Name+" now renews with "+refreshcmd.Join(*update.RefreshCommand))
		}
	}
	return did
}

// Unchanged is what an edit that changed nothing says.
func Unchanged(name string) string { return name + " is unchanged" }

// Bound is what binding a secret says it did.
func Bound(secretName string, b NewBinding) string {
	did := "bound " + secretName
	if b.Use {
		did += " as " + b.EnvVar
	}
	return did
}

// Unbound is what taking a binding back says it did, with the bindings a
// discobox handed on under it that went too.
func Unbound(handedOn int) string {
	did := "unbound"
	if handedOn > 0 {
		did += ", and " + count(handedOn, "binding", "bindings") + " handed on under it"
	}
	return did
}

// BindingHistoryTitle and BindingHistoryNote head a secret's binding history,
// and NoBindingHistory is what a secret with none says.
const (
	BindingHistoryTitle = "Binding history and answered requests"
	BindingHistoryNote  = "Latest 100 changes"
	NoBindingHistory    = "no binding history"
)

// Label is a decision's row in a binding history: when, what, and who.
func (e BindingDecision) Label() string {
	return e.At.Format(time.RFC3339) + " · " + e.Event + " · " + e.Actor
}

// DescribeDecision is one decision of a binding history read in full: what
// happened and who did it, who granted the binding and when, what it is on,
// and the limits it was made within.
func DescribeDecision(e BindingDecision) []DetailSection {
	label := "Bound by"
	if e.RequestID != "" {
		label = "Approved by"
	}
	expiry := "forever"
	if !e.Expires.IsZero() {
		expiry = e.Expires.Format(time.RFC3339)
	}
	grantor, grantedAt := e.GrantedBy, e.GrantedAt
	if grantor == "" {
		grantor, grantedAt = e.Actor, e.At
	}
	plain := func(label, value string) DetailField { return DetailField{Label: label, Value: value, Tone: CardPlain} }
	fields := []DetailField{
		plain("event", e.Event), plain("event actor", e.Actor), plain("event at", e.At.Format(time.RFC3339)),
		plain(label, grantor), plain("granted at", grantedAt.Format(time.RFC3339)), plain("target ID", e.TargetID),
		plain("variable", e.EnvVar), plain("binding ID", e.BindingID), plain("delegation ID", e.ParentID), plain("expires", expiry),
	}
	if e.RequestID != "" {
		fields = append(fields, plain("answered request", e.RequestID))
	}
	if len(e.RouteLists) == 0 {
		fields = append(fields, plain("effective limits", "not recorded for this historical change"))
	}
	for _, list := range e.RouteLists {
		fields = append(fields, DetailField{Label: "within", Value: RouteRows(list), Tone: CardStandout})
	}
	return []DetailSection{{Label: "the decision", Fields: fields}}
}

// ---------------------------------------------------------------------------
// Acting on a secret. Each reads what it acts on again — the secret, and the
// binding an unbind names — never trusting a listing a console read before,
// as RunHarnessVerb reads its harness: every console takes an action through
// these, and the read comes with it. Each is what the status line says of it.

// OfferedSecret reads a server's secrets again and is the one id names,
// refusing one no longer listed.
func OfferedSecret(ctx context.Context, src DataSource, server, id string) (Secret, error) {
	listed, err := src.Secrets(ctx, server)
	if err != nil {
		return Secret{}, fmt.Errorf("cannot read the secrets: %w", err)
	}
	for _, s := range listed {
		if s.ID == id {
			return s, nil
		}
	}
	return Secret{}, fmt.Errorf("no secret %s is listed", id)
}

// OfferedBinding reads a secret's bindings again and is the live one id
// names, refusing one that is gone or has lapsed: it carries nothing, and the
// server's sweep removes it.
func OfferedBinding(ctx context.Context, src DataSource, server, secretID, id string, now time.Time) (SecretBinding, error) {
	listed, err := src.Bindings(ctx, server, BindingFilter{SecretID: secretID})
	if err != nil {
		return SecretBinding{}, fmt.Errorf("cannot read the bindings: %w", err)
	}
	for _, b := range listed {
		if b.ID == id && BindingLive(b, now) {
			return b, nil
		}
	}
	return SecretBinding{}, errors.New("that binding is no longer there")
}

// StoreSecret stores a credential the new-secret form answered.
func StoreSecret(ctx context.Context, src DataSource, server string, secret NewSecret) (string, error) {
	if _, err := src.CreateSecret(ctx, server, secret); err != nil {
		return "", err
	}
	return "stored " + secret.Name, nil
}

// SaveSecret writes an edit of the secret id back, naming each part that
// changed. One that changed nothing is not written, and says so.
func SaveSecret(ctx context.Context, src DataSource, server, id string, update SecretUpdate) (string, error) {
	was, err := OfferedSecret(ctx, src, server, id)
	if err != nil {
		return "", err
	}
	did := SecretChanges(was, update)
	if len(did) == 0 {
		return Unchanged(was.Name), nil
	}
	if err := src.UpdateSecret(ctx, server, was.ID, update); err != nil {
		return "", err
	}
	return strings.Join(did, ", "), nil
}

// DeleteSecret removes the secret id and every binding of it.
func DeleteSecret(ctx context.Context, src DataSource, server, id string) (string, error) {
	secret, err := OfferedSecret(ctx, src, server, id)
	if err != nil {
		return "", err
	}
	if err := src.DeleteSecret(ctx, server, secret.ID); err != nil {
		return "", err
	}
	return "deleted " + secret.Name, nil
}

// Bind makes a binding the binding form answered, of the secret it names.
func Bind(ctx context.Context, src DataSource, server string, binding NewBinding) (string, error) {
	secret, err := OfferedSecret(ctx, src, server, binding.SecretID)
	if err != nil {
		return "", err
	}
	if _, err := src.CreateBinding(ctx, server, binding); err != nil {
		return "", err
	}
	return Bound(secret.Name, binding), nil
}

// Unbind takes one of a secret's live bindings back, with what a discobox
// handed on under it.
func Unbind(ctx context.Context, src DataSource, server, secretID, bindingID string) (string, error) {
	b, err := OfferedBinding(ctx, src, server, secretID, bindingID, time.Now())
	if err != nil {
		return "", err
	}
	handedOn, err := src.DeleteBinding(ctx, server, b.ID)
	if err != nil {
		return "", err
	}
	return Unbound(handedOn), nil
}
