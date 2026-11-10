package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// auditKeep is how much of an audit timeline a snapshot carries, the most
// recent records: the whole of one is sent on every change.
const auditKeep = 1000

// subscribe starts following a topic. Each subscription reads on its own
// until it is ended, and sends a snapshot whenever what it read changed.
func (s *Server) subscribe(ctx context.Context, sub Subscribe) {
	ctx, cancel := context.WithCancel(ctx)
	s.subMu.Lock()
	s.mu.Lock()
	if old := s.subs[sub.Sub]; old != nil {
		old()
	}
	s.subs[sub.Sub] = cancel
	s.mu.Unlock()
	s.subMu.Unlock()
	f := &follower{s: s, sub: sub, ctx: ctx}
	switch sub.Topic {
	case TopicListing:
		if b := s.cached([]string{sub.Reveal}); sub.Reveal != "" && len(b) == 1 {
			f.filter = console.Revealing(b[0])
		} else {
			f.filter = s.filter(ctx, sub.Filter)
		}
		go f.poll(ctx, console.ListPollEvery, true, f.listing)
	case TopicResources:
		go f.poll(ctx, console.ListPollEvery, false, f.resources)
	case TopicRequests:
		go f.poll(ctx, console.ListPollEvery, false, f.requests)
	case TopicSessions:
		done := s.followTabs(sub.Box)
		go func() {
			defer done()
			f.about(ctx, func() { f.poll(ctx, console.WorkspacePollEvery, true, f.sessions) })
		}()
	case TopicProvisioning:
		go f.about(ctx, func() { f.provisioning(ctx) })
	case TopicAudit:
		go f.about(ctx, func() { f.audit(ctx) })
	case TopicHeader:
		go f.about(ctx, func() { f.header(ctx) })
	case TopicCommands:
		go f.relisted(ctx, f.commands)
	case TopicPalette:
		go f.relisted(ctx, f.palette)
	case TopicComposer:
		go f.composer(ctx)
	default:
		f.send(Snapshot{Error: fmt.Sprintf("there is no topic %q", sub.Topic)})
	}
}

// about follows a topic about one box: the box is resolved once, as the
// subscription starts, and followed on its server from then on. A box no
// server lists is said once, rather than the listing read again on every
// beat for a box that is gone; one whose server could not be reached is
// tried again every beat, and followed once it can be.
func (f *follower) about(ctx context.Context, follow func()) {
	for {
		box, src, err := f.s.source(ctx, f.sub.Box)
		if err == nil {
			f.box, f.src = box, src
			follow()
			return
		}
		if ctx.Err() != nil {
			return
		}
		f.failed(err)
		if errors.Is(err, errNotListed) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(console.ListPollEvery):
		}
	}
}

// unsubscribe ends a subscription. Once it returns, nothing more of it is
// sent: a snapshot being written is finished first (Server.subMu).
func (s *Server) unsubscribe(sub uint32) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	s.mu.Lock()
	cancel := s.subs[sub]
	delete(s.subs, sub)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
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

// poll reads every so often until ctx ends, and at once when the listing is
// kicked, for a topic that follows the boxes.
func (f *follower) poll(ctx context.Context, every time.Duration, kicked bool, read func(context.Context) (Snapshot, error)) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		kick := f.s.kicked()
		if snap, err := read(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			f.failed(err)
		} else {
			f.send(snap)
		}
		if !kicked {
			kick = nil
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
	f.s.noteInbox(requests, rejections)
	// Which box each is about is the core's rule, against the boxes listed.
	session, boxes := f.s.listed()
	out := Requests{Requests: make([]Request, 0, len(requests)), Rejections: make([]Rejection, 0, len(rejections))}
	for _, r := range requests {
		key := Key(r.Server, r.SandboxID)
		for _, b := range boxes {
			if len(console.RequestsFor(session, b, []console.CredentialRequest{r})) > 0 {
				key = Key(b.Server, b.ID)
			}
		}
		out.Requests = append(out.Requests, wireRequest(r, key))
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

// audit follows a box's audit timeline: its most recent records and the
// trails the last read could not reach, whole on every update.
func (f *follower) audit(ctx context.Context) {
	box, src := f.box, f.src
	timeline := Audit{Records: []AuditRecord{}, Missing: []string{}}
	f.send(Snapshot{Audit: &timeline})
	err := src.FollowAudit(ctx, box.ID, func(u console.AuditUpdate) {
		for _, r := range u.Records {
			timeline.Records = append(timeline.Records, AuditRecord{
				ID: r.ID, Time: r.Time, Source: r.Source, Summary: r.Summary, Group: r.Group, GroupSummary: r.GroupSummary,
			})
		}
		if extra := len(timeline.Records) - auditKeep; extra > 0 {
			timeline.Records = append([]AuditRecord{}, timeline.Records[extra:]...)
		}
		if u.Polled {
			timeline.Missing = nonNil(u.Missing)
		}
		f.send(Snapshot{Audit: &timeline})
	})
	if err != nil && ctx.Err() == nil {
		f.failed(err)
	}
}

// header follows a box's workspace header: the core's (console.NewHeader)
// for the box as the last listing has it, with its ports at the local ports
// of a forward that lasts as long as the subscription — the workspace's,
// which goes with the screen showing it (console.Workspace.Park). It is read
// again whenever the forward binds or loses a port, and whenever the listing
// is read (relisted), since the listing carries the git state and the ports
// served. A forward
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
		h := wireHeader(console.NewHeader(box, forwarded, nil, false))
		h.Unforwarded = unforwarded
		f.send(Snapshot{Header: &h})
		select {
		case <-ctx.Done():
			return
		case <-fresh:
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
