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
	s.mu.Lock()
	if old := s.subs[sub.Sub]; old != nil {
		old()
	}
	s.subs[sub.Sub] = cancel
	s.mu.Unlock()
	f := &follower{s: s, sub: sub}
	switch sub.Topic {
	case TopicListing:
		go f.poll(ctx, console.ListPollEvery, true, f.listing)
	case TopicResources:
		go f.poll(ctx, console.ListPollEvery, false, f.resources)
	case TopicRequests:
		go f.poll(ctx, console.ListPollEvery, false, f.requests)
	case TopicSessions:
		go f.about(ctx, func() { f.poll(ctx, console.WorkspacePollEvery, true, f.sessions) })
	case TopicProvisioning:
		go f.about(ctx, func() { f.provisioning(ctx) })
	case TopicAudit:
		go f.about(ctx, func() { f.audit(ctx) })
	case TopicForwards:
		go f.about(ctx, func() { f.forwards(ctx) })
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

func (s *Server) unsubscribe(sub uint32) {
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
	s    *Server
	sub  Subscribe
	last []byte
	// box and src are the box a topic about one box follows, and the source
	// for its server.
	box console.Sandbox
	src console.DataSource
}

// send sends a snapshot unless it is the one sent last.
func (f *follower) send(snap Snapshot) {
	snap.Sub, snap.Topic = f.sub.Sub, f.sub.Topic
	data, err := json.Marshal(snap)
	if err != nil || bytes.Equal(data, f.last) {
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

func (f *follower) listing(ctx context.Context) (Snapshot, error) {
	l, err := f.s.list(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	out := Listing{Boxes: make([]Box, 0, len(l.Sandboxes)), Unreachable: nonNil(l.Unreachable), Waiting: nonNil(l.Waiting)}
	for _, b := range l.Sandboxes {
		out.Boxes = append(out.Boxes, wireBox(b))
	}
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

// sessions is what the box's workspace opens, by the core's rules: the
// primary, then every session the exec listing opens as a tab, in tab order;
// and its services. A box that is not running opens its primary alone.
func (f *follower) sessions(ctx context.Context) (Snapshot, error) {
	box, src := f.box, f.src
	// The primary opens whatever the listing says (console.Workspace's first
	// answer): attaching it is what waits for, and revives, a box not up yet.
	primary := console.Exec{ID: console.ExecPrimary, Primary: true, Live: true}
	execs, err := src.Execs(ctx, box.ID)
	if errors.Is(err, console.ErrNotRunning) {
		return Snapshot{Sessions: &Sessions{Sessions: []Tab{wireTab(console.SessionTab(primary, ""), primary)}, Services: []Service{}}}, nil
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
	out := Sessions{Running: true, Sessions: []Tab{wireTab(console.SessionTab(primary, ""), primary)}, Services: make([]Service, 0, len(services))}
	for _, e := range console.Sessions(execs) {
		out.Sessions = append(out.Sessions, wireTab(console.SessionTab(e, ""), e))
	}
	for _, svc := range services {
		out.Services = append(out.Services, wireService(svc))
	}
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

// forwards forwards a box's ports to this machine for as long as the
// subscription lasts, and says what is bound whenever that changes.
func (f *follower) forwards(ctx context.Context) {
	box, src := f.box, f.src
	fw, err := src.Forward(ctx, box.ID)
	if err != nil {
		f.failed(err)
		return
	}
	defer func() { _ = fw.Close() }()
	for {
		bindings := fw.Bindings()
		out := Forwards{Ports: make([]Forward, 0, len(bindings))}
		for _, b := range bindings {
			out.Ports = append(out.Ports, Forward{Port: b.Port, UDP: b.UDP, Local: b.Local})
		}
		f.send(Snapshot{Forwards: &out})
		select {
		case <-ctx.Done():
			return
		case _, ok := <-fw.Events():
			if !ok {
				return
			}
		}
	}
}
