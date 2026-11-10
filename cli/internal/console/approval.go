package console

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/cli/internal/requestrole"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// Answering a credential request (ADR 0031, ADR 26-11-02-022): each part —
// to use the credential, to delegate it — on a card of its own
// (ApprovalPartForm), then a review of what the approval replaces, then one
// approval for every part. Nothing is sent until every part has been reviewed;
// leaving any card leaves the whole request waiting on the server. Every
// console walks the same steps (RequestApproval.Decide) and says the same
// things on each card (PartCard, Review); which dialog or sheet draws them is
// its own.

// RequestApproval is what answering a request will do, gathered one card at a
// time: the secret chosen, how long the binding it makes lasts, and each
// part's answer and scope.
//
// It is a value, copied rather than changed at each step, because every step
// can be gone back from: the card before one is rebuilt from the approval as it
// stood when that card was asked.
type RequestApproval struct {
	Request CredentialRequest
	Secrets []Secret
	Secret  Secret
	// Fresh is a new credential rather than a secret the project holds:
	// Secret is empty, and the new-secret form is filled in after the parts.
	Fresh bool
	TTL   time.Duration
	// Raw is the delivery chosen for the value itself, empty for a sentinel
	// (ADR 26-10-27-840 §2).
	Raw string
	// Use and Delegate are each part's answer, PartApprove or PartDeny, empty
	// for a part the request does not have. PartsChosen marks a card opened
	// after its answers were given.
	Use, Delegate           string
	UseScope, DelegateScope string
	PartsChosen             bool
	// Standing is the asking discobox's own bindings of the chosen secret —
	// what an approved part replaces — and Parents the delegations they were
	// handed on under, read once the secret is chosen (ReadStanding).
	Standing []SecretBinding
	Parents  []SecretBinding
	// Held is what applies in the use part's variable now, from whichever
	// level: what an addition stacks on (ADR 26-11-03-329 §5).
	Held []HeldGrant
	// UseAs is how an approved use part is granted, as the request asked
	// until the person chooses on the use card.
	UseAs bindingplan.As
}

// NewRequestApproval is an answer to req, its parts approved until asked
// otherwise, opening on the secrets of its server and what holds its variable
// (RequestSecrets).
func NewRequestApproval(req CredentialRequest, secrets []Secret, held []HeldGrant) RequestApproval {
	a := RequestApproval{Request: req, Secrets: secrets, Held: held, UseAs: bindingplan.Replace}
	if req.Use != nil || req.Delegate == nil {
		a.Use = PartApprove
	}
	if req.Use != nil && req.Use.Add {
		a.UseAs = bindingplan.Add
	}
	if req.Delegate != nil {
		a.Delegate = PartApprove
	}
	return a
}

// FirstPartDelegates reports whether the first card is the delegation's: a
// request that asks only to delegate.
func (a RequestApproval) FirstPartDelegates() bool {
	return a.Request.Use == nil && a.Request.Delegate != nil
}

// PartAsk is what a part's card opens on.
func (a RequestApproval) PartAsk(delegate bool) ApprovalPartAsk {
	scope := a.UseScope
	if delegate {
		scope = a.DelegateScope
	}
	return ApprovalPartAsk{
		Request: a.Request, Secrets: a.Secrets, Held: a.Held, Delegate: delegate,
		Settled: delegate && a.Use == PartApprove, PartsChosen: a.PartsChosen,
		Secret: a.Secret, Fresh: a.Fresh, TTL: a.TTL, Scope: scope, As: a.UseAs, Raw: a.Raw,
	}
}

// PartForm is a part's card's form (ApprovalPartForm).
func (a RequestApproval) PartForm(delegate bool) Form[ApprovalPart] {
	return ApprovalPartForm(a.PartAsk(delegate))
}

// WithPart is the approval as a part's answers make it, unchecked.
func (a RequestApproval) WithPart(delegate bool, p ApprovalPart) RequestApproval {
	a.Fresh, a.Secret, a.TTL = p.Fresh, p.Secret, p.TTL
	if delegate {
		a.DelegateScope = p.Scope
	} else {
		a.UseScope, a.UseAs, a.Raw = p.Scope, p.As, p.Raw
	}
	return a
}

// ApprovalStep is where a request's answer goes after a part's card.
type ApprovalStep int

// The steps.
const (
	// StepDelegatePart is the delegation's card, after the use part's.
	StepDelegatePart ApprovalStep = iota
	// StepDeny is every part denied: the request is answered no.
	StepDeny
	// StepNewSecret is the new credential's form (NewCredentialAsk), which
	// stores it and approves with it.
	StepNewSecret
	// StepReview is the review of what the approval replaces (ReadStanding,
	// then Review), and then the approval.
	StepReview
)

// Decide takes a part's card's answer — PartApprove with its checked answers,
// or PartDeny with them as they stand (ReadApprovalPart) — and says where the
// answer goes next.
func (a RequestApproval) Decide(delegate bool, decision string, p ApprovalPart) (RequestApproval, ApprovalStep) {
	n := a.WithPart(delegate, p)
	if delegate {
		n.Delegate = decision
	} else {
		n.Use = decision
		if decision == PartDeny {
			n.Raw = ""
		}
	}
	n.PartsChosen = true
	if !delegate && a.Request.Delegate != nil {
		return n, StepDelegatePart
	}
	if n.Use != PartApprove && n.Delegate != PartApprove {
		return n, StepDeny
	}
	if n.Fresh {
		return n, StepNewSecret
	}
	return n, StepReview
}

// UsePlanned is how the use part is planned on the cards: as it will be
// granted, empty when it is denied or not asked.
func (a RequestApproval) UsePlanned() bindingplan.As {
	if a.Use != PartApprove {
		return ""
	}
	return a.UseAs
}

// PartTitle is a part's card's title.
func PartTitle(delegate bool) string {
	if delegate {
		return "Allow credential delegation?"
	}
	return "Allow credential use?"
}

// PartCount is which card of how many a part's is.
func (a RequestApproval) PartCount(delegate bool) string {
	if !a.Request.AsksBoth() {
		return "1 of 1"
	}
	if delegate {
		return "2 of 2"
	}
	return "1 of 2"
}

// ReviewTitle is the review's title.
const ReviewTitle = "Confirm credential access"

// part is the request's part a card is about, or one standing in for it
// with the request's routes.
func (a RequestApproval) part(delegate bool) *RequestPart {
	part := a.Request.Use
	if delegate {
		part = a.Request.Delegate
	}
	if part == nil {
		part = &RequestPart{Routes: a.Request.Routes()}
	}
	return part
}

// PartCard is what a part's card says as answered so far: who asks and why,
// what this part is, the grant's restriction, the roles it asked by, the
// ceilings that still apply, and — for the use part — what accepting changes.
func (a RequestApproval) PartCard(delegate bool, answers Answers) []DetailSection {
	ask := a.PartAsk(delegate)
	answered := ReadApprovalPart(ask, answers)
	next := a.WithPart(delegate, answered)
	holder, pinned := HeldBy(a.Held)
	pinned = pinned && !delegate
	part := a.part(delegate)
	req := a.Request

	head := DetailSection{Fields: []DetailField{{Label: "Box", Value: req.SandboxID, Tone: CardStandout}, {Label: "Reason (box's words)", Value: req.Justification}}}
	if req.Server != "" {
		head.Fields = append(head.Fields, DetailField{Label: "Server", Value: req.Server})
	}
	if !delegate {
		head.Fields = append(head.Fields, DetailField{Label: "Variable", Value: VariableOf(req)})
	}
	out := []DetailSection{head}
	explain := "This box may use the credential."
	if delegate {
		explain = "This box may give access to boxes it leads; delegation grants no use to this box."
	}
	out = append(out, DetailSection{Lines: []DetailLine{{Text: explain}}})
	if pinned {
		out = append(out, DetailSection{Lines: []DetailLine{{Text: VariableOf(req) + " already holds " + HeldSecret(a.Secrets, holder).Name + " in this box; no other credential can be granted there, so it answers this.", Tone: CardStandout}}})
	}
	if delegate && req.Use != nil {
		state := "Use denied. Credential and duration remain editable."
		if ask.Settled {
			state = "Use accepted for this answer. Credential and duration are locked. Nothing has been sent yet."
		}
		out = append(out, DetailSection{Lines: []DetailLine{{Text: state, Tone: CardStandout}}})
	}
	out = append(out, GrantRestriction(answered.Scope, part, secretroute.OpenRouteHosts(part.Routes)))
	for _, role := range part.Roles {
		out = append(out, DetailSection{Label: "Requested role (box's words)", Lines: RoleLines(role)})
	}
	limits := DetailSection{Label: "Credential and parent ceilings", Lines: []DetailLine{{Text: "Every independent restriction still applies. Approval cannot widen a parent delegation."}}}
	if next.Fresh {
		limits.Lines = append(limits.Lines, DetailLine{Text: "Credential restrictions and lifetime limit are chosen on the new credential form."})
	} else {
		limits.Fields = append(limits.Fields, DetailField{Label: next.Secret.Name, Value: RouteRows(next.Secret.Routes)})
		for i, list := range next.Secret.RouteConstraints {
			limits.Fields = append(limits.Fields, DetailField{Label: fmt.Sprintf("Constraint %d", i+1), Value: RouteRows(list)})
		}
		if next.Secret.MaxTTL > 0 {
			limits.Fields = append(limits.Fields, DetailField{Label: "Maximum duration", Value: lifetime.Label(next.Secret.MaxTTL)})
		}
	}
	out = append(out, limits)
	if !delegate {
		effect := "Replace existing use grants in " + VariableOf(req) + " for " + lifetime.Label(next.TTL) + ". The final review shows each affected grant."
		if next.UseAs == bindingplan.Add {
			effect = "Add these routes for " + lifetime.Label(next.TTL) + ". Existing grants keep their routes and lifetimes."
		}
		delivery := DetailLine{Text: RawDeliveryText(next.Raw), Tone: CardAlert}
		if next.Raw == "" {
			delivery = DetailLine{Text: "Placeholder delivery: the box never receives the credential value."}
		}
		out = append(out, DetailSection{Label: "What accepting changes", Fields: []DetailField{{Label: "Variable", Value: VariableOf(req)}}, Lines: []DetailLine{{Text: effect}, delivery}})
	}
	return out
}

// RoleLines is one role a part asked by: its name and arguments, its
// description marked as the discobox's own, what its routes grant, and
// anything a reader must not miss about it.
func RoleLines(role requestrole.Role) []DetailLine {
	head := role.Name
	if args := role.ArgsText(); args != "" {
		head += ": " + args
	}
	lines := []DetailLine{{Text: head, Bullet: true, Tone: CardStandout}}
	if role.Description != "" {
		lines = append(lines, DetailLine{Text: "“" + role.Description + "” — the discobox's words", Tone: CardQuiet})
	}
	lines = append(lines, DetailLine{Text: "grants " + requestrole.Summary(role.Routes)})
	for _, note := range role.Notes() {
		lines = append(lines, DetailLine{Text: note, Tone: CardAlert})
	}
	return lines
}

// ReadStanding reads, once the secret is chosen, what the approval would
// replace: the asking discobox's own bindings of it, the delegations they were
// handed on under, and what holds the use part's variable now. A request no
// discobox made has nothing standing.
func ReadStanding(ctx context.Context, src DataSource, a RequestApproval) (RequestApproval, error) {
	if holder, _ := HeldBy(a.Held); holder.SecretID == a.Secret.ID {
		a.Secret = HeldSecret(a.Secrets, holder)
	}
	for _, secret := range a.Secrets {
		if secret.ID == a.Secret.ID {
			a.Secret = secret
		}
	}
	if a.Request.SandboxID == "" {
		return a, nil
	}
	bindings, err := src.Bindings(ctx, a.Request.Server, BindingFilter{SecretID: a.Secret.ID})
	if err != nil {
		return a, StandingUnread(a, err)
	}
	var standing []SecretBinding
	for _, b := range bindings {
		if b.TargetKind == BindingSandbox && b.TargetID == a.Request.SandboxID {
			standing = append(standing, b)
		}
	}
	var held []HeldGrant
	if a.Request.Use != nil && a.Request.EnvVar != "" {
		if held, err = src.HeldGrants(ctx, a.Request.Server, a.Request.SandboxID, a.Request.EnvVar); err != nil {
			return a, StandingUnread(a, err)
		}
	}
	byID := map[string]SecretBinding{}
	for _, b := range bindings {
		byID[b.ID] = b
	}
	var parents []SecretBinding
	seen := map[string]bool{}
	for _, b := range standing {
		for id := b.ParentID; id != "" && !seen[id]; {
			seen[id] = true
			parent, ok := byID[id]
			if !ok {
				break
			}
			parents = append(parents, parent)
			id = parent.ParentID
		}
	}
	a.Standing, a.Held, a.Parents = standing, held, parents
	return a, nil
}

// StandingUnread is what a review that could not read what it replaces says,
// as a card's body: whose holding, why, and that the request still waits.
func StandingUnread(a RequestApproval, err error) error {
	return standingUnread{secret: a.Secret.Name, err: err}
}

type standingUnread struct {
	secret string
	err    error
}

func (e standingUnread) Error() string {
	return fmt.Sprintf("Cannot read what the discobox holds of %s: %v\n\nThe request is still waiting.", e.secret, e.err)
}

func (e standingUnread) Unwrap() error { return e.err }

// ReadingStanding is what a console says while ReadStanding reads.
func (a RequestApproval) ReadingStanding() string {
	return "reading what the discobox holds of " + a.Secret.Name + "…"
}

// ReadingSecrets is what a console says while RequestSecrets reads.
const ReadingSecrets = "reading the project's secrets…"

// Review is what the approval does, said before it is sent: the binding and
// each part's scope and what it replaces, the parent delegations that cap it,
// what holds the variable now, and that the parts are answered together.
func (a RequestApproval) Review() []DetailSection {
	out := []DetailSection{a.bindingSection()}
	for _, parent := range a.Parents {
		value := RouteRows(parent.Routes)
		if len(parent.Routes) == 0 {
			value = "No additional parent route restriction"
		}
		out = append(out, DetailSection{Label: "Parent delegation ceiling", Fields: []DetailField{{Label: parent.ID, Value: value}, {Label: "Expires", Value: parent.Expires.String()}}})
	}
	if a.Use == PartApprove {
		out = append(out, a.heldSection())
	}
	return append(out, DetailSection{Lines: []DetailLine{{Text: "This submits the use and delegation decisions together. Deny refuses the entire request; Esc leaves it waiting."}}})
}

// bindingSection is what the lifetime is being chosen for, so the review
// says how long *what*: the credential, what answers it, and where it may be
// sent, then each part's scope and what it replaces.
func (a RequestApproval) bindingSection() DetailSection {
	answered := a.Secret.Name
	if a.Fresh {
		answered = "a new credential, filled in on the next card"
	}
	fields := []DetailField{
		{Label: "credential", Value: RequestName(a.Request), Tone: CardStandout},
		{Label: "answered with", Value: answered},
	}
	if a.Request.Delegate != nil {
		fields = append(fields, DetailField{Label: "purpose", Value: PartsPurpose(a.Request), Tone: CardStandout})
	}
	if where := a.Request.WhereRows(); where != "" {
		fields = append(fields, DetailField{Label: "may be sent to", Value: where, Tone: CardStandout})
	}
	for _, kind := range []struct {
		name     string
		answer   string
		delegate bool
	}{{"use", a.Use, false}, {"delegate", a.Delegate, true}} {
		if kind.answer == "" {
			continue
		}
		if kind.answer == PartDeny {
			fields = append(fields, DetailField{Label: kind.name + " part", Value: "denied — what the discobox holds of it stays as it is", Tone: CardQuiet})
			continue
		}
		asked := a.GrantedRoutes(kind.delegate)
		scope := a.UseScope
		if kind.delegate {
			scope = a.DelegateScope
		}
		if scope == "" {
			scope = "routes"
		}
		summary := "Requested routes"
		switch scope {
		case "hosts":
			summary = "Requested hosts · Any method and path on these hosts; any protocol\n" + RouteRows(asked)
		case "anywhere":
			summary = "Anywhere · No grant restriction"
		}
		fields = append(fields, DetailField{Label: kind.name + " scope", Value: summary + "\nStill limited by credential scope", Tone: CardStandout})
		fields = append(fields, ReplacedFields(a.Standing, kind.delegate, a.Request.EnvVar, a.UsePlanned(), asked, a.Secret.Routes)...)
	}
	return DetailSection{Label: "the binding", Fields: fields}
}

// PartsPurpose says what a request with a delegate part asks, said the same
// way on the request and on the binding.
func PartsPurpose(req CredentialRequest) string {
	if req.AsksBoth() {
		return "use it, and " + DelegationPurpose
	}
	return DelegationPurpose
}

// GrantedRoutes are the routes a part's approval grants, by its scope: the
// routes it asked for, its open hosts, or none for anywhere.
func (a RequestApproval) GrantedRoutes(delegate bool) []secretroute.Route {
	part, scope := a.Request.Use, a.UseScope
	if delegate {
		part, scope = a.Request.Delegate, a.DelegateScope
	}
	var routes []secretroute.Route
	if part != nil {
		routes = part.Routes
	} else {
		routes = a.Request.Routes()
	}
	switch scope {
	case "anywhere":
		return nil
	case "hosts":
		return []secretroute.Route{{Hosts: secretroute.OpenRouteHosts(routes)}}
	}
	return routes
}

// ReplacedFields is what an approved part would do to the asking discobox's
// own bindings of the chosen secret (ADR 26-11-02-022 §5): which is kept and
// takes the approval, which go, which lose their use or are split, and
// whether a new one is made. bindingplan decides it, by the rule the server
// applies, so access lost to a replacement is said before the approval, not
// found after it. envVar is the variable the use part asks for; useAs is how
// the use part is granted, empty when it is not approved, since a delegate
// part is applied to what the use part leaves (bindingplan.Answer).
//
// Each binding is a heading row and, under it, a diff of its routes against
// asked, the routes the part approves (RouteDiff). secretRoutes stand in for
// either side that names none.
func ReplacedFields(standing []SecretBinding, delegate bool, envVar string, useAs bindingplan.As, asked, secretRoutes []secretroute.Route) []DetailField {
	held := map[string]SecretBinding{}
	var mine []bindingplan.Binding
	for _, b := range standing {
		held[b.ID] = b
		mine = append(mine, bindingplan.Binding{ID: b.ID, Use: b.Use, Delegate: b.Delegate, Adds: b.Adds, EnvName: b.EnvVar, Created: b.Created})
	}
	if !delegate && useAs == "" {
		useAs = bindingplan.Replace
	}
	use, delegatePlan := bindingplan.Answer(mine, envVar, useAs, delegate)
	plan := use
	if delegate {
		plan = delegatePlan
	}
	if plan.Adds {
		return []DetailField{{Label: "replaces", Value: "nothing: an addition beside what it holds", Tone: CardQuiet}}
	}
	var fields []DetailField
	add := func(value string, t CardTone) {
		label := ""
		if len(fields) == 0 {
			label = "replaces"
		}
		fields = append(fields, DetailField{Label: label, Value: value, Tone: t})
	}
	for _, step := range plan.Steps {
		// Said as the binding stands when this part is applied: a delegate
		// part after a use part reads what the use part left.
		b := held[step.Binding.ID]
		what := "the delegation"
		if step.Binding.Use {
			what = "the one in " + step.Binding.EnvName
		}
		var fate string
		switch step.Fate {
		case bindingplan.Updated:
			fate = "kept, takes this approval"
		case bindingplan.Deleted:
			fate = "goes"
		case bindingplan.LosesUse:
			fate = "loses its use; its delegation stays"
		case bindingplan.Split:
			fate = "stays the delegation; its use moves to a binding of its own in " + b.EnvVar
		}
		// A binding or a part with no routes of its own is sent to the
		// secret's, so the secret's are what it is set against.
		was, now := b.Routes, asked
		if len(was) == 0 {
			was = secretRoutes
		}
		if len(now) == 0 {
			now = secretRoutes
		}
		if len(was) == 0 || len(now) == 0 {
			add(what+", sent to the secret's routes — "+fate, CardAlert)
			continue
		}
		add(what+" — "+fate, CardAlert)
		for _, row := range RouteDiff(was, now, step.Binding.ID == plan.Keep) {
			add(row.Text, row.Tone)
		}
	}
	if len(fields) > 0 && plan.Keep == "" {
		add("and a new binding is made", CardAlert)
	}
	return fields
}

// RouteDiff is was set against now as diff rows: "- " for each route of was
// that now does not name, then, for the binding kept, "+ " for each of now
// that was does not, and before them one row counting the routes both name.
// Only the binding kept takes now, so only it gains, and only it can say its
// routes are the same: one that goes, or loses its use, says how many of its
// routes this approval carries on. Routes are compared as secretroute.Format
// writes them, the way a person reads them. A diff is what a person approving
// is checking for; two long lists side by side leave them to find the
// difference themselves.
func RouteDiff(was, now []secretroute.Route, kept bool) []DetailLine {
	wasText, nowText := FormatRoutes(was), FormatRoutes(now)
	var removed, added []DetailLine
	same := 0
	for _, r := range wasText {
		if slices.Contains(nowText, r) {
			same++
			continue
		}
		removed = append(removed, DetailLine{Text: "- " + r, Tone: CardAlert})
	}
	if kept {
		for _, r := range nowText {
			if !slices.Contains(wasText, r) {
				added = append(added, DetailLine{Text: "+ " + r, Tone: CardGood})
			}
		}
	}
	var count string
	switch {
	case same == 0:
	case kept && len(removed) == 0 && len(added) == 0:
		count = "the same routes"
	case kept:
		count = Counted(same, "route", "routes") + " unchanged"
	case len(removed) == 0:
		count = "every route also in this approval"
	default:
		count = Counted(same, "route", "routes") + " also in this approval"
	}
	var rows []DetailLine
	if count != "" {
		rows = append(rows, DetailLine{Text: "  " + count, Tone: CardQuiet})
	}
	return append(append(rows, removed...), added...)
}

// heldSection is what applies in the use part's variable now, grant by grant
// with its lifetime, then the routes asked that none of them names, compared
// as written (ADR 26-11-03-329 §5), and what replacing would do to each.
func (a RequestApproval) heldSection() DetailSection {
	s := DetailSection{Label: "holds in " + VariableOf(a.Request) + " now"}
	if len(a.Held) == 0 {
		s.Fields = append(s.Fields, DetailField{Label: "nothing", Value: "an addition is the first grant there", Tone: CardQuiet})
	}
	// What a replacement does to each grant is bindingplan's rule, the one the
	// server applies; a lifetime of forever cuts nothing short.
	var until time.Time
	if a.TTL > 0 {
		until = time.Now().Add(a.TTL)
	}
	planned := make([]bindingplan.Held, 0, len(a.Held))
	for _, grant := range a.Held {
		planned = append(planned, bindingplan.Held{ID: grant.BindingID, Adds: grant.Adds, Own: grant.From == BindingSandbox, Expires: grant.Expires})
	}
	fates := map[string]bindingplan.HeldFate{}
	for _, step := range bindingplan.Replacing(planned, until) {
		fates[step.Held.ID] = step.Fate
	}
	var named [][]string
	for _, grant := range a.Held {
		what := "binding"
		if grant.Adds {
			what = "addition"
		}
		value := strings.Join(FormatRoutes(grant.Routes), "; ") + " · no expiry"
		if !grant.Expires.IsZero() {
			value = strings.Join(FormatRoutes(grant.Routes), "; ") + " · until " + grant.Expires.Local().Format("Jan 2 15:04")
		}
		switch grant.From {
		case BindingHarnessConfig:
			value += " · from its harness config"
		case BindingProject:
			value += " · from its project"
		}
		s.Fields = append(s.Fields, DetailField{Label: what, Value: value})
		named = append(named, FormatRoutes(grant.Routes))
		var fate string
		switch fates[grant.BindingID] {
		case bindingplan.Goes:
			fate = "goes"
		case bindingplan.Shadowed:
			fate = "is shadowed for " + lifetime.Label(a.TTL)
		case bindingplan.Shortened:
			fate = "lasts only " + lifetime.Label(a.TTL)
		}
		if fate != "" {
			s.Fields = append(s.Fields, DetailField{Value: "if replaced, it " + fate, Tone: CardAlert})
		}
	}
	var asked []secretroute.Route
	if a.Request.Use != nil {
		asked = a.Request.Use.Routes
	}
	if fresh := bindingplan.NewRoutes(FormatRoutes(asked), named); len(fresh) == 0 {
		s.Fields = append(s.Fields, DetailField{Label: "new routes", Value: "none, as written", Tone: CardQuiet})
	} else {
		s.Fields = append(s.Fields, DetailField{Label: "new routes", Value: strings.Join(fresh, "; ") + " (as written)", Tone: CardStandout})
	}
	return s
}

// Approval is what the approval sends: each part's answer and scope, and the
// lifetime, always said — the default is the secret's own ceiling, which most
// credentials do not have, and a window that stayed quiet about it handed out
// permanent credentials without ever saying the word. secretID is the secret
// that answers, a stored new one's for a fresh credential.
func (a RequestApproval) Approval(secretID string) Approval {
	sent := Approval{
		RequestID: a.Request.ID, SecretID: secretID, TTLSeconds: lifetime.Seconds(a.TTL), Raw: a.Raw,
		Use: a.Use, Delegate: a.Delegate, UseScope: a.UseScope, DelegateScope: a.DelegateScope,
	}
	if a.Use == PartApprove && a.Request.Use != nil {
		// Always said, so the binding made is the one the card showed.
		sent.UseAs = string(a.UseAs)
	}
	return sent
}

// Approving is what a console says while the approval is sent.
func (a RequestApproval) Approving() string {
	switch {
	case a.Raw != "":
		return "approving the value itself for " + lifetime.Label(a.TTL) + "…"
	case a.Use == PartApprove && a.Request.Use != nil && a.UseAs == bindingplan.Add:
		return "approving an addition for " + lifetime.Label(a.TTL) + "…"
	}
	return "approving for " + lifetime.Label(a.TTL) + "…"
}

// ApproveRequest sends the approval the review showed.
func ApproveRequest(ctx context.Context, src DataSource, a RequestApproval) (string, error) {
	if err := src.ApproveCredentialRequest(ctx, a.Request.Server, a.Approval(a.Secret.ID)); err != nil {
		return "", err
	}
	return Answered(a.Request, true, a.TTL), nil
}

// NewCredentialTitle is the new credential's form's title.
//
//nolint:gosec // G101: a card's title, not a credential.
const NewCredentialTitle = "New credential"

// NewCredentialAsk is the new credential's form (NewSecretForm), filled from
// the request — named as it asks, or the first free name when the project
// already holds that one, and with the routes it asks for — every field still
// the person's to change. It comes after the parts, so the token typed into it
// is never held by a card that can be gone back to.
func (a RequestApproval) NewCredentialAsk() NewSecretAsk {
	return NewSecretAsk{Name: RequestName(a.Request), Secrets: a.Secrets, Routes: a.Request.Routes(), Approving: true, BindFor: a.TTL}
}

// NewCredentialCard is what the new credential's form says above it: what it
// approves.
func (a RequestApproval) NewCredentialCard() []DetailSection {
	return []DetailSection{{Label: "approves the request", Fields: []DetailField{
		{Label: "credential", Value: RequestName(a.Request), Tone: CardStandout},
		{Label: "bound for", Value: lifetime.Label(a.TTL), Tone: CardStandout},
	}}}
}

// StoringNew is what a console says while a new credential is stored: a
// secret from a command is run for its first value.
func StoringNew(secret NewSecret) string {
	if secret.RefreshCommand != nil {
		return "running " + refreshcmd.Join(secret.RefreshCommand) + "…"
	}
	return "storing the credential…"
}

// ApproveWithNewSecret stores the new credential and answers with it. A
// refusal after the secret was stored says so in applied: the secret stays,
// and the request still waits.
func ApproveWithNewSecret(ctx context.Context, src DataSource, a RequestApproval, secret NewSecret) (note string, applied []string, err error) {
	stored, err := src.CreateSecret(ctx, a.Request.Server, secret)
	if err != nil {
		return "", nil, err
	}
	approval := a.Approval(stored.ID)
	// A new credential's approval always says how its use is granted, as it
	// always has: empty for a use denied or not asked.
	approval.UseAs = string(a.UsePlanned())
	if err := src.ApproveCredentialRequest(ctx, a.Request.Server, approval); err != nil {
		return "", []string{"the token was stored as the project secret " + stored.Name}, err
	}
	return Answered(a.Request, true, a.TTL), nil, nil
}

// Grant restrictions: the binding's own restriction, not a flattening of the
// credential or parent ceilings, as route tables a person reads before saying
// yes.

type grantRouteRow struct {
	protocol                             int
	host, method, path, operation, names string
	blocked                              bool
}

func grantRouteCells(routes []secretroute.Route) []grantRouteRow {
	var rows []grantRouteRow
	for _, route := range routes {
		base := grantRouteRow{method: strings.Join(route.Methods, ","), blocked: route.Blocked}
		if base.method == "" {
			base.method = "*"
		}
		operations := []string{""}
		switch {
		case route.GraphQL != nil:
			base.protocol = 1
			operations = route.GraphQL.Operations
			if len(operations) == 0 {
				operations = []string{"*"}
			}
			base.names = strings.Join(route.GraphQL.Fields, ",")
			if base.names == "" {
				base.names = "*"
			}
		case route.MCP != nil:
			base.protocol = 2
			base.names = strings.Join(route.MCP.Tools, ",")
			if base.names == "" {
				base.names = "*"
			}
		}
		paths := route.Paths
		if len(paths) == 0 {
			paths = []string{"*"}
		}
		for _, host := range route.Hosts {
			for _, path := range paths {
				for _, op := range operations {
					row := base
					row.host = host
					row.path = path
					row.operation = op
					rows = append(rows, row)
				}
			}
		}
	}
	slices.SortStableFunc(rows, func(a, b grantRouteRow) int {
		if c := cmp.Compare(a.protocol, b.protocol); c != 0 {
			return c
		}
		for _, pair := range [][2]string{{a.host, b.host}, {a.path, b.path}, {a.method, b.method}, {a.operation, b.operation}, {a.names, b.names}} {
			if c := cmp.Compare(pair[0], pair[1]); c != 0 {
				return c
			}
		}
		return cmp.Compare(fmt.Sprint(a.blocked), fmt.Sprint(b.blocked))
	})
	return slices.Compact(rows)
}

// RouteTable is a part's routes as a section: grouped by protocol and host, a
// table of each group's methods, paths and names in columns, aligned for a
// fixed-width face. GraphQL and MCP methods are shown only when one is
// restricted. A set too large to table is listed route by route.
func RouteTable(label string, part *RequestPart) DetailSection {
	sec := DetailSection{Label: label}
	if part == nil {
		sec.Lines = []DetailLine{{Text: "not requested", Tone: CardQuiet}}
		return sec
	}
	if !routeTableFits(part.Routes) {
		sec.Lines = append(sec.Lines, DetailLine{Text: "Large route set · compact notation preserves every host, path and protocol restriction.", Tone: CardQuiet})
		for _, r := range part.Routes {
			tone := CardStandout
			if r.Blocked {
				tone = CardAlert
			}
			sec.Lines = append(sec.Lines, DetailLine{Text: secretroute.Format(r), Tone: tone})
		}
		return sec
	}
	rows := grantRouteCells(part.Routes)
	protocols := []string{"HTTP", "GraphQL", "MCP"}
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && rows[end].protocol == rows[start].protocol && rows[end].host == rows[start].host {
			end++
		}
		group := rows[start:end]
		protocol := group[0].protocol
		if len(sec.Lines) > 0 {
			sec.Lines = append(sec.Lines, DetailLine{})
		}
		sec.Lines = append(sec.Lines, DetailLine{Text: protocols[protocol] + " " + group[0].host, Tone: CardStandout})

		showMethod := protocol == 0 || slices.ContainsFunc(group, func(r grantRouteRow) bool { return r.method != "*" })
		showEffect := slices.ContainsFunc(group, func(r grantRouteRow) bool { return r.blocked })
		headers := []string{}
		if showEffect {
			headers = append(headers, "ACCESS")
		}
		if showMethod {
			headers = append(headers, "METHOD")
		}
		headers = append(headers, "PATH")
		switch protocol {
		case 1:
			headers = append(headers, "OPERATION", "FIELDS")
		case 2:
			headers = append(headers, "TOOLS")
		}
		table := [][]string{headers}
		for _, row := range group {
			cells := []string{}
			if showEffect {
				effect := "ALLOW"
				if row.blocked {
					effect = "DENY"
				}
				cells = append(cells, effect)
			}
			if showMethod {
				cells = append(cells, row.method)
			}
			cells = append(cells, row.path)
			switch protocol {
			case 1:
				cells = append(cells, row.operation, row.names)
			case 2:
				cells = append(cells, row.names)
			}
			table = append(table, cells)
		}
		widths := make([]int, len(headers))
		for _, cells := range table {
			for i, cell := range cells {
				widths[i] = max(widths[i], len(cell))
			}
		}
		for i, cells := range table {
			var text strings.Builder
			text.WriteString("  ")
			for col, cell := range cells {
				if col == len(cells)-1 {
					text.WriteString(cell)
				} else {
					fmt.Fprintf(&text, "%-*s  ", widths[col], cell)
				}
			}
			tone := CardStandout
			if i == 0 {
				tone = CardQuiet
			} else if group[i-1].blocked {
				tone = CardAlert
			}
			sec.Lines = append(sec.Lines, DetailLine{Text: text.String(), Tone: tone})
		}
		start = end
	}
	return sec
}

// GrantRestriction is the binding's own restriction for a part's scope: its
// routes as asked, any protocol on its open hosts (dropping the request's
// blocks), or anywhere.
func GrantRestriction(scope string, part *RequestPart, hosts []string) DetailSection {
	if scope == "routes" {
		return RouteTable("Grant restriction · Requested routes", part)
	}
	sec := DetailSection{Label: "Grant restriction · Requested hosts"}
	if scope == "anywhere" {
		sec.Label = "Grant restriction · Anywhere"
		sec.Lines = []DetailLine{{Text: "Any protocol · any host", Tone: CardStandout}, {Text: "  METHOD  PATH", Tone: CardQuiet}, {Text: "  *       *", Tone: CardStandout}, {}, {Text: "No grant restriction. Still limited by credential scope and any parent delegation.", Tone: CardQuiet}}
		return sec
	}
	for _, host := range hosts {
		if len(sec.Lines) > 0 {
			sec.Lines = append(sec.Lines, DetailLine{})
		}
		sec.Lines = append(sec.Lines, DetailLine{Text: "Any protocol · " + host, Tone: CardStandout}, DetailLine{Text: "  METHOD  PATH", Tone: CardQuiet}, DetailLine{Text: "  *       *", Tone: CardStandout})
	}
	if len(hosts) == 0 {
		sec.Lines = append(sec.Lines, DetailLine{Text: "No open requested hosts. This scope cannot be accepted.", Tone: CardAlert})
	}
	sec.Lines = append(sec.Lines, DetailLine{}, DetailLine{Text: "Any method, path and protocol on these hosts. Request-level blocks are dropped.", Tone: CardQuiet})
	return sec
}

// routeTableFits bounds output before forming a Cartesian product. A small
// request may name thousands of hosts and paths, or repeat a long protocol
// constraint per row. The compact fallback renders each original route
// exactly once.
func routeTableFits(routes []secretroute.Route) bool {
	const maxRows = 2048
	const maxBytes = 256 << 10
	rowsLeft, bytesLeft := maxRows, maxBytes
	var hostWidth, pathWidth, methodWidth, nameWidth, operationWidth int
	longest := func(values []string) int {
		n := 0
		for _, s := range values {
			n = max(n, len(s))
		}
		return n
	}
	joined := func(values []string) int {
		n := len(values)
		for _, s := range values {
			n += len(s)
		}
		return n
	}
	for _, r := range routes {
		hosts, paths, operations := len(r.Hosts), max(len(r.Paths), 1), 1
		names, operationBytes := 0, 0
		if r.GraphQL != nil {
			operations = max(len(r.GraphQL.Operations), 1)
			names = joined(r.GraphQL.Fields)
			operationBytes = longest(r.GraphQL.Operations)
		}
		if r.MCP != nil {
			names += joined(r.MCP.Tools)
		}
		count := hosts
		if count > rowsLeft/paths {
			return false
		}
		count *= paths
		if count > rowsLeft/operations {
			return false
		}
		count *= operations
		hostWidth = max(hostWidth, longest(r.Hosts))
		pathWidth = max(pathWidth, longest(r.Paths))
		methodWidth = max(methodWidth, joined(r.Methods))
		nameWidth = max(nameWidth, names)
		operationWidth = max(operationWidth, operationBytes)
		width := longest(r.Hosts) + longest(r.Paths) + joined(r.Methods) + names + operationBytes + 64
		if count > bytesLeft/max(width, 1) {
			return false
		}
		rowsLeft -= count
		bytesLeft -= count * width
	}
	return (maxRows - rowsLeft) <= maxBytes/max(hostWidth+pathWidth+methodWidth+nameWidth+operationWidth+64, 1)
}
