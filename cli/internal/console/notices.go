package console

import "slices"

// A workspace's notice: the one thing it says that is not the terminal a
// person came to watch. There are three, and only one is shown: a refused
// credential to replace, a request a person is waiting on, and a harness
// change the box can save for every box on its harness. How a notice is drawn
// is each renderer's (the TUI's band, the desktop's bar); which one shows,
// what dismissing it covers and when that ends are the rules here.

// NoticeKind is which notice a workspace shows. Their order (NoticeKinds) is
// the precedence.
type NoticeKind string

// The notices.
const (
	// NoNotice is a workspace with nothing to say.
	NoNotice NoticeKind = "none"
	// NoticeRejected is a refused credential the box is affected by
	// (RejectionsFor), answered by its remedy (the rejections command).
	NoticeRejected NoticeKind = "rejected"
	// NoticeRequest is a request the box's agent is waiting on
	// (RequestsFor), answered by the requests command.
	NoticeRequest NoticeKind = "request"
	// NoticeHarnessChanges is a harness change the box captured, reviewed by
	// the review config command.
	NoticeHarnessChanges NoticeKind = "harnessChanges"
)

// NoticeKinds is every notice, in precedence.
//
// **A refused credential outranks the request an agent makes about it.** The
// two are usually the same event: the agent takes the 401, concludes it needs
// a credential, and asks for one — so the request *is* the symptom, and
// showing it hides the cause while offering the one action that cannot help.
// Nothing becomes unreachable by that: both commands still reach both.
//
// **A harness change is last.** It is an offer, not a problem: nothing stops
// while it waits.
var NoticeKinds = []NoticeKind{NoticeRejected, NoticeRequest, NoticeHarnessChanges}

// NoticeCommand is the command that answers a notice: what pressing it does.
func NoticeCommand(kind NoticeKind) string {
	switch kind {
	case NoticeRejected:
		return "rejections"
	case NoticeRequest:
		return "requests"
	case NoticeHarnessChanges:
		return "review config"
	}
	return ""
}

// Inbox is what waits on a person, as a console last read it: the requests
// and the refused credentials on every listed server.
type Inbox struct {
	Session    Session
	Requests   []CredentialRequest
	Rejections []SecretRejection
}

// Wanted reports whether a notice has something to say about box.
//
// A refusal stays up over a harness change, a refused harness login included.
// The refusal is the control plane's record; a pending change is the box's own
// report, and a box that could take a refusal down by reporting one would turn
// "your login was refused" into "click to review and save" in front of the
// person it wants to sign in.
func (in Inbox) Wanted(kind NoticeKind, box Sandbox) bool {
	switch kind {
	case NoticeRejected:
		return len(RejectionsFor(in.Session, box, in.Rejections)) > 0
	case NoticeRequest:
		return len(RequestsFor(in.Session, box, in.Requests)) > 0
	case NoticeHarnessChanges:
		return box.HarnessConfigPending
	}
	return false
}

// Occurrence names what a notice would be about on box, or nothing: the
// refusals by credential, host and when each was first seen; the requests
// waiting; the candidate the box captured. A dismissal is of these, never of
// the notice: dismissing a refused credential must not hide the next one, nor
// a request the one that arrives after it. It is a set because requests and
// refusals arrive and go one at a time.
func (in Inbox) Occurrence(kind NoticeKind, box Sandbox) []string {
	var names []string
	switch kind {
	case NoticeRejected:
		for _, rejection := range RejectionsFor(in.Session, box, in.Rejections) {
			names = append(names, rejection.Occurrence())
		}
	case NoticeHarnessChanges:
		if box.HarnessConfigCandidate != "" {
			names = append(names, box.HarnessConfigCandidate)
		}
	case NoticeRequest:
		for _, req := range RequestsFor(in.Session, box, in.Requests) {
			names = append(names, req.ID)
		}
	}
	return names
}

// Dismissals are the notices a window was told to take down, each on one box
// and covering what it was about then. They are the window's own: nothing is
// sent to the server, and a new window shows every notice that still applies.
// The zero value has dismissed nothing.
type Dismissals map[string][]string

// dismissalKey is a notice on one box, which is its server as well as its ID.
func (in Inbox) dismissalKey(kind NoticeKind, box Sandbox) string {
	return in.Session.ServerName(box.Server) + "\x00" + box.ID + "\x00" + string(kind)
}

// Dismissed reports whether a notice's occurrence on box is one somebody
// dismissed: every name it is about was among those dismissed.
func (d Dismissals) Dismissed(in Inbox, kind NoticeKind, box Sandbox) bool {
	dismissed, ok := d[in.dismissalKey(kind, box)]
	if !ok {
		return false
	}
	now := in.Occurrence(kind, box)
	if len(now) == 0 {
		return false
	}
	for _, name := range now {
		if !slices.Contains(dismissed, name) {
			return false
		}
	}
	return true
}

// Showing is the notice box's workspace shows: the first that has something
// to say and was not dismissed. Dismissing one shows what was queued behind it
// rather than nothing.
func (d Dismissals) Showing(in Inbox, box Sandbox) NoticeKind {
	for _, kind := range NoticeKinds {
		if in.Wanted(kind, box) && !d.Dismissed(in, kind, box) {
			return kind
		}
	}
	return NoNotice
}

// NothingToDismiss is what dismissing says when no notice is shown.
const NothingToDismiss = "no banner to dismiss"

// Dismiss takes the notice box's workspace shows down until what it is about
// changes, and says which it took down: NoNotice for none.
func (d *Dismissals) Dismiss(in Inbox, box Sandbox) NoticeKind {
	kind := d.Showing(in, box)
	if kind == NoNotice {
		return kind
	}
	if *d == nil {
		*d = Dismissals{}
	}
	(*d)[in.dismissalKey(kind, box)] = in.Occurrence(kind, box)
	return kind
}

// Cover adds names to what a notice's dismissal on box covers: what a console
// settled itself, such as a harness change it saved, which the box keeps
// reporting until its next listing.
func (d *Dismissals) Cover(in Inbox, kind NoticeKind, box Sandbox, names ...string) {
	if *d == nil {
		*d = Dismissals{}
	}
	key := in.dismissalKey(kind, box)
	(*d)[key] = append((*d)[key], names...)
}

// Prune forgets what each dismissal covered that is now over, and the
// dismissal once nothing it covered is left, against the boxes listed. It is
// run by everything that reads what the notices are about.
//
// It drops names rather than whole dismissals: something new arriving beside
// what was dismissed brings the notice back for the new one, and what was
// dismissed stays dismissed. A dismissal of a box the listing is not showing
// is kept — a server that has stopped answering has settled nothing.
func (d Dismissals) Prune(in Inbox, listed []Sandbox) {
	for _, box := range listed {
		for _, kind := range NoticeKinds {
			key := in.dismissalKey(kind, box)
			dismissed, ok := d[key]
			if !ok {
				continue
			}
			now := in.Occurrence(kind, box)
			kept := slices.DeleteFunc(slices.Clone(dismissed), func(name string) bool { return !slices.Contains(now, name) })
			if len(kept) == 0 {
				delete(d, key)
				continue
			}
			d[key] = kept
		}
	}
}

// ShownRejection is the refusal a box's notice is about: the oldest one a
// dismissal has not covered. A notice back up for a second refusal is about
// that one — its name, its host, its remedy — and not the first, which
// somebody already dismissed. With nothing dismissed, or everything, it is the
// oldest. Rejections are oldest first (SortRejections).
func (d Dismissals) ShownRejection(in Inbox, box Sandbox) (SecretRejection, bool) {
	all := RejectionsFor(in.Session, box, in.Rejections)
	if len(all) == 0 {
		return SecretRejection{}, false
	}
	dismissed := d[in.dismissalKey(NoticeRejected, box)]
	for _, rejection := range all {
		if !slices.Contains(dismissed, rejection.Occurrence()) {
			return rejection, true
		}
	}
	return all[0], true
}

// SortRejections puts refusals oldest first: the credential that has been
// failing longest is the one somebody has been living with.
func SortRejections(rejections []SecretRejection) {
	slices.SortStableFunc(rejections, func(a, b SecretRejection) int { return a.FirstSeen.Compare(b.FirstSeen) })
}

// Notice is what a box's notice says, the core's words for each renderer to
// draw: what it is about, where, what waits behind it, and what pressing it
// does. Kind is NoNotice for a box with nothing to say.
type Notice struct {
	Kind NoticeKind
	// Subject is the notice's sentence, and Detail what stands beside it (a
	// refusal's host); Behind is what waits behind it, the first thing a
	// narrow window drops.
	Subject, Detail, Behind string
	// Call is what pressing it does, in words.
	Call string
	// Command is the registry command pressing it runs on the box.
	Command string
	// Urgent is a person being waited on now: the one notice a renderer
	// draws as moving.
	Urgent bool
}

// HarnessChangesSubject is what a harness change's notice says: whose
// settings or login the box changed.
func HarnessChangesSubject(box Sandbox) string {
	return HarnessOf(box) + " settings or login changed in this box"
}

// HarnessOf is what a box's harness is called where its changes are said:
// its name, else its slug, else "the harness".
func HarnessOf(box Sandbox) string {
	if box.HarnessName != "" {
		return box.HarnessName
	}
	if box.Harness != "" {
		return box.Harness
	}
	return "the harness"
}

// HarnessChangesCall is what pressing a harness change's notice does.
const HarnessChangesCall = "click to review and save"

// RejectionNotice is what a refusal's notice on box says.
func (in Inbox) RejectionNotice(box Sandbox, rejection SecretRejection) Notice {
	n := Notice{
		Kind: NoticeRejected, Command: NoticeCommand(NoticeRejected),
		Subject: RejectionHeadline(rejection), Detail: rejection.Host, Call: RejectionCall(rejection),
	}
	if waiting := len(RequestsFor(in.Session, box, in.Requests)); waiting > 0 {
		n.Behind = RequestsBehind(waiting)
	}
	return n
}

// NoticeFor is what box's workspace notice says, as the dismissals leave it.
func (d Dismissals) NoticeFor(in Inbox, box Sandbox) Notice {
	return d.NoticeOf(in, d.Showing(in, box), box)
}

// NoticeOf is what box's notice of kind says, shown or not: the band a
// console draws for the kind it shows, every word the core's. A kind with
// nothing to say about box is a Notice of kind NoNotice.
func (d Dismissals) NoticeOf(in Inbox, kind NoticeKind, box Sandbox) Notice {
	if kind == NoNotice || !in.Wanted(kind, box) {
		return Notice{Kind: NoNotice}
	}
	n := Notice{Kind: kind, Command: NoticeCommand(kind)}
	switch kind {
	case NoticeRejected:
		rejection, _ := d.ShownRejection(in, box)
		return in.RejectionNotice(box, rejection)
	case NoticeRequest:
		what, subject := RequestNotice(RequestsFor(in.Session, box, in.Requests))
		n.Subject, n.Detail, n.Call, n.Urgent = what, subject, RequestCall, true
	case NoticeHarnessChanges:
		n.Subject, n.Call = HarnessChangesSubject(box), HarnessChangesCall
	}
	return n
}
