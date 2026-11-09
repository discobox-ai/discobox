package console

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// The credential inbox's forms: one part of a credential request (use or
// delegation), a trust request, and a refresh request. Each is answered by
// accepting or declining; only accepting is checked, so these forms' Submit is
// the accept, and a renderer that declines reads the answers as they stand
// (ReadApprovalPart) without asking the form.

// ApprovalPartAsk is what one part's card opens on: the request, the
// project's secrets, what applies in the use part's variable now, and the
// answers so far, so a card gone back to opens as it was left.
type ApprovalPartAsk struct {
	Request CredentialRequest
	Secrets []Secret
	Held    []HeldGrant
	// Delegate is the delegation part rather than the use part.
	Delegate bool
	// Settled is a delegation card after its use part was approved: the
	// credential and duration are the use part's, shown locked.
	Settled bool
	// PartsChosen is a card opened again after its answers were given, which
	// opens on TTL rather than on what the request asked for.
	PartsChosen bool

	Secret Secret
	Fresh  bool
	TTL    time.Duration
	Scope  string
	As     bindingplan.As
	Raw    string
}

// ApprovalPart is a part's answers: the credential (a secret the project
// holds, or Fresh for a new one), how long it is granted for, the grant's
// scope, and for the use part how it is granted and delivered.
type ApprovalPart struct {
	Fresh  bool
	Secret Secret
	TTL    time.Duration
	Scope  string
	As     bindingplan.As
	// Raw is the delivery chosen for the value itself, empty for a sentinel
	// (ADR 26-10-27-840 §2).
	Raw string
}

// approvalCustom is the duration option that is the typed field below it.
const approvalCustom = "custom"

// pinnedHolder is the grant the use part's variable is pinned to, when it is.
func (ask ApprovalPartAsk) pinnedHolder() (HeldGrant, bool) {
	holder, pinned := HeldBy(ask.Held)
	return holder, pinned && !ask.Delegate
}

// ApprovalPartForm is one part of a credential request as a card.
//
// A variable the discobox's own bindings already deliver a secret in is
// answered, on the use card, with that secret alone: the server refuses any
// other there (HeldBy), so the credential is locked to it, with no new
// credential offered.
func ApprovalPartForm(ask ApprovalPartAsk) Form[ApprovalPart] {
	holder, pinned := ask.pinnedHolder()
	var credentials []Choice
	if pinned {
		held := HeldSecret(ask.Secrets, holder)
		credentials = []Choice{{Key: held.ID, Label: held.Name, Hint: "Already in " + VariableOf(ask.Request) + " in this box"}}
	} else {
		for _, s := range SecretsForRequest(ask.Secrets, ask.Request) {
			credentials = append(credentials, Choice{Key: s.ID, Label: s.Name, Hint: SecretDetail(s)})
		}
		credentials = append(credentials, Choice{Key: "new", Label: "New credential…", Hint: "Enter its value after reviewing both parts"})
	}
	credential := ask.Secret.ID
	if credential == "" && ask.Fresh {
		credential = "new"
	}

	offered, ttl := ask.Request.AskedTTL, ask.TTL
	if ask.PartsChosen {
		offered = ask.TTL
	} else {
		ttl = ask.Request.AskedTTL
		if ttl <= 0 || ttl%time.Second != 0 {
			ttl = agentcreds.DefaultGrantTTL
		}
	}
	var durations []Choice
	for _, d := range LifetimeChoices(offered) {
		durations = append(durations, Choice{Key: strconv.FormatInt(lifetime.Seconds(d), 10), Label: lifetime.Label(d)})
	}
	durations = append(durations, Choice{Key: approvalCustom, Label: "Custom…", Hint: "Type a duration such as 90m or 3d"})

	fields := []Field{
		{Key: "credential", Label: "Credential", Choices: credentials, Value: credential, Locked: ask.Settled || pinned},
		{Key: "duration", Label: "Duration", Choices: durations, Value: strconv.FormatInt(lifetime.Seconds(ttl), 10), Locked: ask.Settled},
		{Key: "customDuration", Label: "Custom duration", Placeholder: "90m, 3d, 6mo or forever",
			When: func(a Answers) bool { return a.Get("duration") == approvalCustom }, Why: "Choose Custom to type a duration"},
		{Key: "scope", Label: "Scope", Value: ask.Scope, Choices: []Choice{
			{Key: "routes", Label: "Requested routes", Hint: "Keep methods, paths, protocol restrictions and blocked routes"},
			{Key: "hosts", Label: "Requested hosts", Hint: "Any method, path and protocol on open requested hosts; drops request blocks"},
			{Key: "anywhere", Label: "Anywhere", Hint: "No grant restriction; credential and parent limits still apply"}}},
	}
	// Use choices belong to this part alone; delegation never changes delivery.
	if !ask.Delegate {
		fields = append(fields,
			Field{Key: "as", Label: "Grant as", Value: string(ask.As), Choices: []Choice{
				{Key: string(bindingplan.Replace), Label: "Replacement", Hint: "Replaces existing use grants in this variable"},
				{Key: string(bindingplan.Add), Label: "Addition", Hint: "Existing grants remain; these routes have their own lifetime"}}},
			Field{Key: "delivery", Label: "Delivery", Value: ask.Raw, Choices: []Choice{
				{Key: "sentinel", Label: "Placeholder", Hint: "The pool swaps in the value on its permitted routes"},
				{Key: "env", Label: "Value in variable", Hint: "Only if the credential allows raw delivery"},
				{Key: "file", Label: "Value in memory file", Hint: "Only if the credential allows raw delivery"}}})
	}

	return Form[ApprovalPart]{Fields: fields, Submit: func(a Answers) (ApprovalPart, *Refusal) {
		part := ReadApprovalPart(ask, a)
		if a.Get("duration") == approvalCustom {
			if _, err := lifetime.Parse(a.Get("customDuration")); err != nil {
				return part, refuse(err.Error())
			}
		}
		if part.Scope == "hosts" && len(secretroute.OpenRouteHosts(ask.Routes())) == 0 {
			return part, refuse("No open requested hosts; choose Requested routes or Anywhere.")
		}
		if !part.Fresh && !WithinLimit(part.Secret, part.TTL) {
			return part, refuse("Credential allows at most " + lifetime.Label(part.Secret.MaxTTL))
		}
		if !ask.Delegate && part.Raw != "" && (part.Fresh || !part.Secret.AllowRaw || part.As == bindingplan.Add) {
			return part, refuse("Raw delivery requires an existing credential that allows it and a replacement grant.")
		}
		if !ask.Delegate && part.As == bindingplan.Add && !CanAdd(ask.Held, part.Fresh, part.Secret.ID) {
			return part, refuse(VariableOf(ask.Request) + " holds " + HeldSecret(ask.Secrets, holder).Name + "; an addition stacks only on that credential. Choose it, or grant a replacement.")
		}
		return part, nil
	}}
}

// Routes are the routes the asked part grants: its own, or the request's.
func (ask ApprovalPartAsk) Routes() []secretroute.Route {
	part := ask.Request.Use
	if ask.Delegate {
		part = ask.Request.Delegate
	}
	if part == nil {
		return ask.Request.Routes()
	}
	return part.Routes
}

// ReadApprovalPart is what a part's answers say, unchecked: what a card shows
// as it is answered, and what declining it carries on with.
func ReadApprovalPart(ask ApprovalPartAsk, a Answers) ApprovalPart {
	part := ApprovalPart{Fresh: a.Get("credential") == "new", Secret: ask.Secret, TTL: ask.TTL, As: ask.As, Raw: ask.Raw}
	if !ask.Settled {
		part.Secret = Secret{}
		if holder, pinned := ask.pinnedHolder(); pinned {
			part.Secret = HeldSecret(ask.Secrets, holder)
		}
		for _, s := range ask.Secrets {
			if s.ID == a.Get("credential") {
				part.Secret = s
			}
		}
		seconds, _ := strconv.ParseInt(a.Get("duration"), 10, 64)
		part.TTL = time.Duration(seconds) * time.Second
		if a.Get("duration") == approvalCustom {
			part.TTL, _ = lifetime.Parse(a.Get("customDuration"))
		}
	}
	part.Scope = a.Get("scope")
	if !ask.Delegate {
		part.As = bindingplan.As(a.Get("as"))
		part.Raw = a.Get("delivery")
		if part.Raw == "sentinel" {
			part.Raw = ""
		}
	}
	return part
}

// HeldBy is the grant that delivers a secret in the use part's variable now,
// and whether the variable is pinned to it. A grant from the discobox's own
// bindings — a replacement, or an addition stacked on one — pins it: the
// server takes no other secret in that variable from a request, whether added
// or replaced. Otherwise holder is the grant the variable's delivery stands on
// from the discobox's harness config or project, which a replacement shadows
// but an addition only stacks on with the same secret (CanAdd).
func HeldBy(held []HeldGrant) (holder HeldGrant, pinned bool) {
	for _, grant := range held {
		if grant.From == BindingSandbox {
			return grant, true
		}
	}
	for _, grant := range held {
		if !grant.Adds {
			return grant, false
		}
	}
	return HeldGrant{}, false
}

// HeldSecret is the secret holding the variable, as the project's secrets
// list it. One the list does not have — gone, or not readable here — is still
// named, since what the server binds is its ID.
func HeldSecret(secrets []Secret, holder HeldGrant) Secret {
	for _, secret := range secrets {
		if secret.ID == holder.SecretID {
			return secret
		}
	}
	name := holder.SecretName
	if name == "" {
		name = holder.SecretID
	}
	return Secret{ID: holder.SecretID, Name: name}
}

// CanAdd reports whether the use part can be granted as an addition with the
// chosen secret (or a fresh one): an addition stacks on what delivers the
// variable, so it has to be that secret (ADR 26-11-03-329 §4).
func CanAdd(held []HeldGrant, fresh bool, secretID string) bool {
	holder, _ := HeldBy(held)
	return holder.SecretID == "" || (!fresh && holder.SecretID == secretID)
}

// WithinLimit reports whether a binding of secret may last d: the secret caps
// nothing, or d is no longer than its cap. Forever is longer than any.
func WithinLimit(secret Secret, d time.Duration) bool {
	return secret.MaxTTL <= 0 || (d > 0 && d <= secret.MaxTTL)
}

// LifetimeChoices is the presets, with the lifetime the agent asked for in its
// place among them when it is not already one: the option the card opens on
// has to be an option on it. Forever stays last, since nothing an agent asks
// for is longer.
//
// An option's identity is its count of whole seconds, so an ask that is not
// one cannot be an option: it would collide with the one whose count it
// shares — forever's, for anything under a second. Such an ask is dropped
// here and the card opens on agentcreds.DefaultGrantTTL, the same as an ask
// nobody made.
func LifetimeChoices(asked time.Duration) []time.Duration {
	if asked <= 0 || asked%time.Second != 0 || slices.Contains(lifetime.Presets, asked) {
		return lifetime.Presets
	}
	at := len(lifetime.Presets)
	for i, d := range lifetime.Presets {
		if d <= 0 || d > asked {
			at = i
			break
		}
	}
	return slices.Insert(slices.Clone(lifetime.Presets), at, asked)
}

// VariableOf is the variable a request's use part names, or a stand-in.
func VariableOf(req CredentialRequest) string {
	if req.EnvVar != "" {
		return req.EnvVar
	}
	return "its variable"
}

// SecretsForRequest orders the secrets by how likely each is to be the
// answer: the one named what the request asked for first, then the rest as
// listed. Order is the whole of the opinion here — every one of them can be
// chosen.
func SecretsForRequest(secrets []Secret, req CredentialRequest) []Secret {
	out := append([]Secret(nil), secrets...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Name == req.Name && out[j].Name != req.Name
	})
	return out
}

// SecretDetail says what a secret is and where it may be sent. A credential
// that caps how long its bindings may last says so too.
func SecretDetail(secret Secret) string {
	where := "sent nowhere"
	if len(secret.Routes) > 0 {
		where = RoutesText(secret.Routes)
	}
	detail := secretKind(secret) + " · " + where
	if secret.MaxTTL > 0 {
		detail += " · at most " + lifetime.Label(secret.MaxTTL)
	}
	return detail
}

// secretKind is what a secret is, said the way that matters when choosing it:
// a token got from a command is named by the command, which is how its value
// is kept current (ADR 26-09-25-122).
func secretKind(secret Secret) string {
	if len(secret.RefreshCommand) > 0 {
		return "from " + refreshcmd.Join(secret.RefreshCommand)
	}
	return secret.Type
}

// trustLifetimes are the lifetimes a trust is offered for. A trust always
// lapses, so the presets stop at a month, which is also the most the server
// allows.
var trustLifetimes = []time.Duration{time.Hour, lifetime.Day, lifetime.Week, lifetime.Month}

// trustPinOption is one certificate a trust card offers to pin.
type trustPinOption struct {
	pin    TrustPin
	label  string
	detail string
}

// trustPinOptions is every pin the request offers, the server's default
// first: the CA the agent supplied, each CA in the chain, and the leaf's own
// key last, since a key pin breaks the day the host rotates its key.
func trustPinOptions(ask TrustAsk) []trustPinOption {
	var options []trustPinOption
	seen := map[string]bool{}
	add := func(option trustPinOption) {
		key := option.pin.Kind + ":" + option.pin.SHA256
		if seen[key] {
			return
		}
		seen[key] = true
		options = append(options, option)
	}
	if ask.SuppliedCA != nil {
		add(trustPinOption{
			pin:    TrustPin{Kind: TrustPinCA, SHA256: ask.SuppliedCA.SHA256},
			label:  "Trust by the CA the agent supplied",
			detail: ask.SuppliedCA.Subject + " · " + shortFingerprint(ask.SuppliedCA.SHA256),
		})
	}
	for i := len(ask.Chain) - 1; i >= 0; i-- {
		cert := ask.Chain[i]
		if !cert.IsCA {
			continue
		}
		add(trustPinOption{
			pin:    TrustPin{Kind: TrustPinCA, SHA256: cert.SHA256},
			label:  "Trust by its CA",
			detail: cert.Subject + " · " + shortFingerprint(cert.SHA256),
		})
	}
	if len(ask.Chain) > 0 {
		leaf := ask.Chain[0]
		add(trustPinOption{
			pin:    TrustPin{Kind: TrustPinLeafSPKI, SHA256: leaf.SPKISHA256},
			label:  "Trust by its key",
			detail: "breaks when the host rotates its key · " + shortFingerprint(leaf.SPKISHA256),
		})
	}
	for i, option := range options {
		if option.pin == ask.DefaultPin {
			options[0], options[i] = options[i], options[0]
			options[0].detail = "what an approval takes by default · " + options[0].detail
			break
		}
	}
	return options
}

// shortFingerprint is a hash at a width a person compares by eye: the head and
// tail, which is what they check against the one they were told.
func shortFingerprint(hash string) string {
	if len(hash) <= 16 {
		return hash
	}
	return hash[:8] + "…" + hash[len(hash)-8:]
}

// TrustAnswer is an accepted trust request: the certificate pinned, and for
// how long.
type TrustAnswer struct {
	Pin TrustPin
	TTL time.Duration
}

// TrustForm is a trust request as a card: which certificate from the chain the
// pool observed to pin, and for how long — the lifetime the agent asked for
// when it is one a trust may have.
func TrustForm(req CredentialRequest) Form[TrustAnswer] {
	var pins []Choice
	for _, opt := range trustPinOptions(*req.Trust) {
		pins = append(pins, Choice{Key: opt.pin.Kind + trustPinSep + opt.pin.SHA256, Label: opt.label, Hint: opt.detail})
	}
	var durations []Choice
	offered := false
	for _, ttl := range trustLifetimes {
		offered = offered || ttl == req.AskedTTL
		durations = append(durations, Choice{Key: strconv.FormatInt(lifetime.Seconds(ttl), 10), Label: lifetime.Label(ttl)})
	}
	if !offered && req.AskedTTL > 0 && req.AskedTTL <= lifetime.Month && req.AskedTTL%time.Second == 0 {
		durations = append(durations, Choice{Key: strconv.FormatInt(lifetime.Seconds(req.AskedTTL), 10), Label: lifetime.Label(req.AskedTTL)})
	}
	asked := ""
	if req.AskedTTL > 0 {
		asked = strconv.FormatInt(lifetime.Seconds(req.AskedTTL), 10)
	}
	return Form[TrustAnswer]{
		Fields: []Field{
			{Key: "pin", Label: "Trust by", Choices: pins},
			{Key: "duration", Label: "Duration", Choices: durations, Value: asked},
		},
		Submit: func(a Answers) (TrustAnswer, *Refusal) {
			trust, ok := ReadTrust(a)
			if !ok {
				return trust, refuse("Choose a certificate to trust.")
			}
			return trust, nil
		},
	}
}

// trustPinSep joins a pin's kind and hash in the key of the option that
// offers it, so the answer is the pin itself rather than a place in a list.
const trustPinSep = ":"

// ReadTrust is what a trust card's answers say, unchecked: the pin chosen,
// false when the answer names none, and the lifetime.
func ReadTrust(a Answers) (TrustAnswer, bool) {
	seconds, _ := strconv.ParseInt(a.Get("duration"), 10, 64)
	trust := TrustAnswer{TTL: time.Duration(seconds) * time.Second}
	kind, hash, ok := strings.Cut(a.Get("pin"), trustPinSep)
	if !ok || kind == "" || hash == "" {
		return trust, false
	}
	trust.Pin = TrustPin{Kind: kind, SHA256: hash}
	return trust, true
}

// RefreshAnswer is an accepted refresh request: a value pasted in, or the
// token's saved command run — once, or again without asking while the
// console that ran it stays open.
type RefreshAnswer struct {
	Manual  bool
	Value   string
	Session bool
}

// RefreshForm is a refresh request for secret as a card: where its new value
// comes from. A token with no saved command can only be pasted.
func RefreshForm(secret Secret) Form[RefreshAnswer] {
	sources := []Choice{
		{Key: "command", Label: "Run saved command", Hint: "Run locally as you, without a shell"},
		{Key: "manual", Label: "Paste a value", Hint: "Enter a replacement token; the field stays masked"},
	}
	if len(secret.RefreshCommand) == 0 {
		sources = sources[1:]
	}
	return Form[RefreshAnswer]{
		Fields: []Field{
			{Key: "source", Label: "New value", Choices: sources},
			{Key: "permission", Label: "Run permission", Choices: []Choice{
				{Key: "once", Label: "Once"},
				{Key: "session", Label: "While this window is open", Hint: "Run again without asking for this exact credential and command"}},
				When: func(a Answers) bool { return a.Get("source") == "command" }, Why: "No command runs for a pasted value"},
			{Key: "value", Label: "Token", Placeholder: "Paste the replacement token", Masked: true,
				When: func(a Answers) bool { return a.Get("source") == "manual" }, Why: "The command supplies the value"},
		},
		Submit: func(a Answers) (RefreshAnswer, *Refusal) {
			refresh := ReadRefresh(a)
			if refresh.Manual && refresh.Value == "" {
				return refresh, refuse("Enter a replacement token.")
			}
			return refresh, nil
		},
	}
}

// ReadRefresh is what a refresh card's answers say, unchecked: what the card
// shows as it is answered.
func ReadRefresh(a Answers) RefreshAnswer {
	if a.Get("source") == "manual" {
		return RefreshAnswer{Manual: true, Value: a.Get("value")}
	}
	return RefreshAnswer{Session: a.Get("permission") == "session"}
}
