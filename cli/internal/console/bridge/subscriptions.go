package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// auditKeep is how much of an audit timeline a snapshot carries, the most
// recent records: the whole of one is sent on every change.
const auditKeep = 1000

// subscribe starts following a topic. Each subscription reads on its own
// until it is ended — but for the audit timeline, whose subscriptions on one
// box share one read (auditFeed) — and sends a snapshot whenever what it read
// changed.
func (s *Server) subscribe(ctx context.Context, sub Subscribe) {
	serving := ctx
	ctx, cancel := context.WithCancel(ctx)
	s.subMu.Lock()
	s.mu.Lock()
	if old := s.subs[sub.Sub]; old != nil {
		old()
	}
	s.subs[sub.Sub] = cancel
	s.mu.Unlock()
	s.subMu.Unlock()
	s.leaveAudit(sub.Sub)
	f := &follower{s: s, sub: sub, ctx: ctx}
	switch sub.Topic {
	case TopicListing:
		if b := s.cached([]string{sub.Reveal}); sub.Reveal != "" && len(b) == 1 {
			f.filter = console.Revealing(b[0])
		} else {
			f.filter = s.filter(ctx, sub.Filter)
		}
		go f.poll(ctx, console.ListPollEvery, s.kicked, f.listing)
	case TopicResources:
		go f.poll(ctx, console.ListPollEvery, nil, f.resources)
	case TopicRequests:
		go f.poll(ctx, console.ListPollEvery, s.rekicked, f.requests)
	case TopicSessions:
		done := s.followTabs(sub.Box)
		go func() {
			defer done()
			f.about(ctx, func() { f.poll(ctx, console.WorkspacePollEvery, s.kicked, f.sessions) })
		}()
	case TopicProvisioning:
		go f.about(ctx, func() { f.provisioning(ctx) })
	case TopicAudit:
		s.followAudit(serving, f)
	case TopicHeader:
		go f.about(ctx, func() { f.header(ctx) })
	case TopicCommands:
		go f.relisted(ctx, f.commands)
	case TopicPalette:
		go f.relisted(ctx, f.palette)
	case TopicComposer:
		go f.composer(ctx)
	case TopicIssue:
		go f.about(ctx, func() { f.issue(ctx) })
	default:
		f.send(Snapshot{Error: fmt.Sprintf("there is no topic %q", sub.Topic)})
	}
}

// about follows a topic about one box: the box is resolved once, as the
// subscription starts (findBox), and followed on its server from then on.
func (f *follower) about(ctx context.Context, follow func()) {
	box, src, ok := f.s.findBox(ctx, f.sub.Box, f.failed)
	if !ok {
		return
	}
	f.box, f.src = box, src
	follow()
}

// findBox finds the box a topic about one box follows, and the source for
// its server, saying why it could not to failed. A box no server lists is
// said once, and given up on, rather than the listing read again on every
// beat for a box that is gone; one whose server could not be reached is tried
// again every beat until ctx ends.
func (s *Server) findBox(ctx context.Context, key string, failed func(error)) (console.Sandbox, console.DataSource, bool) {
	for {
		box, src, err := s.source(ctx, key)
		if err == nil {
			return box, src, true
		}
		if ctx.Err() != nil {
			return box, nil, false
		}
		failed(err)
		if errors.Is(err, errNotListed) {
			return box, nil, false
		}
		select {
		case <-ctx.Done():
			return box, nil, false
		case <-time.After(console.ListPollEvery):
		}
	}
}

// unsubscribe ends a subscription. Once it returns, nothing more of it is
// sent: a snapshot being written is finished first (Server.subMu).
func (s *Server) unsubscribe(sub uint32) {
	s.subMu.Lock()
	s.mu.Lock()
	cancel := s.subs[sub]
	delete(s.subs, sub)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.subMu.Unlock()
	// Off its feed now, not when something notices it ended: the app's next
	// audit subscription on the box, made at once, must not join a feed
	// this one's leaving is about to end.
	s.leaveAudit(sub)
}

// follower is one subscription: what it last sent, so only a change is sent
// again.
type follower struct {
	s   *Server
	sub Subscribe
	// ctx ends with the subscription; nothing is sent after it has.
	ctx  context.Context
	last []byte
	// box and src are the box a topic about one box follows, and the source
	// for its server.
	box console.Sandbox
	src console.DataSource
	// filter is what a listing subscription narrows the list to.
	filter console.Filter
	// seen is every request the requests topic has sent, nil before its
	// first snapshot: what arrived since is what is new. It only grows: a
	// server that missed one poll contributes none to it, and its requests,
	// listed again, have not arrived.
	seen map[string]bool
}

// send sends a snapshot unless it is the one sent last, or the subscription
// has ended. Every follower sends through here, so this is the one place
// that holds an ended subscription to sending nothing, whatever woke it.
func (f *follower) send(snap Snapshot) {
	snap.Sub, snap.Topic = f.sub.Sub, f.sub.Topic
	data, err := json.Marshal(snap)
	if err != nil || bytes.Equal(data, f.last) {
		return
	}
	f.s.subMu.RLock()
	defer f.s.subMu.RUnlock()
	if f.ctx.Err() != nil {
		return
	}
	f.last = data
	_ = f.s.w.Write(KindJSON, 0, mustMessage(data))
}

// mustMessage wraps a snapshot's JSON as a message, without encoding it
// twice.
func mustMessage(snapshot []byte) []byte {
	var b bytes.Buffer
	b.WriteString(`{"type":"snapshot","snapshot":`)
	b.Write(snapshot)
	b.WriteByte('}')
	return b.Bytes()
}

func (f *follower) failed(err error) { f.send(Snapshot{Error: err.Error()}) }

// poll reads every so often until ctx ends, and at once when kicked closes
// what it returns: the listing kicked, for a topic that follows the boxes
// (Server.kicked), the requests read again (Server.rekicked). Nil is never.
func (f *follower) poll(ctx context.Context, every time.Duration, kicked func() <-chan struct{}, read func(context.Context) (Snapshot, error)) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		var kick <-chan struct{}
		if kicked != nil {
			kick = kicked()
		}
		if snap, err := read(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			f.failed(err)
		} else {
			f.send(snap)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-kick:
		}
	}
}

// listing is the core's list (console.List) over the last listing, narrowed
// to the subscription's filter.
func (f *follower) listing(ctx context.Context) (Snapshot, error) {
	listing, err := f.s.list(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	session, _ := f.s.listed()
	l := console.NewList(session, console.ListStaleAfter)
	l.Filter = f.filter
	l.SetAll(listing.Sandboxes)
	l.SetUnreachable(listing.Unreachable)
	l.SetWaiting(listing.Waiting)
	l.SetListed(listing.Listed)
	out := wireListing(&l)
	return Snapshot{Listing: &out}, nil
}

func (f *follower) resources(ctx context.Context) (Snapshot, error) {
	r, err := f.s.ds.Resources(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Resources: &Resources{
		Known: r.Known, CPUVCPUs: r.CPUVCPUs, CPUCapacity: r.CPUCapacity, MemoryBytes: r.MemoryBytes, MemoryCapacity: r.MemoryCapacity,
		DiskKnown: r.DiskKnown, DiskFreeBytes: r.DiskFreeBytes, DiskDataBytes: r.DiskDataBytes, DiskCacheBytes: r.DiskCacheBytes,
	}}, nil
}

func (f *follower) requests(ctx context.Context) (Snapshot, error) {
	requests, err := f.s.ds.CredentialRequests(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	rejections, err := f.s.ds.SecretRejections(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	console.SortRejections(rejections)
	f.s.noteInbox(requests, rejections)
	f.s.renewByPermit(requests)
	// Which box each is about is the core's rule, against the boxes listed.
	session, boxes := f.s.listed()
	in := console.Inbox{Session: session, Requests: requests, Rejections: rejections}
	out := Requests{
		Requests: make([]Request, 0, len(requests)), Rejections: make([]Rejection, 0, len(rejections)),
		Notices: f.s.notices(in, boxes), Arrived: []Arrival{}, Said: f.s.takeSaid(),
	}
	first := f.seen == nil
	if first {
		f.seen = map[string]bool{}
	}
	for _, r := range requests {
		var asker console.Sandbox
		for _, b := range boxes {
			if len(console.RequestsFor(session, b, []console.CredentialRequest{r})) > 0 {
				asker = b
			}
		}
		// A box the listing does not hold is still named, on its server.
		key := ""
		if r.SandboxID != "" {
			key = Key(r.Server, r.SandboxID)
		}
		if asker.ID != "" {
			key = Key(asker.Server, asker.ID)
		}
		out.Requests = append(out.Requests, wireRequest(r, key))
		if !first && !f.seen[r.ID] {
			title, body := console.RequestArrival(r, asker)
			out.Arrived = append(out.Arrived, Arrival{Request: r.ID, Server: r.Server, Box: key, Title: title, Body: body})
		}
		f.seen[r.ID] = true
	}
	for _, r := range rejections {
		var keys []string
		for _, b := range boxes {
			if len(console.RejectionsFor(session, b, []console.SecretRejection{r})) > 0 {
				keys = append(keys, Key(b.Server, b.ID))
			}
		}
		slices.Sort(keys)
		out.Rejections = append(out.Rejections, wireRejection(r, keys))
	}
	return Snapshot{Requests: &out}, nil
}

// sessions is what the box's workspace opens, by the core's rules, in tab
// order (console.Exec.Before): its services' tabs (console.ListedServiceTabs), the
// primary, and every session the exec listing opens as a tab. A box that is
// not running opens its primary alone.
func (f *follower) sessions(ctx context.Context) (Snapshot, error) {
	box, src := f.box, f.src
	// The primary opens whatever the listing says (console.Workspace's first
	// answer): attaching it is what waits for, and revives, a box not up yet.
	primary := console.Exec{ID: console.ExecPrimary, Primary: true, Live: true}
	execs, err := src.Execs(ctx, box.ID)
	if errors.Is(err, console.ErrNotRunning) {
		return Snapshot{Sessions: &Sessions{Sessions: []Tab{wireTab(console.SessionTab(primary, ""), primary)}}}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	services, err := src.Services(ctx, box.ID)
	if errors.Is(err, console.ErrNotRunning) {
		services, err = nil, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	type listed struct {
		tab  console.Tab
		exec console.Exec
	}
	all := []listed{{console.SessionTab(primary, ""), primary}}
	for _, e := range console.Sessions(execs) {
		all = append(all, listed{console.SessionTab(e, ""), e})
	}
	byID := make(map[string]console.Service, len(services))
	for _, svc := range services {
		byID[svc.ID] = svc
	}
	for _, t := range console.ListedServiceTabs(services) {
		all = append(all, listed{t, console.Exec{ID: t.ExecID, Live: byID[t.Service].Live()}})
	}
	slices.SortStableFunc(all, func(a, b listed) int {
		switch {
		case a.tab.Session().Before(b.tab.Session()):
			return -1
		case b.tab.Session().Before(a.tab.Session()):
			return 1
		}
		return 0
	})
	out := Sessions{Running: true, Sessions: make([]Tab, 0, len(all))}
	tabs := make([]console.Tab, 0, len(all))
	for _, l := range all {
		tabs = append(tabs, l.tab)
		out.Sessions = append(out.Sessions, wireTab(l.tab, l.exec))
	}
	f.s.noteTabs(f.sub.Box, tabs, services)
	return Snapshot{Sessions: &out}, nil
}

// provisioning follows what a box is being made to do, a step at a time,
// until it has nothing left to report or the subscription ends.
func (f *follower) provisioning(ctx context.Context) {
	box, src := f.box, f.src
	f.send(Snapshot{Provisioning: &Provisioning{}})
	n := console.NewNarration()
	go func() {
		src.WatchProvisioning(ctx, box.ID, n)
		n.Close()
	}()
	for ev := range n.Events() {
		switch {
		case ev.Progress != nil:
			f.send(Snapshot{Provisioning: &Provisioning{Step: ev.Progress.Text}})
		case ev.Asking != nil:
			ev.Asking.Decline(console.ErrNoFrontend)
		case ev.Handover != nil:
			ev.Handover.Done(console.ErrNoFrontend)
		}
	}
}

// auditFeed is one box's audit timeline, read once for every audit
// subscription following it (Server.audits), each narrowed by its own search:
// a new query is a new subscription, which narrows what was read rather than
// reading the trails again — and drawing nothing while it does. It is read
// from the first subscription until the last one leaves (leaveAudit); one
// opened after that, or after its read failed whole or its box was found
// gone, reads the trails afresh.
type auditFeed struct {
	mu sync.Mutex
	// records are its most recent, oldest first; missing what the last read
	// could not reach; polled whether any read has finished.
	records []console.AuditRecord
	missing []string
	polled  bool
	// reading is whether its box was found and its trails are being read:
	// before, nothing is sent but why the box could not be found. err is why,
	// the last time it was looked for, or the whole read failing, after
	// which done is set and nothing more is sent (console.AuditUnreadable).
	reading bool
	err     error
	done    bool
	// followers is each subscription following it, with its search.
	followers map[*follower]console.Query
	stop      context.CancelFunc
}

// auditFollower is an audit subscription on the feed it follows, by its
// Sub in Server.auditSubs: what leaveAudit takes off it.
type auditFollower struct {
	f    *follower
	feed *auditFeed
}

// followAudit follows a box's audit timeline for f, narrowed to what its
// search lets through: on the box's feed, started for it if none is being
// read, until it leaves (leaveAudit). f joins it here, in the order the app's
// messages came, so a subscription for a new query made before the old is
// ended finds the feed still read.
func (s *Server) followAudit(serving context.Context, f *follower) {
	var q console.Query
	if f.sub.Audit != nil {
		q = console.ParseQuery(f.sub.Audit.Text)
	}
	key := f.sub.Box
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	feed := s.audits[key]
	if feed == nil {
		// The first follower is on it before it reads anything, so it is
		// sent every step, from the first.
		ctx, stop := context.WithCancel(serving)
		feed = &auditFeed{missing: []string{}, followers: map[*follower]console.Query{f: q}, stop: stop}
		s.audits[key] = feed
		go feed.read(ctx, s, key)
	} else {
		feed.mu.Lock()
		feed.followers[f] = q
		feed.mu.Unlock()
		// What it has is sent off the pipe's reader: a snapshot is written
		// to the app, which the reader never waits on.
		go func() {
			feed.mu.Lock()
			defer feed.mu.Unlock()
			if _, on := feed.followers[f]; on {
				feed.joined(f, q)
			}
		}()
	}
	s.auditSubs[f.sub.Sub] = auditFollower{f: f, feed: feed}
}

// leaveAudit takes an audit subscription off its feed as it ends, and ends
// the feed once nothing follows it. It is called once the subscription's
// context is canceled and Server.subMu is let go: a feed sends under its
// own lock, and a send takes subMu. Anything else is not on a feed, and
// leaving is nothing.
func (s *Server) leaveAudit(sub uint32) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	on, ok := s.auditSubs[sub]
	if !ok {
		return
	}
	delete(s.auditSubs, sub)
	feed := on.feed
	feed.mu.Lock()
	delete(feed.followers, on.f)
	last := len(feed.followers) == 0
	feed.mu.Unlock()
	if last {
		feed.stop()
		s.dropAudit(on.f.sub.Box, feed)
	}
}

// dropAudit lets go of a box's feed, under auditMu, so the next subscription
// on the box reads its trails afresh: the feed ended, failed whole, or found
// its box gone. Its followers stay on it until they leave.
func (s *Server) dropAudit(key string, feed *auditFeed) {
	if s.audits[key] == feed {
		delete(s.audits, key)
	}
}

// read reads the box's trails until ctx ends or the read fails whole: the
// box resolved once, as `about` resolves one for a topic (findBox), and
// followed on its server. Every change is sent to every follower, under mu.
func (feed *auditFeed) read(ctx context.Context, s *Server, key string) {
	box, src, ok := s.findBox(ctx, key, func(err error) {
		feed.failed(s, key, err, errors.Is(err, errNotListed))
	})
	if !ok {
		return
	}
	feed.mu.Lock()
	feed.reading, feed.err = true, nil
	feed.sent()
	feed.mu.Unlock()
	// A record's time says its day once it is another day's: the timeline
	// is sent again as the day turns, however quiet the trails are — for as
	// long as they are followed.
	following, stop := context.WithCancel(ctx)
	defer stop()
	turns := dayTurns
	go func() {
		for {
			turn := time.NewTimer(turns(time.Now()))
			select {
			case <-following.Done():
				turn.Stop()
				return
			case <-turn.C:
				feed.mu.Lock()
				if !feed.done {
					feed.sent()
				}
				feed.mu.Unlock()
			}
		}
	}()
	err := src.FollowAudit(ctx, box.ID, func(u console.AuditUpdate) {
		feed.mu.Lock()
		defer feed.mu.Unlock()
		feed.records = append(feed.records, u.Records...)
		if extra := len(feed.records) - auditKeep; extra > 0 {
			feed.records = append([]console.AuditRecord(nil), feed.records[extra:]...)
		}
		if u.Polled {
			feed.missing, feed.polled = nonNil(u.Missing), true
		}
		feed.sent()
	})
	stop()
	if err != nil && ctx.Err() == nil {
		feed.failed(s, key, errors.New(console.AuditUnreadable(err)), true)
	}
}

// failed says why to every follower: the box could not be found this time,
// or, done, the feed will read nothing more — its read failed whole, or its
// box is gone. A feed that is done is let go of first, so a subscription on
// the box from then on, a new search or the view opened again, tries afresh
// rather than joining it.
func (feed *auditFeed) failed(s *Server, key string, err error, done bool) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	if done {
		s.dropAudit(key, feed)
	}
	feed.mu.Lock()
	defer feed.mu.Unlock()
	feed.err, feed.done = err, done
	feed.sent()
}

// sent sends what changed to every follower, under mu: why the box could
// not be found or the read failed, or else the timeline under each one's
// search.
func (feed *auditFeed) sent() {
	now := time.Now()
	for f, q := range feed.followers {
		if feed.err != nil {
			f.failed(feed.err)
		} else if feed.reading {
			f.send(Snapshot{Audit: wireAudit(feed.records, feed.missing, feed.polled, q, now)})
		}
	}
}

// joined sends a follower that just joined what the feed has, under mu: the
// timeline under its search once it is being read, then why it could not be
// found or failed, if it could not.
func (feed *auditFeed) joined(f *follower, q console.Query) {
	if feed.reading {
		f.send(Snapshot{Audit: wireAudit(feed.records, feed.missing, feed.polled, q, time.Now())})
	}
	if feed.err != nil {
		f.failed(feed.err)
	}
}

// dayTurns is how long a followed timeline waits before it is sent again
// for a new day: untilTomorrow, which a test shortens.
var dayTurns = untilTomorrow

// untilTomorrow is how long from now until the next local midnight, and a
// moment past it, so the times read then are the new day's.
func untilTomorrow(now time.Time) time.Duration {
	y, m, d := now.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, now.Location()).Sub(now) + time.Second
}

// wireAudit is the timeline as the audit topic sends it, under a search:
// the records it lets through with their times as of now and what it lights
// in each, the rows they fold into, what it says with none, and what the
// search let through of how many.
func wireAudit(records []console.AuditRecord, missing []string, polled bool, q console.Query, now time.Time) *Audit {
	shown := make([]console.AuditRecord, 0, len(records))
	for _, i := range console.AuditSearch(q, records) {
		shown = append(shown, records[i])
	}
	a := &Audit{Records: make([]AuditRecord, 0, len(shown)), Runs: []AuditRun{}, Missing: missing}
	for _, r := range shown {
		source, summary := console.AuditLit(q, r.Source, r.Summary)
		a.Records = append(a.Records, AuditRecord{
			ID: r.ID, Time: r.Time, Clock: console.AuditTime(r.Time, now), Source: r.Source, Summary: r.Summary,
			Group: r.Group, GroupSummary: r.GroupSummary, SourceLit: source, SummaryLit: summary,
		})
	}
	for _, run := range console.AuditRuns(shown) {
		w := AuditRun{First: run.First, Last: run.Last}
		if run.Folds() {
			newest := shown[run.Last]
			w.Summary = console.AuditRunSummary(newest, run.Count())
			w.SourceLit, w.SummaryLit = console.AuditLit(q, newest.Source, w.Summary)
		}
		a.Runs = append(a.Runs, w)
	}
	if !q.Empty() {
		a.Tally = console.AuditTally(len(shown), len(records))
	}
	a.Below.One, a.Below.Many = console.AuditBelowWords(!q.Empty())
	switch {
	case len(shown) > 0:
	case len(records) > 0:
		a.Note = console.AuditNoMatch
	case polled:
		a.Note = console.AuditNothing
	default:
		a.Note = console.AuditReading
	}
	return a
}

// header follows a box's workspace header: the core's (console.NewHeader)
// for the box as the last listing has it, with its ports at the local ports
// of a forward that lasts as long as the subscription — the workspace's,
// which goes with the screen showing it (console.Workspace.Park). It is read
// again whenever the forward binds or loses a port, and whenever the listing
// is read (relisted), since the listing carries the git state and the ports
// served, and whenever what the box's attached primary says of its program
// changes what its agent is said to be doing (program.go). A forward
// that could not be opened leaves the ports listed and unlinked, and says
// why.
func (f *follower) header(ctx context.Context) {
	box, src := f.box, f.src
	var bindings func() []console.Binding
	var events <-chan struct{}
	unforwarded := ""
	if fw, err := src.Forward(ctx, box.ID); err != nil {
		unforwarded = err.Error()
	} else {
		defer func() { _ = fw.Close() }()
		bindings, events = fw.Bindings, fw.Events()
		// The pane's links are pointed through it while it is held (follow).
		defer f.s.holdForward(f.sub.Box, fw.Bindings)()
	}
	for {
		fresh := f.s.relisted()
		if b, ok := f.s.listedBox(f.sub.Box); ok {
			box = b
		}
		var forwarded map[console.PortKey]int
		if bindings != nil {
			forwarded = console.Forwarded(bindings())
		}
		header, heard := f.s.header(f.sub.Box, box, forwarded)
		h := wireHeader(header)
		h.Unforwarded = unforwarded
		f.send(Snapshot{Header: &h})
		select {
		case <-ctx.Done():
			return
		case <-fresh:
		case <-heard:
		case _, ok := <-events:
			if !ok {
				// The forward has closed: what it bound is gone.
				events, bindings = nil, nil
			}
		}
	}
}

// relisted reads from the last listing now, and again each time another is
// read, until ctx ends: a topic that is about what the app asks, not about
// the servers, which it never reads itself.
func (f *follower) relisted(ctx context.Context, read func() Snapshot) {
	for {
		fresh := f.s.relisted()
		f.send(read())
		select {
		case <-ctx.Done():
			return
		case <-fresh:
		}
	}
}

// asked is the core's Context for where the subscription asks from, from
// what the bridge last read (cachedContext).
func (f *follower) asked() console.Context {
	var x Context
	if f.sub.Context != nil {
		x = *f.sub.Context
	}
	return f.s.cachedContext(x)
}

// commands is every registry command as it stands for the subscription's
// context and boxes. A box the last listing does not have is left out, so a
// box command asked about it reads as one with nothing to act on.
func (f *follower) commands() Snapshot {
	cctx := f.asked()
	targets := f.s.cached(f.sub.Targets)
	f.s.regMu.Lock()
	entries := f.s.reg.Entries(cctx, targets)
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, wireEntry(e, f.s.reg.Usage(e.ID)))
	}
	f.s.regMu.Unlock()
	return Snapshot{Commands: &Commands{Entries: out}}
}

// palette is what the query finds (console.Registry.Palette) among the last
// listing's boxes and the registry's commands. Text that commits a chip as it
// is typed is answered as the committed query, and says so.
func (f *follower) palette() Snapshot {
	var asked PaletteQuery
	if f.sub.Palette != nil {
		asked = *f.sub.Palette
	}
	session, last := f.s.lastListing()
	l := console.NewList(session, console.ListStaleAfter)
	l.SetAll(last.Sandboxes)
	l.SetUnreachable(last.Unreachable)
	l.SetWaiting(last.Waiting)
	l.SetListed(last.Listed)
	ref := func(key string) (console.BoxRef, bool) {
		b := f.s.cached([]string{key})
		if len(b) == 0 {
			return console.BoxRef{}, false
		}
		return console.BoxRef{Server: session.ServerName(b[0].Server), ID: b[0].ID}, true
	}
	q := console.PaletteQuery{Text: asked.Text, Command: asked.Command, Archived: asked.Archived, Recent: map[console.BoxRef]int{}}
	var commit *PaletteCommit
	if q.Command == "" && asked.Box == "" {
		if id, rest, ok := console.PaletteCommit(q.Text); ok {
			q.Command, q.Text = id, rest
			commit = &PaletteCommit{Command: id, Text: rest}
		}
	}
	if asked.Box != "" {
		// A box chip the listing has lost is still asked about, by a
		// reference nothing finds, so every command says it is gone.
		r, ok := ref(asked.Box)
		if !ok {
			r = console.BoxRef{Server: keyServer(asked.Box), ID: asked.Box}
		}
		q.Box = &r
	}
	q.Current, _ = ref(asked.Current)
	for i, key := range asked.Recent {
		if r, ok := ref(key); ok {
			q.Recent[r] = len(asked.Recent) - i
		}
	}
	cctx := f.asked()
	f.s.regMu.Lock()
	res := f.s.reg.Palette(&l, cctx, q)
	out := Palette{Rows: make([]PaletteRow, 0, len(res.Rows)), Archived: res.Archived, Commit: commit, Partial: nonNil(l.Partial()), Stale: []Note{}}
	for _, n := range l.Stale() {
		out.Stale = append(out.Stale, Note{Server: n.Server, Title: n.Title, Detail: n.Detail})
	}
	for _, r := range res.Rows {
		row := PaletteRow{Why: r.Why}
		if r.Box != nil {
			row.Box = Key(r.Box.Server, r.Box.ID)
		} else {
			e := wireEntry(*r.Command, f.s.reg.Usage(r.Command.ID))
			row.Command = &e
		}
		out.Rows = append(out.Rows, row)
	}
	f.s.regMu.Unlock()
	return Snapshot{Palette: &out}
}

// issue follows a box's issue or pull request, by the number its tag gives:
// read as it is subscribed (in full when the app asked to see it now), then
// every console.IssueRefreshEvery, soon again while GitHub has yet to say
// whether a pull request can merge (console.PullSettleAfter, at most
// console.PullSettleTries times), and in full at once when a command here
// changed it (reissue). A head newer than the one the person was shown is
// said (console.ShownHead, Issue.Pushed): the head the app had shown before
// this subscription (IssueQuery.Shown), else the first read's, moved only by
// a read the app asked for (IssueQuery.Full).
func (f *follower) issue(ctx context.Context) {
	box, src := f.box, f.src
	var q IssueQuery
	if f.sub.Issue != nil {
		q = *f.sub.Issue
	}
	number, err := box.Work(q.Pull)
	if err != nil {
		f.failed(err)
		return
	}
	full, asked, settles := q.Full, q.Full, 0
	shown := console.NewShownHead(q.Shown)
	for {
		changed := f.s.reissued(box.Repository, number)
		read, cancel := context.WithTimeout(ctx, console.IssueTimeout)
		issue, err := src.Issue(read, box.Repository, number, full)
		cancel()
		every := console.IssueRefreshEvery
		switch {
		case err != nil && ctx.Err() != nil:
			return
		case err != nil:
			f.failed(err)
		default:
			out := wireIssue(issue, box.Repository, time.Now())
			// Only the subscription's first read is one the app asked for; a
			// command here changing it shows the person nothing new.
			if pushed := shown.Read(issue, asked); pushed != "" {
				out.Pushed, out.Notice = pushed, console.PushedNotice(pushed, issue.Pull.HeadSHA)
			}
			asked = false
			f.send(Snapshot{Issue: &out})
			if issue.Settling() && settles < console.PullSettleTries {
				settles++
				every = console.PullSettleAfter
			}
		}
		full = false
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		case <-changed:
			full = true
		}
	}
}
