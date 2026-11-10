package console

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
)

// The credential inbox's rules (ADR 0031, 876 §1): what a request waiting on
// a person is called, what its row and a workspace's notice say, the cards
// that answer a trust, an adoption, an API access and a refresh request, and
// the calls that answer each. The TUI's dialogs and the desktop's sheets draw
// them; neither decides any of it. Nothing here decides anything the server
// does not: a card collects an answer, and the server makes the binding, so a
// window and `discobox secret request approve` make the same one from the same
// checks. A credential request's own cards are approval.go's.

// RequestKind is what a request waits on a person for.
type RequestKind string

// The request kinds.
const (
	// RequestCredential is a credential the box asked for.
	RequestCredential RequestKind = "credential"
	// RequestRefresh is a new value for a token the project holds.
	RequestRefresh RequestKind = "refresh"
	// RequestTrust is a host whose certificate the box asked to trust.
	RequestTrust RequestKind = "trust"
	// RequestAdoption is a box asking to lead another.
	RequestAdoption RequestKind = "adoption"
	// RequestAPIAccess is a box asking for access to the Discobox API.
	RequestAPIAccess RequestKind = "apiAccess"
)

// Kind is what the request waits on a person for.
func (r CredentialRequest) Kind() RequestKind {
	switch {
	case r.APIAccess:
		return RequestAPIAccess
	case r.Adoption != nil:
		return RequestAdoption
	case r.Trust != nil:
		return RequestTrust
	case r.Refresh != nil:
		return RequestRefresh
	}
	return RequestCredential
}

// RequestName is what is asked for, on one line: the credential an agent
// names, or what a trust, an adoption or an API access is of.
func RequestName(req CredentialRequest) string {
	switch req.Kind() {
	case RequestAPIAccess:
		return "discobox API access"
	case RequestAdoption:
		if req.Adoption.ChildName != "" {
			return "adoption of " + req.Adoption.ChildName
		}
		return "adoption of " + req.Adoption.ChildID
	case RequestTrust:
		return "trust of " + req.Host
	}
	if req.Name != "" {
		return req.Name
	}
	if req.EnvVar != "" {
		return req.EnvVar
	}
	return "a credential"
}

// RequestSubject is what a request's row says is asked for: the credential an
// agent names, or failing that the variable it wants one in. An ask to
// delegate and an ask to use the same credential may both be waiting, and are
// different questions; one request may ask both.
func RequestSubject(r CredentialRequest) string {
	switch r.Kind() {
	case RequestAdoption, RequestAPIAccess:
		return RequestName(r)
	case RequestTrust:
		return "trust this host"
	}
	switch {
	case r.AsksBoth():
		return RequestName(r) + " · to use and to delegate"
	case r.DelegateOnly():
		return RequestName(r) + " · to delegate"
	}
	switch {
	case r.Name != "":
		return r.Name
	case r.EnvVar != "":
		return r.EnvVar
	default:
		return "a credential"
	}
}

// Requester says who is waiting: the agent inside a discobox, or — for a
// value gone stale with no discobox in the middle of using it — the control
// plane that noticed.
func Requester(r CredentialRequest) string {
	if r.SandboxID != "" {
		return "agent · " + ShortID(r.SandboxID)
	}
	return "control plane"
}

// ShortID is an identifier at the width a column has for one: its prefix
// dropped, and at most eight characters.
func ShortID(id string) string {
	if _, rest, ok := strings.Cut(id, "_"); ok {
		id = rest
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Counted is n of a thing, its word chosen for n.
func Counted(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// The requests table's words, on every console's secrets page.
const (
	RequestsTitle    = "Requests"
	NothingWaiting   = "nothing is waiting"
	NothingOnThisBox = "nothing waiting on this discobox"
	// NothingRefusedHere is a refusal's remedy asked of a box no refusal
	// affects.
	NothingRefusedHere = "nothing refused on this discobox"
)

// RequestsWaiting is how the requests table counts its rows: every one is
// somebody blocked.
func RequestsWaiting(n int) string { return Counted(n, "waiting", "waiting") }

// OldestRequest is the request waiting longest, false for none: what a
// console answers from a page with no box under the cursor.
func OldestRequest(requests []CredentialRequest) (CredentialRequest, bool) {
	var oldest *CredentialRequest
	for i := range requests {
		if oldest == nil || requests[i].Created.Before(oldest.Created) {
			oldest = &requests[i]
		}
	}
	if oldest == nil {
		return CredentialRequest{}, false
	}
	return *oldest, true
}

// RequestsOn is the requests waiting on one server's secrets (ADR
// 0131 §2): a request is answered with one of its own server's secrets, so
// another server's page offers none of it. A request no discobox owns — one
// made from the CLI, or by a person — is still waiting on somebody, and is
// kept. Oldest first: the one waiting longest is the one somebody has been
// blocked on longest.
func RequestsOn(session Session, server string, requests []CredentialRequest) []CredentialRequest {
	out := []CredentialRequest{}
	for _, r := range requests {
		if session.ServerName(r.Server) == session.ServerName(server) {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b CredentialRequest) int { return a.Created.Compare(b.Created) })
	return out
}

// RequestArrival is what a notification of a request arriving says: what
// kind of request, from which box, and what it asks for. box is the listed
// box that asked, the zero Sandbox for one no listed box made.
func RequestArrival(req CredentialRequest, box Sandbox) (title, body string) {
	what, _ := RequestNotice([]CredentialRequest{req})
	title = strings.ToUpper(what[:1]) + what[1:]
	from := Requester(req)
	if box.ID != "" {
		from = box.Title()
	}
	body = RequestSubject(req)
	if where := req.Where(); where != "" && req.Kind() == RequestCredential {
		body += " for " + where
	}
	return title, from + " · " + body
}

// RequestNotice is what a workspace's notice says about the requests waiting
// on its box (RequestsFor, oldest first): what is waiting, and on what. Empty
// for none.
func RequestNotice(pending []CredentialRequest) (what, subject string) {
	if len(pending) == 0 {
		return "", ""
	}
	first := pending[0]
	subject = RequestName(first)
	what = "credential request"
	switch first.Kind() {
	case RequestAPIAccess:
		what = "API access request"
	case RequestAdoption:
		what = "adoption request"
	case RequestTrust:
		// The name already says the host, so it is not said twice.
		what = "trust request"
	case RequestRefresh:
		// A token this discobox already has, wanting a new value.
		what = "refresh request"
	default:
		if host := first.Where(); host != "" {
			subject += " for " + host
		}
	}
	if len(pending) == 1 {
		return what, subject
	}
	one, many := "credential request", "credential requests"
	counts := map[RequestKind]int{}
	for _, req := range pending {
		counts[req.Kind()]++
	}
	switch {
	case counts[RequestTrust]+counts[RequestAdoption]+counts[RequestRefresh] == 0:
	case counts[RequestTrust] == len(pending):
		one, many = "trust request", "trust requests"
	case counts[RequestAdoption] == len(pending):
		one, many = "adoption request", "adoption requests"
	case counts[RequestRefresh] == len(pending):
		one, many = "refresh request", "refresh requests"
	default:
		one, many = "request", "requests"
	}
	return Counted(len(pending), one, many), "oldest: " + subject
}

// RequestCall is what pressing a request's notice does.
const RequestCall = "click to answer"

// RejectionHeadline is a refusal's notice: what was refused, and by whom.
//
// It says "refused" rather than "expired" or "invalid" because refused is what
// is actually known — the upstream said no to a credential this side could not
// renew — and because the two words a reader might otherwise supply for
// themselves, *your login inside this box*, are the ones that send them
// somewhere nothing can be fixed.
func RejectionHeadline(rejection SecretRejection) string {
	if rejection.HarnessOwned() {
		return rejection.HarnessConfigName + " sign-in refused"
	}
	return rejection.Name() + " refused"
}

// RejectionCall is what pressing a refusal's notice does, in the words of the
// gesture rather than of a button. A harness's login is replaced in the box,
// so its call says how; any other secret is replaced out here.
func RejectionCall(rejection SecretRejection) string {
	if rejection.HarnessOwned() {
		return "click to sign in again"
	}
	return "click to replace it on this machine"
}

// RequestsBehind is what a refusal's notice says about the requests waiting
// behind it. A request waiting behind a refusal is usually the same event: the
// agent took the 401 and asked for a credential. Saying so stops it being
// answered, which would leave the dead credential bound to the harness and the
// new one belonging to nobody.
func RequestsBehind(n int) string {
	return Counted(n, "request", "requests") + " waiting behind it"
}

// SignInAgain says how a harness's refused shared login is replaced: in a
// discobox, by the harness's own sign-in, and then saved. It is a message
// rather than an action because the action is the person's, in the terminal.
func SignInAgain(rejection SecretRejection) (title, body string) {
	name := rejection.HarnessConfigName
	if name == "" {
		name = "the harness"
	}
	body = name + "'s shared login was refused, and could not be renewed. Every discobox on " + name + " uses it.\n\n" +
		"Sign in again in a new discobox on " + name + ", the way " + name + " signs in anywhere — its own login command, in its terminal. " +
		"A login typed in a box can be read by the code running there, so use one whose code you trust rather than the box this refusal showed in.\n\n" +
		"The new login shows up as a harness change on that box: save it, and it replaces the refused login for every discobox on " + name + ". " +
		"Saving also shares the box's changed settings files, which can run commands in every discobox on " + name + ": read the review before you save."
	return name + " sign-in refused", body
}

// ErrSecretGone is a refusal's remedy with nothing to open: the secret that
// was refused is gone from its server.
var ErrSecretGone = errors.New("the credential is gone")

// RefusedSecret is the secret a refusal is replaced by editing, read again
// from its own server (ADR 0131 §2), never from a list a console read before.
func RefusedSecret(ctx context.Context, src DataSource, session Session, rejection SecretRejection) (Secret, error) {
	secrets, err := src.Secrets(ctx, session.ServerName(rejection.Server))
	if err != nil {
		return Secret{}, err
	}
	for _, secret := range secrets {
		if secret.ID == rejection.SecretID {
			return secret, nil
		}
	}
	return Secret{}, ErrSecretGone
}

// ErrRequestGone is a request that is no longer waiting: somebody answered it,
// or it lapsed.
var ErrRequestGone = errors.New("the request is no longer waiting")

// OfferedRequest is the request with this ID on server, read again: what a
// console answers, never one it read before, which somebody else may have
// answered since.
func OfferedRequest(ctx context.Context, src DataSource, session Session, server, id string) (CredentialRequest, error) {
	requests, err := src.CredentialRequests(ctx)
	if err != nil {
		return CredentialRequest{}, fmt.Errorf("cannot read the requests: %w", err)
	}
	for _, r := range requests {
		if r.ID == id && session.ServerName(r.Server) == session.ServerName(server) {
			return r, nil
		}
	}
	return CredentialRequest{}, ErrRequestGone
}

// RequestSecrets is what a credential or refresh request's cards open on: the
// secrets of its server, and what applies in the use part's variable now,
// since a secret that already holds it is the only answer the server takes
// (HeldBy). A trust, an adoption and an API access read nothing first.
func RequestSecrets(ctx context.Context, src DataSource, req CredentialRequest) ([]Secret, []HeldGrant, error) {
	secrets, err := src.Secrets(ctx, req.Server)
	if err != nil {
		return nil, nil, err
	}
	var held []HeldGrant
	if req.Use != nil && req.EnvVar != "" && req.SandboxID != "" {
		held, err = src.HeldGrants(ctx, req.Server, req.SandboxID, req.EnvVar)
	}
	return secrets, held, err
}

// CardAction is one way a request's card is answered, by its key.
type CardAction string

// The card actions.
const (
	// ActionAccept accepts the card: the one action its form checks.
	ActionAccept CardAction = "accept"
	// ActionDeny answers the request no.
	ActionDeny CardAction = "deny"
	// ActionRaw shows the request as the server sent it, and the card again
	// as it was left.
	ActionRaw CardAction = "raw"
	// ActionRefresh gives a token its new value.
	ActionRefresh CardAction = "refresh"
	// ActionDismiss closes a refresh request without a value.
	ActionDismiss CardAction = "dismiss"
)

// CardOffer is one of a card's actions as it is offered: its key, its label,
// and whether it declines (drawn as a refusal, never the default).
type CardOffer struct {
	Action   CardAction
	Label    string
	Declines bool
}

// Accepts reports whether the action is the card's accept, the one its form
// checks: a refresh's is its refresh.
func (a CardAction) Accepts() bool { return a == ActionAccept || a == ActionRefresh }

// CardActions are the actions a request's card offers, in order: accept or
// refresh first, then the decline, then a credential part's Request JSON when
// the server sent the request's own.
func CardActions(req CredentialRequest, part bool) []CardOffer {
	if req.Kind() == RequestRefresh {
		return []CardOffer{{Action: ActionRefresh, Label: "Refresh"}, {Action: ActionDismiss, Label: "Dismiss", Declines: true}}
	}
	out := []CardOffer{{Action: ActionAccept, Label: "Accept"}, {Action: ActionDeny, Label: "Deny", Declines: true}}
	if part && req.Raw != "" {
		out = append(out, CardOffer{Action: ActionRaw, Label: "Request JSON"})
	}
	return out
}

// LeaveWaiting is what leaving a request's card does, said beside the key
// that leaves it.
const LeaveWaiting = "leave it waiting"

// Answered is what a console says once a request is answered, yes or no. The
// lifetime is in an approval's report because it is the half of what was just
// handed out that nothing else on screen will say again.
func Answered(req CredentialRequest, approved bool, ttl time.Duration) string {
	switch {
	case req.Kind() == RequestAdoption || req.Kind() == RequestAPIAccess:
		if approved {
			return "approved " + RequestName(req)
		}
		return "denied " + RequestName(req)
	case approved:
		return "approved " + RequestName(req) + " for " + lifetime.Label(ttl)
	case req.Kind() == RequestRefresh:
		return "dismissed the refresh of " + RequestName(req)
	}
	return "denied " + RequestName(req)
}

// AnswerFailed is the title of what a failed answer says, above the server's
// own words, which are left whole: they say what to do next.
func AnswerFailed(approved bool) string {
	if approved {
		return "Could not approve"
	}
	return "Could not deny"
}

// AnswerFailedText is a failed answer said on one line, for a console with no
// card to say it on: the server's words, which request still waits, and what
// an approval had already done.
func AnswerFailedText(req CredentialRequest, approved bool, err error, applied []string) string {
	text := AnswerFailed(approved) + ": " + err.Error() + " — " + RequestName(req) + " is still waiting"
	if len(applied) > 0 {
		text += "; already done, and still in place: " + strings.Join(applied, "; ")
	}
	return text
}

// SecretsUnread is what a request's cards say when what they open on could
// not be read.
func SecretsUnread(err error) string {
	return fmt.Sprintf("Cannot read the project's secrets, or what the discobox holds: %v\n\nThe request is still waiting.", err)
}

// RawRequest is a request as the server sent it, shown from its card and
// left back to it.
func RawRequest(req CredentialRequest) Question {
	return Question{Title: "Raw request", Subject: req.ID, Body: req.Raw, Choices: []Choice{{Key: "back", Label: "Back to the request"}}}
}

// StillWaiting says which request is still waiting, under a failed answer.
func StillWaiting(req CredentialRequest) DetailSection {
	switch req.Kind() {
	case RequestAdoption:
		return DetailSection{Label: "still waiting", Fields: []DetailField{{Label: "request", Value: RequestName(req), Tone: CardStandout}}}
	case RequestTrust:
		return DetailSection{Label: "still waiting", Fields: []DetailField{{Label: "trust", Value: req.Host, Tone: CardStandout}}}
	}
	fields := []DetailField{{Label: "credential", Value: RequestName(req), Tone: CardStandout}}
	if where := req.WhereRows(); where != "" {
		fields = append(fields, DetailField{Label: "for", Value: where, Tone: CardStandout})
	}
	return DetailSection{Label: "still waiting", Fields: fields}
}

// AppliedBeforeFailing is what an approval that failed had already done to
// the project's secrets: a credential stored ahead of a binding that did not
// happen, which stays, and is said rather than left behind in silence.
func AppliedBeforeFailing(applied []string) []DetailSection {
	if len(applied) == 0 {
		return nil
	}
	done := DetailSection{Label: "already done, and still in place"}
	for _, change := range applied {
		done.Lines = append(done.Lines, DetailLine{Text: change, Tone: CardAlert, Bullet: true})
	}
	done.Lines = append(done.Lines, DetailLine{}, DetailLine{Text: "it was done for this approval; the secrets page removes it if the binding is not coming", Tone: CardQuiet})
	return []DetailSection{done}
}

// DenyRequest answers a request no: a credential, a trust, an adoption or an
// API access denied, a refresh dismissed. It is a complete answer, not a
// dismissal: the asking agent is waiting on one.
func DenyRequest(ctx context.Context, src DataSource, req CredentialRequest) (string, error) {
	var err error
	switch req.Kind() {
	case RequestAPIAccess:
		err = src.DecideAPIAccessRequest(ctx, req.Server, req.ID, false)
	case RequestAdoption:
		err = src.DecideAdoptionRequest(ctx, req.Server, req.ID, false)
	case RequestTrust:
		err = src.DenyTrustRequest(ctx, req.Server, req.ID)
	case RequestRefresh:
		err = src.DismissRefresh(ctx, req.Server, req.ID)
	default:
		err = src.DenyCredentialRequest(ctx, req.Server, req.ID)
	}
	if err != nil {
		return "", err
	}
	return Answered(req, false, 0), nil
}

// Denying is what a console says while a request is answered no.
func Denying(req CredentialRequest) (title, doing string) {
	switch req.Kind() {
	case RequestAPIAccess:
		return "API access request", "answering…"
	case RequestAdoption:
		return "Adoption request", "answering…"
	case RequestTrust:
		return "Trust request", "denying…"
	case RequestRefresh:
		return "Refresh request", "dismissing…"
	}
	return "Credential request", "denying…"
}

// ApproveAccess answers an adoption or an API access request yes.
func ApproveAccess(ctx context.Context, src DataSource, req CredentialRequest) (string, error) {
	var err error
	switch req.Kind() {
	case RequestAPIAccess:
		err = src.DecideAPIAccessRequest(ctx, req.Server, req.ID, true)
	case RequestAdoption:
		err = src.DecideAdoptionRequest(ctx, req.Server, req.ID, true)
	default:
		return "", fmt.Errorf("a %s request is not answered yes or no", req.Kind())
	}
	if err != nil {
		return "", err
	}
	return Answered(req, true, 0), nil
}

// APIAccessTitle is an API access request's card's title.
const APIAccessTitle = "Allow discobox API access?"

// APIAccessCard is what an API access request is: who asks, why, and what
// accepting gives it.
func APIAccessCard(req CredentialRequest) []DetailSection {
	head := DetailSection{Fields: []DetailField{{Label: "Box", Value: req.SandboxID, Tone: CardStandout}, {Label: "Reason (box's words)", Value: req.Justification}}}
	if req.Server != "" {
		head.Fields = append(head.Fields, DetailField{Label: "Server", Value: req.Server})
	}
	return []DetailSection{
		head,
		{Label: "Project access", Fields: []DetailField{{Label: "Discovery", Value: "List boxes and watch project events"}, {Label: "Create", Value: "Launch boxes within its allowed egress, using project harness configs and their credentials"}}},
		{Label: "Authority over boxes it leads", Fields: []DetailField{{Label: "Terminals/files", Value: "Run commands; read and write files; read output and type input"}, {Label: "Lifecycle", Value: "Start, stop, restart, repair, upgrade and archive"}, {Label: "Source/settings", Value: "Deliver source and edit metadata"}, {Label: "Services/ports", Value: "Inspect and control services; reach HTTP ports"}, {Label: "Credentials", Value: "Answer eligible requests within its delegation"}}},
		{Label: "Limits", Lines: []DetailLine{{Text: "Management covers direct children in this project. Wider-egress children are read-only."}, {Text: "Enabling API access grants no credential. Credential and egress restrictions still apply."}, {Text: "Access stays on until a person turns it off in Settings. Deny leaves access unchanged."}}},
	}
}

// adoptionParties is the child and the box asking to lead it, each named with
// its ID.
func adoptionParties(ask *AdoptionAsk) (child, requester string) {
	child, requester = ask.ChildID, ask.RequesterID
	if ask.ChildName != "" {
		child = fmt.Sprintf("%s (%s)", ask.ChildName, ask.ChildID)
	}
	if ask.RequesterName != "" {
		requester = fmt.Sprintf("%s (%s)", ask.RequesterName, ask.RequesterID)
	}
	return child, requester
}

// AdoptionTitle is an adoption request's card's title.
func AdoptionTitle(req CredentialRequest) string {
	child, requester := adoptionParties(req.Adoption)
	return "Let " + requester + " lead " + child + "?"
}

// AdoptionCard is what an adoption request is: which box asks to lead which,
// why, and the authority accepting hands it.
func AdoptionCard(req CredentialRequest) []DetailSection {
	ask := req.Adoption
	child, requester := adoptionParties(ask)
	out := []DetailSection{{Fields: []DetailField{{Label: "Requesting box", Value: requester, Tone: CardStandout}, {Label: "Target box", Value: child, Tone: CardStandout}, {Label: "Current parent", Value: ask.CurrentParentID}, {Label: "Reason (box's words)", Value: req.Justification}}}}
	if ask.ReadOnly {
		out = append(out, DetailSection{Label: "Read-only authority", Lines: []DetailLine{{Text: "The target has wider egress. The requesting box may only read it.", Tone: CardAlert}, {Text: "All writes and sends into it will be refused."}}})
	} else {
		out = append(out, DetailSection{Label: "Authority over this box", Fields: []DetailField{{Label: "Terminals/files", Value: "Run commands; read and write files; attach to and end execs; read and type into terminals"}, {Label: "Lifecycle", Value: "Start, stop, restart, repair, upgrade and archive"}, {Label: "Settings/source", Value: "Change metadata and deliver its source"}, {Label: "Services/ports", Value: "Inspect and control services; reach ports"}, {Label: "Credentials", Value: "Answer requests within the parent's delegation"}}})
	}
	return append(out, DetailSection{Label: "What accepting changes", Lines: []DetailLine{{Text: requester + " replaces " + ask.CurrentParentID + " as this box's parent.", Tone: CardStandout}, {Text: "The child's credentials stay its own. Commands and file reads may expose existing files and credential sentinels. Authority covers this child alone."}, {Text: "Deny keeps the current parent. Esc leaves the request waiting."}}})
}

// TrustTitle is a trust request's card's title.
func TrustTitle(req CredentialRequest) string { return "Trust " + req.Host + "?" }

// TrustCard is a trust request as answered so far (TrustForm's answers): who
// asks to reach which host, why, the certificate chosen, and what accepting
// does. Nil while no certificate is chosen.
func TrustCard(req CredentialRequest, a Answers) []DetailSection {
	trust, ok := ReadTrust(a)
	if !ok {
		return nil
	}
	pin := trust.Pin
	var cert TrustCertificate
	if req.Trust.SuppliedCA != nil && req.Trust.SuppliedCA.SHA256 == pin.SHA256 {
		cert = *req.Trust.SuppliedCA
	} else {
		for _, c := range req.Trust.Chain {
			if c.SHA256 == pin.SHA256 || c.SPKISHA256 == pin.SHA256 {
				cert = c
				break
			}
		}
	}
	meaning := "Trust certificates verified by this CA for this host and box only."
	hash := "CA SHA-256"
	if pin.Kind == TrustPinLeafSPKI {
		meaning = "Trust this host's public key only. Key rotation requires new consent."
		hash = "Key SHA-256"
	}
	head := DetailSection{Fields: []DetailField{{Label: "Box", Value: req.SandboxID, Tone: CardStandout}, {Label: "Host", Value: req.Host, Tone: CardStandout}, {Label: "Reason (box's words)", Value: req.Justification}}}
	if req.Server != "" {
		head.Fields = append(head.Fields, DetailField{Label: "Server", Value: req.Server})
	}
	return []DetailSection{
		head,
		{Label: "Selected certificate", Fields: []DetailField{{Label: "Subject", Value: cert.Subject}, {Label: "Issuer", Value: cert.Issuer}, {Label: "Valid", Value: fmt.Sprintf("%s to %s", cert.NotBefore.Format(time.DateOnly), cert.NotAfter.Format(time.DateOnly))}, {Label: hash, Value: pin.SHA256, Tone: CardStandout}}, Lines: []DetailLine{{Text: meaning}}},
		{Label: "What accepting changes", Lines: []DetailLine{{Text: "This box may connect to " + req.Host + " using this trust for " + lifetime.Label(trust.TTL) + "."}, {Text: "Trust always expires. Deny leaves this host refused; other hosts and boxes are unaffected."}}},
	}
}

// Trusting is what a console says while a trust is sent.
func Trusting(req CredentialRequest, trust TrustAnswer) string {
	return "trusting " + req.Host + " for " + lifetime.Label(trust.TTL) + "…"
}

// TrustRequest answers a trust request yes, pinning the certificate chosen
// for the asking discobox alone (ADR 0149).
func TrustRequest(ctx context.Context, src DataSource, req CredentialRequest, trust TrustAnswer) (string, error) {
	err := src.ApproveTrustRequest(ctx, req.Server, TrustApproval{RequestID: req.ID, Pin: trust.Pin, TTLSeconds: lifetime.Seconds(trust.TTL)})
	if err != nil {
		return "", err
	}
	return Answered(req, true, trust.TTL), nil
}

// RefreshTitle is a refresh request's card's title.
func RefreshTitle(secret Secret) string { return "Refresh " + secret.Name + "?" }

// RefreshTokenGone is a refresh request whose token is no longer in the
// project: there is nothing to give a value.
const RefreshTokenGone = "The token this request is about is no longer in the project."

// RefreshedSecret is the token a refresh request is about, among a server's
// secrets.
func RefreshedSecret(req CredentialRequest, secrets []Secret) (Secret, bool) {
	for _, secret := range secrets {
		if secret.ID == req.Refresh.SecretID {
			return secret, true
		}
	}
	return Secret{}, false
}

// RefreshCard is a refresh request as answered so far (RefreshForm's
// answers): the token, why it wants a value, the command it would run, and
// what answering does.
func RefreshCard(req CredentialRequest, secret Secret, a Answers, now time.Time) []DetailSection {
	sections := refreshAsk(req, secret, now)
	if refresh := ReadRefresh(a); refresh.Manual {
		sections = append(sections, DetailSection{Label: "Paste a replacement", Lines: []DetailLine{{Text: "No command will run. The masked value replaces the project token."}}})
	} else {
		permission := "Run once, then ask again next time."
		if refresh.Session {
			permission = "Run this exact command again without asking while this console window stays open."
		}
		sections = append(sections, DetailSection{Label: "Command permission", Lines: []DetailLine{{Text: "Runs locally as you, without a shell."}, {Text: permission, Tone: CardStandout}}})
	}
	return append(sections, DetailSection{Label: "What refreshing changes", Lines: []DetailLine{{Text: "Replaces the project credential's value. Existing grants keep their routes and lifetimes."}, {Text: "Dismiss keeps the current value; the request may return when the credential is needed again."}}})
}

// refreshAsk is the request as a person reads it before running something:
// the token, why it wants a value, and — above all — the command.
func refreshAsk(req CredentialRequest, secret Secret, now time.Time) []DetailSection {
	why := "its value is past the lifetime it was given"
	if req.Refresh.Cause == "rejected" {
		why = "an upstream refused its value"
	}
	fields := []DetailField{
		{Label: "token", Value: secret.Name, Tone: CardStandout},
		{Label: "why", Value: why},
	}
	if len(secret.Routes) > 0 {
		fields = append(fields, DetailField{Label: "sent to", Value: RouteRows(secret.Routes)})
	}
	if secret.ValueTTL > 0 {
		fields = append(fields, DetailField{Label: "a value lasts", Value: lifetime.Label(secret.ValueTTL)})
	}
	if req.SandboxID != "" {
		fields = append(fields, DetailField{Label: "needed by", Value: req.SandboxID, Tone: CardQuiet})
	}
	if when := Ago(req.Created, now); when != "" {
		fields = append(fields, DetailField{Label: "asked", Value: when, Tone: CardQuiet})
	}
	if req.Server != "" {
		fields = append(fields, DetailField{Label: "on server", Value: req.Server, Tone: CardQuiet})
	}
	sections := []DetailSection{{Label: "wants a new value", Fields: fields}}
	if len(secret.RefreshCommand) > 0 {
		sections = append(sections, DetailSection{Label: "would run", Lines: []DetailLine{{Text: refreshcmd.Join(secret.RefreshCommand), Tone: CardStandout}}})
	}
	return sections
}

// Renewal is the renewal a refresh card's accepted answer sends for secret:
// the value pasted, or the token's saved command, with the session's
// permission when one was given.
func (r RefreshAnswer) Renewal(req CredentialRequest, secret Secret) Renewal {
	if r.Manual {
		return Renewal{RequestID: req.ID, SecretID: secret.ID, Value: r.Value}
	}
	return Renewal{RequestID: req.ID, SecretID: secret.ID, Command: secret.RefreshCommand, Session: r.Session}
}

// Renewing is what a console says while a renewal is sent.
func Renewing(renewal Renewal) string {
	if len(renewal.Command) > 0 {
		return "running " + refreshcmd.Join(renewal.Command) + "…"
	}
	return "sending the new value…"
}

// RenewingAlready is a renewal asked for while one is out for the request.
func RenewingAlready(name string) string { return name + " is being renewed already" }

// RenewFailedText is a failed renewal said on one line.
func RenewFailedText(name string, err error) string {
	return "Could not renew: " + err.Error() + " — " + name + " is still waiting"
}

// RenewFailed is what a failed renewal says under the command's or the
// server's words: the request is still waiting.
func RenewFailed(name string) DetailSection {
	return DetailSection{Label: "still waiting", Fields: []DetailField{{Label: "token", Value: name, Tone: CardStandout}}}
}

// Renewals are a console's renewals: the permissions it holds and the
// requests a renewal is out for, one at a time per request, whether a person
// answered it or a permission did. A console holds one for as long as it is
// open; the zero value permits nothing and runs nothing.
type Renewals struct {
	Permits RenewalPermits
	running map[string]bool
}

// Start marks a renewal out for req, and reports false when one already is.
func (r *Renewals) Start(req CredentialRequest) bool {
	if r.running[req.ID] {
		return false
	}
	if r.running == nil {
		r.running = map[string]bool{}
	}
	r.running[req.ID] = true
	return true
}

// Due is the refresh requests a permission covers that no renewal is out
// for, each started (Start): what a console renews without asking, as the
// inbox lists them, with the permissions as they stand for the work.
func (r *Renewals) Due(session Session, requests []CredentialRequest) ([]CredentialRequest, RenewalPermits) {
	due := RenewableByPermit(session, r.Permits, requests, r.running)
	for _, req := range due {
		r.Start(req)
	}
	return due, r.Permits.Clone()
}

// RenewalSaid is what a console says once a renewal comes back, and whether
// it says it as a warning; Said false says nothing (a renewal a permission
// found nothing to do for, or somebody else's value written first).
type RenewalSaid struct {
	Text          string
	Warning, Said bool
}

// Finish ends the renewal out for req and says what came of it: name is the
// token, and secret it with its command when known. A permission's renewal
// that failed takes its permit back, so the next ask goes to a person who can
// see why. auto is a renewal a permission made without asking.
func (r *Renewals) Finish(server string, req CredentialRequest, secret Secret, auto bool, err error) RenewalSaid {
	delete(r.running, req.ID)
	switch {
	case errors.Is(err, ErrNotPermitted):
		return RenewalSaid{}
	case errors.Is(err, ErrAlreadyAnswered) && auto:
		return RenewalSaid{}
	case errors.Is(err, ErrAlreadyAnswered):
		return RenewalSaid{Text: secret.Name + " was already renewed", Said: true}
	case err != nil:
		r.Permits.Revoke(server, secret.ID, secret.RefreshCommand)
		return RenewalSaid{Text: "could not renew " + secret.Name + ": " + err.Error(), Warning: true, Said: true}
	}
	return RenewalSaid{Text: "renewed " + secret.Name, Said: true}
}

// RenewalPermits are the refresh commands a person let run again without
// asking, for as long as the console that was told so stays open: each the
// server, the token, and the command exactly as it was allowed. An edited
// command is a different permit, and asks again. A console holds one; the
// zero value permits nothing.
type RenewalPermits map[string]bool

// renewalPermit is what a permission is kept under.
func renewalPermit(server, secretID string, command []string) string {
	return server + "\x00" + secretID + "\x00" + strings.Join(command, "\x00")
}

// Allow permits the command to renew the token again without asking.
func (p *RenewalPermits) Allow(server, secretID string, command []string) {
	if *p == nil {
		*p = RenewalPermits{}
	}
	(*p)[renewalPermit(server, secretID, command)] = true
}

// Revoke takes a permission back: one that failed hands the next ask to a
// person, who can see why, rather than trying again on every beat.
func (p RenewalPermits) Revoke(server, secretID string, command []string) {
	delete(p, renewalPermit(server, secretID, command))
}

// Covers reports whether any permission names this token on this server: a
// request worth reading the token again for.
func (p RenewalPermits) Covers(server, secretID string) bool {
	prefix := server + "\x00" + secretID + "\x00"
	for key := range p {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// Clone is the permissions as they stand, for a renewal run off a console's
// own loop, the only place they change.
func (p RenewalPermits) Clone() RenewalPermits {
	out := make(RenewalPermits, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// Permitted reports whether the command is the one allowed for this token.
func (p RenewalPermits) Permitted(server, secretID string, command []string) bool {
	return p[renewalPermit(server, secretID, command)]
}

// ErrNotPermitted is an automatic renewal that found nothing it may do: the
// token is gone, has no command, or its command is no longer the one allowed.
var ErrNotPermitted = errors.New("no permission covers the token's command")

// RenewByPermit answers a refresh request a permission covers, without
// asking. It reads the token first, because the permission is for a command,
// and the command it would run is the one the token names now. permits is a
// copy (Clone): it is only read. It returns the token it renewed, and
// ErrNotPermitted when no permission applies; ErrAlreadyAnswered is somebody
// else's value written first, a renewed token. A caller revokes the
// permission of one that failed otherwise.
func RenewByPermit(ctx context.Context, src DataSource, permits RenewalPermits, server string, req CredentialRequest) (Secret, error) {
	secrets, err := src.Secrets(ctx, req.Server)
	if err != nil {
		return Secret{}, ErrNotPermitted
	}
	secret, ok := RefreshedSecret(req, secrets)
	if !ok || len(secret.RefreshCommand) == 0 || !permits.Permitted(server, secret.ID, secret.RefreshCommand) {
		return Secret{}, ErrNotPermitted
	}
	return secret, src.RefreshSecret(ctx, req.Server, Renewal{RequestID: req.ID, SecretID: secret.ID, Command: secret.RefreshCommand, Session: true})
}

// RenewableByPermit is the refresh requests a permission may answer: each
// one whose token on its server some permission names, and that no renewal
// is already running for.
func RenewableByPermit(session Session, permits RenewalPermits, requests []CredentialRequest, running map[string]bool) []CredentialRequest {
	var out []CredentialRequest
	if len(permits) == 0 {
		return out
	}
	for _, req := range requests {
		if req.Kind() == RequestRefresh && !running[req.ID] && permits.Covers(session.ServerName(req.Server), req.Refresh.SecretID) {
			out = append(out, req)
		}
	}
	return out
}
