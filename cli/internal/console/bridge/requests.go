package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The credential inbox, served: a request answered through the core's cards
// (console.RequestApproval, TrustForm, RefreshForm, the access cards), a
// refused credential's remedy, a box's notice and its dismissal, and the
// renewals a permission makes without asking. Every rule is the core's; the
// bridge puts its cards to the app as forms and sends what they answer.

// rekicked is closed when the requests are to be read again now (reread).
func (s *Server) rekicked() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rekick
}

// reread has the requests topic read again now: after a request is
// answered, a notice dismissed, or a renewal comes back.
func (s *Server) reread() {
	s.mu.Lock()
	close(s.rekick)
	s.rekick = make(chan struct{})
	s.mu.Unlock()
}

// notices is each listed box's notice, as the window's dismissals leave it,
// for the boxes with one. The dismissals are pruned against the same read.
func (s *Server) notices(in console.Inbox, boxes []console.Sandbox) []Notice {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dismissed.Prune(in, boxes)
	out := []Notice{}
	for _, b := range boxes {
		n := s.dismissed.NoticeFor(in, b)
		if n.Kind == console.NoNotice {
			continue
		}
		out = append(out, Notice{
			Box: Key(b.Server, b.ID), Kind: n.Kind, Subject: n.Subject, Detail: n.Detail, Behind: n.Behind,
			Call: n.Call, Command: n.Command, Urgent: n.Urgent,
		})
	}
	return out
}

// takeSaid is what the renewals a permission made said since the last
// requests snapshot, which carries it once.
func (s *Server) takeSaid() []Said {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.said
	s.said = nil
	return nonNil(out)
}

// renewByPermit answers the refresh requests a permission covers, once
// each, as the inbox lists them (console.Renewals.Due, RenewByPermit), off the
// requests topic's read. What each did is the core's to say
// (console.Renewals.Finish), in the next snapshot; only a renewal that did
// something has the requests read again at once — one a permission found
// nothing to do for waits for the next beat, or it would be due again at once,
// without end.
func (s *Server) renewByPermit(requests []console.CredentialRequest) {
	s.mu.Lock()
	session := s.session
	due, permits := s.renewals.Due(session, requests)
	s.mu.Unlock()
	for _, req := range due {
		go func() {
			server := session.ServerName(req.Server)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			secret, err := console.RenewByPermit(ctx, s.ds, permits, server, req)
			s.mu.Lock()
			said := s.renewals.Finish(server, req, secret, true, err)
			if said.Said {
				s.said = append(s.said, Said{Text: said.Text, Warning: said.Warning})
			}
			s.mu.Unlock()
			if !errors.Is(err, console.ErrNotPermitted) {
				s.reread()
			}
		}()
	}
}

// cover takes a notice of box down for what a command here settled (a
// harness change saved), as a dismissal of it, and has the requests read
// again so the app's notice follows.
func (s *Server) cover(box console.Sandbox, kind console.NoticeKind, names ...string) {
	s.mu.Lock()
	in := console.Inbox{Session: s.session}
	s.dismissed.Cover(in, kind, box, names...)
	s.mu.Unlock()
	s.reread()
}

// CredentialRequests answers the oldest request waiting on box, as the
// TUI's band and key do.
func (f *frontend) CredentialRequests(box console.Sandbox) outcome {
	if e, ok := f.s.reg.Entry(f.cctx, f.c.Command, []console.Sandbox{box}); ok && !e.Enabled {
		return refused(e.Why)
	}
	return func() Result {
		session, _ := f.s.listed()
		requests, err := f.s.ds.CredentialRequests(f.ctx)
		if err != nil {
			return failed("cannot read the requests: " + err.Error())
		}
		pending := console.RequestsFor(session, box, requests)
		if len(pending) == 0 {
			return failed(console.NothingOnThisBox)
		}
		return f.answer(pending[0])
	}
}

// answerOn answers the request with this ID on server, read again: the
// secrets page's answer action, from its requests table or a notification.
func (f *frontend) answerOn(server, id string) Result {
	session, _ := f.s.listed()
	req, err := console.OfferedRequest(f.ctx, f.s.ds, session, server, id)
	if err != nil {
		return failed(err.Error())
	}
	return f.answer(req)
}

// pageRequests is the requests waiting on a secrets page's server, oldest
// first, each with how long it has waited (console.RequestsOn): what its
// requests table lists. They are read now, never from the inbox's beat: the
// page is read again after a request on it is answered. Requests that cannot
// be read fail the page's read, as its secrets do: an empty table would say
// nothing is waiting.
func (f *frontend) pageRequests(server string, now time.Time) ([]Request, error) {
	requests, err := f.s.ds.CredentialRequests(f.ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot read the requests: %w", err)
	}
	session, boxes := f.s.listed()
	on := console.RequestsOn(session, server, requests)
	out := make([]Request, 0, len(on))
	for _, r := range on {
		key := ""
		if r.SandboxID != "" {
			key = Key(r.Server, r.SandboxID)
		}
		for _, b := range boxes {
			if len(console.RequestsFor(session, b, []console.CredentialRequest{r})) > 0 {
				key = Key(b.Server, b.ID)
			}
		}
		w := wireRequest(r, key)
		w.Age = console.Ago(r.Created, now)
		out = append(out, w)
	}
	return out, nil
}

// cardSheet is a request's card as a form put to the app: its title and
// aside, the card the core says beside its fields for the answers so far, and
// its actions as the core offers them, only the accept checked.
func cardSheet(title, aside string, offers []console.CardOffer, card func(console.Answers) []console.DetailSection) sheet {
	actions := make([]FormAction, 0, len(offers))
	for _, o := range offers {
		actions = append(actions, FormAction{Key: o.Action, Label: o.Label, Declines: o.Declines})
	}
	return sheet{
		title: title, aside: aside, card: card, actions: actions, leave: console.LeaveWaiting,
		checks: func(action console.CardAction) bool { return action.Accepts() },
	}
}

// answered is what came of answering a request: what it says, whether it
// was approved, and, when it failed, what an approval had already done.
type answered struct {
	note     string
	approved bool
	applied  []string
	err      error
}

// answer answers req through the core's cards for its kind, and says what
// came of it. Dismissing a card leaves the request waiting.
func (f *frontend) answer(req console.CredentialRequest) Result {
	var (
		got  answered
		r    Result
		over bool
	)
	switch req.Kind() {
	case console.RequestAPIAccess, console.RequestAdoption:
		r, got.approved, over = f.acceptOrDeny(req)
		if !over && got.approved {
			got.note, got.err = console.ApproveAccess(f.ctx, f.s.ds, req)
		}
	case console.RequestTrust:
		form := console.TrustForm(req)
		sent, ok, err := put(f.ctx, f.s, f.c.ID, cardSheet(console.TrustTitle(req), "", console.CardActions(req, false),
			func(a console.Answers) []console.DetailSection { return console.TrustCard(req, a) }), form, form.Opening())
		if r, over = f.filled(ok, err); !over && sent.action.Accepts() {
			got.approved = true
			f.s.send(Message{Type: TypeProgress, Progress: &Progress{Command: f.c.ID, Text: console.Trusting(req, sent.value)}})
			got.note, got.err = console.TrustRequest(f.ctx, f.s.ds, req, sent.value)
		}
	case console.RequestRefresh:
		return f.refresh(req)
	default:
		got, r, over = f.approve(req)
	}
	if over {
		return r
	}
	if !got.approved && got.err == nil {
		got.note, got.err = console.DenyRequest(f.ctx, f.s.ds, req)
	}
	f.s.reread()
	if got.err != nil {
		return failed(console.AnswerFailedText(req, got.approved, got.err, got.applied))
	}
	return Result{Status: StatusDone, Note: got.note}
}

// acceptOrDeny puts an adoption's or an API access's card, which has
// nothing to answer but yes or no.
func (f *frontend) acceptOrDeny(req console.CredentialRequest) (r Result, approved, over bool) {
	title, card := console.APIAccessTitle, console.APIAccessCard(req)
	if req.Kind() == console.RequestAdoption {
		title, card = console.AdoptionTitle(req), console.AdoptionCard(req)
	}
	got, ok, err := put(f.ctx, f.s, f.c.ID, cardSheet(title, "", console.CardActions(req, false),
		func(console.Answers) []console.DetailSection { return card }), console.Form[struct{}]{}, console.Answers{})
	if r, over := f.filled(ok, err); over {
		return r, false, true
	}
	return Result{}, got.action.Accepts(), false
}

// refresh answers a refresh request: its token's card, a renewal sent, or
// the request dismissed. A run the person allows for the session is
// remembered for as long as the window is open (console.RenewalPermits).
func (f *frontend) refresh(req console.CredentialRequest) Result {
	secrets, _, err := console.RequestSecrets(f.ctx, f.s.ds, req)
	if err != nil {
		return failed(console.SecretsUnread(err))
	}
	secret, ok := console.RefreshedSecret(req, secrets)
	if !ok {
		return failed(console.RefreshTokenGone)
	}
	form := console.RefreshForm(secret)
	got, ok, err := put(f.ctx, f.s, f.c.ID, cardSheet(console.RefreshTitle(secret), "", console.CardActions(req, false),
		func(a console.Answers) []console.DetailSection {
			return console.RefreshCard(req, secret, a, time.Now())
		}), form, form.Opening())
	if r, over := f.filled(ok, err); over {
		return r
	}
	if !got.action.Accepts() {
		note, err := console.DenyRequest(f.ctx, f.s.ds, req)
		f.s.reread()
		if err != nil {
			return failed(console.AnswerFailedText(req, false, err, nil))
		}
		return Result{Status: StatusDone, Note: note}
	}
	session, _ := f.s.listed()
	server := session.ServerName(req.Server)
	f.s.mu.Lock()
	if got.value.Session {
		f.s.renewals.Permits.Allow(server, secret.ID, secret.RefreshCommand)
	}
	started := f.s.renewals.Start(req)
	f.s.mu.Unlock()
	if !started {
		return failed(console.RenewingAlready(secret.Name))
	}
	renewal := got.value.Renewal(req, secret)
	f.s.send(Message{Type: TypeProgress, Progress: &Progress{Command: f.c.ID, Text: console.Renewing(renewal)}})
	err = f.s.ds.RefreshSecret(f.ctx, req.Server, renewal)
	f.s.mu.Lock()
	said := f.s.renewals.Finish(server, req, secret, false, err)
	f.s.mu.Unlock()
	f.s.reread()
	if said.Warning {
		return failed(console.RenewFailedText(secret.Name, err))
	}
	return Result{Status: StatusDone, Note: said.Text}
}

// approve answers a credential request part by part (console.RequestApproval):
// each part's card, a new credential's form when one is asked for, and the
// review of what the approval replaces, then the one approval. Leaving any
// card leaves the request waiting (over, with r); leaving the new
// credential's form goes back to the first card, as nothing has been stored.
// A denial comes back unsent, as not approved.
func (f *frontend) approve(req console.CredentialRequest) (got answered, r Result, over bool) {
	f.s.send(Message{Type: TypeProgress, Progress: &Progress{Command: f.c.ID, Text: console.ReadingSecrets}})
	secrets, held, err := console.RequestSecrets(f.ctx, f.s.ds, req)
	if err != nil {
		return got, failed(console.SecretsUnread(err)), true
	}
	start := console.NewRequestApproval(req, secrets, held)
	a, delegate := start, start.FirstPartDelegates()
	for {
		sent, r, over := f.part(a, delegate)
		if over {
			return got, r, true
		}
		decision, part := console.PartApprove, sent.value
		if !sent.action.Accepts() {
			decision, part = console.PartDeny, console.ReadApprovalPart(a.PartAsk(delegate), sent.answers)
		}
		next, step := a.Decide(delegate, decision, part)
		switch step {
		case console.StepDelegatePart:
			a, delegate = next, true
			continue
		case console.StepDeny:
			return got, Result{}, false
		case console.StepNewSecret:
			secret, ok, err := fill(f.ctx, f.s, f.c.ID, sheet{
				title: console.NewCredentialTitle, action: "Store and approve",
				card: func(console.Answers) []console.DetailSection { return next.NewCredentialCard() },
			}, console.NewSecretForm(next.NewCredentialAsk()))
			if err != nil {
				return got, failed(err.Error()), true
			}
			if !ok {
				a, delegate = start, start.FirstPartDelegates()
				continue
			}
			f.s.send(Message{Type: TypeProgress, Progress: &Progress{Command: f.c.ID, Text: console.StoringNew(secret)}})
			got.approved = true
			got.note, got.applied, got.err = console.ApproveWithNewSecret(f.ctx, f.s.ds, next, secret)
			return got, Result{}, false
		}
		f.s.send(Message{Type: TypeProgress, Progress: &Progress{Command: f.c.ID, Text: next.ReadingStanding()}})
		read, err := console.ReadStanding(f.ctx, f.s.ds, next)
		if err != nil {
			return got, failed(err.Error()), true
		}
		review, ok, err := put(f.ctx, f.s, f.c.ID, cardSheet(console.ReviewTitle, "", console.CardActions(req, false),
			func(console.Answers) []console.DetailSection { return read.Review() }), console.Form[struct{}]{}, console.Answers{})
		if r, over := f.filled(ok, err); over {
			return got, r, true
		}
		if !review.action.Accepts() {
			return got, Result{}, false
		}
		f.s.send(Message{Type: TypeProgress, Progress: &Progress{Command: f.c.ID, Text: read.Approving()}})
		got.approved = true
		got.note, got.err = console.ApproveRequest(f.ctx, f.s.ds, read)
		return got, Result{}, false
	}
}

// part puts one part's card until it is accepted or denied: its Request
// JSON shows the request as the server sent it, then the card again as it
// was left.
func (f *frontend) part(a console.RequestApproval, delegate bool) (formed[console.ApprovalPart], Result, bool) {
	form := a.PartForm(delegate)
	answers := form.Opening()
	for {
		sent, ok, err := put(f.ctx, f.s, f.c.ID, cardSheet(console.PartTitle(delegate), a.PartCount(delegate), console.CardActions(a.Request, true),
			func(x console.Answers) []console.DetailSection { return a.PartCard(delegate, x) }), form, answers)
		if r, over := f.filled(ok, err); over {
			return sent, r, true
		}
		if sent.action != console.ActionRaw {
			return sent, Result{}, false
		}
		answers = sent.answers
		_, err = f.s.ask(f.ctx, f.c.ID, console.RawRequest(a.Request))
		if err != nil && f.ctx.Err() != nil {
			return sent, failed(err.Error()), true
		}
	}
}

// RejectedCredentials is a refused credential's remedy, for the refusal box's
// notice is about (console.Dismissals.ShownRejection): how to sign in again
// for a harness's shared login, said and acknowledged; any other secret's
// edit, through the core's form, opened saying why.
func (f *frontend) RejectedCredentials(box console.Sandbox) outcome {
	if e, ok := f.s.reg.Entry(f.cctx, f.c.Command, []console.Sandbox{box}); ok && !e.Enabled {
		return refused(e.Why)
	}
	return func() Result {
		requests, rejections := f.s.inbox(f.ctx)
		console.SortRejections(rejections)
		session, _ := f.s.listed()
		in := console.Inbox{Session: session, Requests: requests, Rejections: rejections}
		f.s.mu.Lock()
		rejection, ok := f.s.dismissed.ShownRejection(in, box)
		f.s.mu.Unlock()
		if !ok {
			return failed(console.NothingRefusedHere)
		}
		if rejection.HarnessOwned() {
			title, body := console.SignInAgain(rejection)
			_, err := f.s.ask(f.ctx, f.c.ID, console.Question{Title: title, Body: body, Choices: []console.Choice{{Key: "ok", Label: "OK"}}})
			if r, over := f.asked(err); over {
				return r
			}
			return Result{Status: StatusDone}
		}
		secret, err := console.RefusedSecret(f.ctx, f.s.ds, session, rejection)
		if err != nil {
			return failed(rejection.Name() + ": " + err.Error())
		}
		server := session.ServerName(rejection.Server)
		update, ok, err := fill(f.ctx, f.s, f.c.ID, sheet{title: "Edit " + secret.Name, about: aboutText(console.EditSecretAbout(&rejection))}, console.EditSecretForm(secret))
		if r, over := f.filled(ok, err); over {
			return r
		}
		note, err := console.SaveSecret(f.ctx, f.s.ds, server, secret.ID, update)
		if err != nil {
			return failed(err.Error())
		}
		f.s.reread()
		return Result{Status: StatusDone, Note: note}
	}
}

// aboutText is what a form says above its fields, a line to a sentence.
func aboutText(lines []console.DetailLine) string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return strings.Join(out, "\n")
}

// DismissBanner takes down the notice of the box whose workspace the app
// shows, until what it is about changes (console.Dismissals.Dismiss).
func (f *frontend) DismissBanner() outcome {
	return func() Result {
		box := f.cctx.Workspace
		if box == nil {
			return failed(console.NothingToDismiss)
		}
		requests, rejections := f.s.inbox(f.ctx)
		console.SortRejections(rejections)
		session, _ := f.s.listed()
		in := console.Inbox{Session: session, Requests: requests, Rejections: rejections}
		f.s.mu.Lock()
		kind := f.s.dismissed.Dismiss(in, *box)
		f.s.mu.Unlock()
		if kind == console.NoNotice {
			return failed(console.NothingToDismiss)
		}
		f.s.reread()
		return Result{Status: StatusDone}
	}
}
