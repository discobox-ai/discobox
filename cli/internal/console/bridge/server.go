package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/clipimage"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// Server is the CLI's side of the bridge (ADR 26-11-07-289): it serves the
// console core over a pipe to the desktop app. Commands run through the
// core's registry and dispatch (console.Dispatch), with the core's rules;
// what a command asks or reports while it runs goes to the app as questions,
// forms and progress; and the app follows what it draws by subscribing.
type Server struct {
	ds      console.DataSource
	w       *Writer
	streams *streams

	// regMu holds the registry, which is the window's palette ranking, still
	// while a command is ruled on: commands run concurrently.
	regMu sync.Mutex
	reg   *console.Registry

	nextID atomic.Uint64

	mu      sync.Mutex
	session console.Session
	// saved is the list's filter as this machine's windows last left it
	// (console.ListView): what a listing subscribed without a filter opens
	// on, and what a filter the app chooses is saved over.
	saved console.ListView
	// boxes is every box of the last listing, by the key the app knows it by,
	// and unanswered the servers that listing had no answer from, whose boxes
	// are missing from it rather than gone.
	boxes      map[string]console.Sandbox
	unanswered map[string]bool
	// last is the last listing whole — its boxes in its order, and what it
	// says about the servers — and fresh is closed when a listing replaces
	// it: what the commands and the palette are read from and sent again on.
	last  console.Listing
	fresh chan struct{}
	// heard is what each box's attached primary has said of its program,
	// by its key (program.go).
	heardMu sync.Mutex
	heard   map[string]*heard
	// kick is closed to read the listing again now (relist).
	kick chan struct{}
	// issues is closed, by the issue's page, to read an issue again now
	// (reissue): a command here changed it.
	issues map[string]chan struct{}
	// files counts the files changed sessions being started (beginFiles) or
	// still being ended in their boxes (endFiles), which the bridge waits for
	// on its way out; closing is set once it has begun waiting, after which
	// none is started.
	filesMu sync.Mutex
	closing bool
	files   sync.WaitGroup
	// subMu is held to write a snapshot (read) and to end a subscription
	// (write): an unsubscribe the app sent before a command is never
	// followed by a snapshot of that subscription, written by a follower
	// that was already past its check of whether it had ended.
	subMu sync.RWMutex
	subs  map[uint32]context.CancelFunc
	forms map[uint64]*mailbox
	// inboxed is what waited on a person when it was last read, by the
	// requests topic or a command, and when.
	inboxed   inboxRead
	questions map[uint64]chan Answer
	// edits is each edit the app was asked to open, by its ID, waiting for its
	// saved.
	edits map[uint64]chan Saved
	// tabs is each box's sessions as the core made them tabs, by box key and
	// exec ID, as listed: what a stream opened on one of them
	// ends as (console.Tab.End). following counts each box's sessions
	// subscriptions; a box's tabs go when the last of them ends. services is
	// each box's services as listed with them, by box key and service ID:
	// what a service's tab is opened on (console.OpenService).
	tabs      map[string]map[string]console.Tab
	services  map[string]map[string]console.Service
	following map[string]int
	// forwards is the bindings of the forward each box's header holds, by
	// box key, the latest last: what a link in its panes is pointed through
	// (follow).
	forwards map[string][]*func() []console.Binding

	// shown is the filter of the app's list, as it last subscribed with one
	// of its own: the folder and server the run options follow.
	shown console.Filter

	// The window's inbox (requests.go), under mu: the notices it was told
	// to take down; the refresh commands a person let run again without
	// asking while it is open, and the requests a renewal is out for
	// (console.Renewals); what
	// those renewals said, until a requests snapshot carries it; and
	// rekick, closed to read the requests again now (reread).
	dismissed console.Dismissals
	renewals  console.Renewals
	said      []Said
	rekick    chan struct{}
	// optsMu guards options, the window's run options.
	optsMu  sync.Mutex
	options runOptions
}

// NewServer is a bridge serving ds, writing to the app on out.
func NewServer(ds console.DataSource, out io.Writer) *Server {
	s := &Server{
		ds:        ds,
		w:         NewWriter(out),
		reg:       console.NewRegistry(ds),
		boxes:     map[string]console.Sandbox{},
		heard:     map[string]*heard{},
		kick:      make(chan struct{}),
		issues:    map[string]chan struct{}{},
		rekick:    make(chan struct{}),
		fresh:     make(chan struct{}),
		subs:      map[uint32]context.CancelFunc{},
		forms:     map[uint64]*mailbox{},
		questions: map[uint64]chan Answer{},
		edits:     map[uint64]chan Saved{},
		tabs:      map[string]map[string]console.Tab{},
		services:  map[string]map[string]console.Service{},
		following: map[string]int{},
		forwards:  map[string][]*func() []console.Binding{},
		options:   openOptions(console.Session{}, console.Filter{}),
	}
	s.streams = newStreams(s.w, s.send)
	return s
}

// Serve reads the app's frames until in ends or ctx does, then cancels every
// command and subscription and closes every stream. Nothing but hello is
// answered before hello, nor anything after a hello of another protocol.
func (s *Server) Serve(ctx context.Context, in io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		s.streams.closeAll()
		// A files changed session is the window's own: nothing else will
		// ever list it to end it.
		s.filesMu.Lock()
		s.closing = true
		s.filesMu.Unlock()
		s.files.Wait()
	}()
	go func() {
		<-ctx.Done()
		if c, ok := in.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	welcomed := false
	for {
		f, err := ReadFrame(in)
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if f.Kind != KindJSON {
			if welcomed {
				s.streams.frame(f)
			}
			continue
		}
		var m Message
		if err := json.Unmarshal(f.Payload, &m); err != nil {
			continue
		}
		if !welcomed && m.Type == "" {
			// Protocol 1 had no message types: its app's hello is
			// {"id":0,"op":"hello"}, and it reads an error in its own shape.
			s.send1(`this CLI speaks desktop bridge protocol 2, and the desktop app protocol 1: the app is older; run the app built with this CLI`)
			continue
		}
		if m.Type == TypeHello && m.Hello != nil {
			welcomed = s.hello(ctx, *m.Hello)
			continue
		}
		if welcomed {
			s.handle(ctx, m)
		}
	}
}

// send1 answers an app of protocol 1, which had no envelope, with an error
// on its hello, so it says why and exits rather than waiting.
func (s *Server) send1(why string) {
	data, err := json.Marshal(map[string]any{"id": 0, "error": why})
	if err == nil {
		_ = s.w.Write(KindJSON, 0, data)
	}
}

func (s *Server) send(m Message) {
	data, err := json.Marshal(m)
	if err == nil {
		_ = s.w.Write(KindJSON, 0, data)
	}
}

// hello answers with this side's protocol, and with the spec and the session
// only when the app's is the same: it is the app that says which side is
// older, and exits.
func (s *Server) hello(ctx context.Context, h Hello) bool {
	if h.Protocol != Protocol {
		s.send(Message{Type: TypeWelcome, Welcome: &Welcome{Protocol: Protocol}})
		return false
	}
	session, _ := s.ds.Session(ctx)
	saved, _ := session.Restore(session.View)
	s.mu.Lock()
	s.session, s.saved = session, saved
	s.mu.Unlock()
	_, shown := session.Restore(saved)
	s.mu.Lock()
	s.shown = shown
	s.mu.Unlock()
	s.optsMu.Lock()
	s.options = openOptions(session, shown)
	s.optsMu.Unlock()
	spec := console.NewSpec()
	s.send(Message{Type: TypeWelcome, Welcome: &Welcome{Protocol: Protocol, Spec: &spec, Session: &Session{
		PrimaryServer: s.ds.PrimaryServer(), Project: session.Project,
		Directory: session.Directory, Branch: session.Branch, Host: session.Host, Draft: session.Draft,
	}}})
	return true
}

func (s *Server) handle(ctx context.Context, m Message) {
	switch {
	case m.Type == TypeSubscribe && m.Subscribe != nil:
		s.subscribe(ctx, *m.Subscribe)
	case m.Type == TypeUnsubscribe && m.Unsubscribe != nil:
		s.unsubscribe(m.Unsubscribe.Sub)
	case m.Type == TypeCommand && m.Command != nil:
		c := *m.Command
		go func() {
			r := s.run(ctx, c)
			r.ID = c.ID
			s.send(Message{Type: TypeResult, Result: &r})
		}()
	case m.Type == TypeFollow && m.Follow != nil:
		f := *m.Follow
		go func() {
			r := s.follow(ctx, f)
			r.ID = f.ID
			s.send(Message{Type: TypeResult, Result: &r})
		}()
	case m.Type == TypeDraft && m.Draft != nil:
		// In the order the app sent them, here on the reader: two drafts
		// saved concurrently could land older last. It is one small local
		// write, never a server's answer.
		s.saveDraft(ctx, *m.Draft)
	case m.Type == TypeSubmit && m.Submit != nil:
		s.mu.Lock()
		box := s.forms[m.Submit.Form]
		s.mu.Unlock()
		if box != nil {
			box.put(*m.Submit)
		}
	case m.Type == TypeAnswer && m.Answer != nil:
		s.mu.Lock()
		ch := s.questions[m.Answer.Question]
		delete(s.questions, m.Answer.Question)
		s.mu.Unlock()
		if ch != nil {
			ch <- *m.Answer
		}
	case m.Type == TypeSaved && m.Saved != nil:
		s.mu.Lock()
		ch := s.edits[m.Saved.Edit]
		delete(s.edits, m.Saved.Edit)
		s.mu.Unlock()
		if ch != nil {
			ch <- *m.Saved
		}
	case m.Type == TypeUpload && m.Upload != nil:
		u := *m.Upload
		go func() {
			r := s.upload(ctx, u)
			r.ID = u.ID
			s.send(Message{Type: TypeResult, Result: &r})
		}()
	case m.Type == TypeOpen && m.Open != nil:
		o := *m.Open
		// Made pending here, in the order the app sent it, so frames that
		// follow the open are kept for it rather than racing it.
		if s.streams.get(o.Stream) == nil {
			return
		}
		go s.open(ctx, o)
	case m.Type == TypeClose && m.Close != nil:
		s.streams.close(m.Close.Stream)
	}
}

// open attaches a stream to one of a box's sessions: a service's tab to what
// the core opens for its service as last listed (console.OpenService), any
// other to its session.
func (s *Server) open(ctx context.Context, o Open) {
	tab := s.tab(o.Box, o.Exec)
	box, src, err := s.source(ctx, o.Box)
	if err != nil {
		s.streams.fail(o.Stream, err, tab)
		return
	}
	cols, rows := size(o.Cols, o.Rows)
	var term console.Terminal
	if id, ok := console.ServiceOfTab(o.Exec); ok {
		// A service's tab is opened on the service as last listed; one the
		// bridge no longer has (its listing dropped it, or nothing follows the
		// box's sessions now) has nothing to open, and its key is no session.
		svc, listed := s.service(o.Box, id)
		if !listed {
			s.streams.fail(o.Stream, errors.New("the service is no longer listed"), console.ServiceTab(console.Service{ID: id}, 0))
			return
		}
		term, err = console.OpenService(ctx, src, box.ID, svc, cols, rows)
	} else {
		term, err = src.OpenExec(ctx, box.ID, o.Exec, cols, rows)
	}
	if err != nil {
		s.streams.fail(o.Stream, err, tab)
		return
	}
	var watch *programWatch
	if primaryOf(o.Exec) {
		watch = s.watchProgram(o.Box, o.Stream)
	}
	s.streams.attach(o.Stream, term, tab, watch, nil)
}

// tab is the tab a box's session is, as the core made it when its sessions
// were listed; the primary by its ID. One never listed is a session as its ID
// alone says (console.SessionTab), which ends as a shell does. A session made
// here (frontend.session) is attached with the tab it was made as, and needs
// no lookup.
func (s *Server) tab(box, exec string) console.Tab {
	s.mu.Lock()
	t, ok := s.tabs[box][exec]
	s.mu.Unlock()
	if ok {
		return t
	}
	return console.SessionTab(console.Exec{ID: exec, Primary: exec == console.ExecPrimary}, "")
}

// service is one of a box's services, as last listed.
func (s *Server) service(box, id string) (console.Service, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.services[box][id]
	return svc, ok
}

// noteTabs keeps a box's sessions as tabs, as listed, and its services. It
// only adds sessions: a listing read while a session was being made does not
// hold it yet, and an open of it must not miss it. The services are the
// listing's, whole: a service's tab is opened on what it last said. What it
// keeps for a box goes when the box's sessions stop being followed
// (followTabs); a stream already open keeps its own.
func (s *Server) noteTabs(box string, tabs []console.Tab, services []console.Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tabs[box] == nil {
		s.tabs[box] = map[string]console.Tab{}
	}
	for _, t := range tabs {
		s.tabs[box][t.ExecID] = t
	}
	s.services[box] = make(map[string]console.Service, len(services))
	for _, svc := range services {
		s.services[box][svc.ID] = svc
	}
}

// followTabs counts a subscription following a box's sessions, and the
// function it returns ends it: what noteTabs kept for the box goes once
// nothing follows its sessions, since the app opens a box's sessions from
// what that topic told it, and a box followed again is listed again before
// the app can open anything from it.
func (s *Server) followTabs(box string) func() {
	s.mu.Lock()
	s.following[box]++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.following[box]--; s.following[box] <= 0 {
			delete(s.following, box)
			delete(s.tabs, box)
			delete(s.services, box)
		}
	}
}

// size is a pane's size, or a terminal's default for one the app has not
// laid out yet; the resize that follows corrects it.
func size(cols, rows int) (int, int) {
	if cols <= 0 || rows <= 0 {
		return 80, 24
	}
	return cols, rows
}

// upload carries an upload's images into its box, as the TUI carries a
// paste's (tui/imagepaste.go): the clipboard's image, or the files dropped when
// every one is an image (clipimage.Files), each written under the core's name
// for it. A drop that is not all images is done with no paths, for the app to
// paste the files' paths as text.
func (s *Server) upload(ctx context.Context, u Upload) Result {
	box, src, err := s.source(ctx, u.Box)
	if err != nil {
		return failed(err.Error())
	}
	images := []clipimage.Image{{Name: u.Name, Data: u.Data}}
	switch {
	case len(u.Files) > 0:
		if images, err = clipimage.Files(u.Files); err != nil {
			return failed("cannot read the dropped image: " + err.Error())
		}
		if len(images) == 0 {
			return Result{Status: StatusDone}
		}
	case len(u.Data) == 0:
		return failed("the upload carries no image")
	}
	ctx, cancel := context.WithTimeout(ctx, console.PutImageTimeout)
	defer cancel()
	now := time.Now()
	paths := make([]string, 0, len(images))
	for i, image := range images {
		path, err := src.PutImage(ctx, box.ID, console.PastedImageName(image.Name, now, i), image.Data)
		if err != nil {
			return failed(err.Error())
		}
		paths = append(paths, path)
	}
	return Result{Status: StatusDone, Paths: paths}
}

// ---------------------------------------------------------------------------
// Boxes

// Key is the key the app knows a box by: its ID when the listing names no
// server (one is configured), and the server's name and the ID otherwise. Two
// servers can list the same ID, so the ID alone names no box.
func Key(server, id string) string {
	if server == "" {
		return id
	}
	return server + "/" + id
}

// keyServer is the server a box key names: "" for a box of the one server.
func keyServer(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[:i]
	}
	return ""
}

// list reads the listing, noting each box in it by its key.
func (s *Server) list(ctx context.Context) (console.Listing, error) {
	listing, err := s.ds.List(ctx)
	if err != nil {
		return listing, err
	}
	boxes := make(map[string]console.Sandbox, len(listing.Sandboxes))
	for _, b := range listing.Sandboxes {
		boxes[Key(b.Server, b.ID)] = b
	}
	unanswered := map[string]bool{}
	for _, server := range append(slices.Clone(listing.Unreachable), listing.Waiting...) {
		unanswered[server] = true
	}
	s.mu.Lock()
	s.boxes, s.unanswered, s.last = boxes, unanswered, listing
	close(s.fresh)
	s.fresh = make(chan struct{})
	s.mu.Unlock()
	return listing, nil
}

// filter is what a listing subscription narrows the list to: the app's
// filter, saved as this machine's windows open on it, or with none the one
// they were left on.
func (s *Server) filter(ctx context.Context, asked *Filter) console.Filter {
	s.mu.Lock()
	session, saved := s.session, s.saved
	s.mu.Unlock()
	if asked == nil {
		_, f := session.Restore(saved)
		s.mu.Lock()
		s.shown = f
		s.mu.Unlock()
		return f
	}
	f := consoleFilter(*asked)
	s.mu.Lock()
	moved := s.shown.Folder.Source != f.Folder.Source || s.shown.Server != f.Server
	s.shown = f
	s.mu.Unlock()
	// The run options follow the list's folder and server: read again for
	// it, and the composer told, once, as the filter moves.
	if moved {
		go s.readOptions(ctx)
	}
	// Whether to is the core's rule (Session.Saves). A write that fails
	// leaves the next window opening on the view saved before, and there is
	// no reply to a subscribe to say it in: it is marked saved anyway, as the
	// TUI marks it, so it is not tried on every change.
	if view := f.View(); session.Saves(view, saved) {
		s.mu.Lock()
		s.saved = view
		s.mu.Unlock()
		_ = s.ds.SaveView(ctx, view)
	}
	return f
}

// listed is the session and the boxes of the last listing, in its order.
func (s *Server) listed() (console.Session, []console.Sandbox) {
	session, last := s.lastListing()
	return session, last.Sandboxes
}

// lastListing is the session and the last listing, whole: its boxes in its
// order, and what it says about the servers.
func (s *Server) lastListing() (console.Session, console.Listing) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.last
	l.Sandboxes = slices.Clone(l.Sandboxes)
	return s.session, l
}

// cached is the boxes of the last listing with these keys, in the order
// given, leaving out any it does not have: it never lists again.
func (s *Server) cached(keys []string) []console.Sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]console.Sandbox, 0, len(keys))
	for _, key := range keys {
		if b, ok := s.boxes[key]; ok {
			out = append(out, b)
		}
	}
	return out
}

// relisted is closed when the next listing is read.
func (s *Server) relisted() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fresh
}

// listedBox is a box by its key, as the last listing has it.
func (s *Server) listedBox(key string) (console.Sandbox, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.boxes[key]
	return b, ok
}

type inboxRead struct {
	at         time.Time
	requests   []console.CredentialRequest
	rejections []console.SecretRejection
}

// inbox is what waits on a person, as last read if that was within a
// listing's beat, and read again otherwise. A command's rules read it, and
// most commands are a key pressed in a run: reading the server for each one
// would be two requests a key. The requests topic keeps it current while the
// app follows it, so a request the app has been shown is one its command
// sees.
func (s *Server) inbox(ctx context.Context) ([]console.CredentialRequest, []console.SecretRejection) {
	s.mu.Lock()
	read := s.inboxed
	s.mu.Unlock()
	if !read.at.IsZero() && time.Since(read.at) < console.ListPollEvery {
		return read.requests, read.rejections
	}
	requests, _ := s.ds.CredentialRequests(ctx)
	rejections, _ := s.ds.SecretRejections(ctx)
	s.noteInbox(requests, rejections)
	return requests, rejections
}

func (s *Server) noteInbox(requests []console.CredentialRequest, rejections []console.SecretRejection) {
	s.mu.Lock()
	s.inboxed = inboxRead{at: time.Now(), requests: requests, rejections: rejections}
	s.mu.Unlock()
}

// relist has every listing subscription read again now: after a command
// that changed a box, and for refresh.
func (s *Server) relist() {
	s.mu.Lock()
	close(s.kick)
	s.kick = make(chan struct{})
	s.mu.Unlock()
}

// endFiles ends a files changed session in its box, once its stream is let
// go however that happens — its tab closed, discobox-review quit, the window
// gone: it is the tab's own, and nothing else ever lists it to end it
// (console.Exec.FilesChanged). The bridge waits for each on its way out.
func (s *Server) endFiles(src console.DataSource, box, exec string) func() {
	return func() {
		// Every stream is let go before the bridge waits (Serve), so this
		// is never refused.
		s.filesMu.Lock()
		s.files.Add(1)
		s.filesMu.Unlock()
		go func() {
			defer s.files.Done()
			ctx, cancel := context.WithTimeout(context.Background(), console.IssueTimeout)
			defer cancel()
			_ = src.EndExec(ctx, box, exec)
		}()
	}
}

// beginFiles counts a files changed session about to start, and is false
// once the bridge is on its way out and starts none.
func (s *Server) beginFiles() bool {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if s.closing {
		return false
	}
	s.files.Add(1)
	return true
}

// reissue reads an issue again now, for every subscription following it: a
// command here commented on it, closed, reopened or merged it.
func (s *Server) reissue(repository string, number int) {
	key := console.WorkAddress(repository, number, false)
	s.mu.Lock()
	if c := s.issues[key]; c != nil {
		close(c)
		delete(s.issues, key)
	}
	s.mu.Unlock()
}

// reissued is closed when an issue is to be read again (reissue).
func (s *Server) reissued(repository string, number int) <-chan struct{} {
	key := console.WorkAddress(repository, number, false)
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.issues[key]
	if c == nil {
		c = make(chan struct{})
		s.issues[key] = c
	}
	return c
}

func (s *Server) kicked() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kick
}

// errNotListed is a box no server lists, which no retry brings back.
var errNotListed = errors.New("no server lists it")

// box is a box by its key, from the last listing; one not in it is listed
// again first. One whose server did not answer is an error that may pass;
// one its server answered without is errNotListed. Neither is ever a request
// to the primary server about another server's box.
func (s *Server) box(ctx context.Context, key string) (console.Sandbox, error) {
	s.mu.Lock()
	b, ok := s.boxes[key]
	s.mu.Unlock()
	if ok {
		return b, nil
	}
	if _, err := s.list(ctx); err != nil {
		return console.Sandbox{}, err
	}
	s.mu.Lock()
	b, ok = s.boxes[key]
	server := keyServer(key)
	missing := s.unanswered[server]
	s.mu.Unlock()
	switch {
	case ok:
		return b, nil
	case missing:
		// Its server did not answer the listing: the box is missing from
		// it, not gone.
		return console.Sandbox{}, fmt.Errorf("the server %s is not answering", server)
	}
	return console.Sandbox{}, fmt.Errorf("no discobox %s is listed: %w", key, errNotListed)
}

// source is a box and the data source for the server it is on.
func (s *Server) source(ctx context.Context, key string) (console.Sandbox, console.DataSource, error) {
	b, err := s.box(ctx, key)
	if err != nil {
		return b, nil, err
	}
	src := s.ds.ForServer(b.Server)
	if src == nil {
		return b, nil, fmt.Errorf("the server %s is not answering", b.Server)
	}
	return b, src, nil
}

// ---------------------------------------------------------------------------
// Questions and forms

func (s *Server) id() uint64 { return s.nextID.Add(1) }

// ask puts a question to the app for a running command and waits for its
// answer: a key, or why there is none (context.Canceled for one dismissed).
func (s *Server) ask(ctx context.Context, command uint64, q console.Question) (string, error) {
	id := s.id()
	ch := make(chan Answer, 1)
	s.mu.Lock()
	s.questions[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.questions, id)
		s.mu.Unlock()
	}()
	question := Question{
		ID: id, Command: command, Title: q.Title, Subject: q.Subject, Body: q.Body, Choices: choices(q.Choices), DefaultNo: q.DefaultNo,
	}
	// A directory the answer turns on is counted while the question is up,
	// and the question sent again as the count climbs, until it is final or
	// the question is answered.
	var total func() console.DirectoryTotal
	var tick <-chan time.Time
	if q.Measure != "" {
		var stop func()
		total, stop = s.ds.MeasureDirectory(ctx, q.Measure)
		defer stop()
		counted := total()
		question.Count = counted.Said()
		if !counted.Done {
			ticker := time.NewTicker(console.MeasureEvery)
			defer ticker.Stop()
			tick = ticker.C
		}
	}
	s.send(Message{Type: TypeQuestion, Question: &question})
	for {
		select {
		case a := <-ch:
			if a.Declined {
				return "", context.Canceled
			}
			return a.Key, nil
		case <-tick:
			counted := total()
			if counted.Done {
				tick = nil
			}
			if said := counted.Said(); said != question.Count {
				question.Count = said
				again := question
				s.send(Message{Type: TypeQuestion, Question: &again})
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// edit has the app open a file in the person's editor for a running command
// and waits for its saved: nil once they are finished with it,
// context.Canceled for one they gave up, or why it could not be opened.
//
// The app is handed a copy, never the file itself, and what was saved there
// is written back only once the person is finished with it: the app's Cancel
// keeps the file as it was, which an editor saving the file itself could not
// (a tool's file is its copy on this machine, edited in place). The copy is
// under a text format's extension (console.EditExtension), since the app
// opens a file with the handler for its type and a tool file's name is its
// declaration's: one a box named config.sh would choose the program.
func (s *Server) edit(ctx context.Context, command uint64, e *console.Editing) error {
	ext := console.EditExtension(e.Path)
	content, err := os.ReadFile(e.Path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "discobox-edit-*"+ext)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := s.editAt(ctx, command, tmp.Name(), e.Title); err != nil {
		return err
	}
	edited, err := os.ReadFile(tmp.Name())
	if err != nil || bytes.Equal(edited, content) {
		return err
	}
	return os.WriteFile(e.Path, edited, 0o600)
}

// editAt has the app open the file at path, and waits for its saved.
func (s *Server) editAt(ctx context.Context, command uint64, path, title string) error {
	id := s.id()
	ch := make(chan Saved, 1)
	s.mu.Lock()
	s.edits[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.edits, id)
		s.mu.Unlock()
	}()
	s.send(Message{Type: TypeEdit, Edit: &Edit{ID: id, Command: command, Path: path, Title: title}})
	select {
	case saved := <-ch:
		switch {
		case saved.Canceled:
			return context.Canceled
		case saved.Error != "":
			return errors.New(saved.Error)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sheet is how a form is put to the app: its title, what its one submit
// says (empty for the app's own word) or its actions, what it is about, and
// the card beside it, read again for every draft. checks is which actions the
// form checks; the others carry the answers as they stand (a decline, a
// detour). Nil checks every submit.
//
// A form with actions takes a submit only with one of them: any other — none,
// or a key it does not offer — is refused on the form, never read as the
// accept or the decline it is not.
type sheet struct {
	title, action, about, aside string
	// leave is what leaving the form does, said beside the key that does it.
	leave   string
	card    func(console.Answers) []console.DetailSection
	actions []FormAction
	checks  func(action console.CardAction) bool
}

// offers reports whether a submitted action is one the sheet takes: any, on a
// form with no actions.
func (sh sheet) offers(action console.CardAction) bool {
	if len(sh.actions) == 0 {
		return action == ""
	}
	return slices.ContainsFunc(sh.actions, func(a FormAction) bool { return a.Key == action })
}

// formed is what a form was submitted with: the action pressed (empty on a
// form with none), the answers as they stood, and, for one the form checks,
// the answer they make.
type formed[T any] struct {
	action  console.CardAction
	answers console.Answers
	value   T
}

// put puts a form to the app for a running command, holding start, until
// it is submitted with an action the form does not check or with answers it
// accepts, or it is dismissed (ok false). A refusal goes back as field
// errors, and the form stays up holding what was typed; a draft is answered
// with the form again, its fields' Applies and its card recomputed for the
// draft. The app sends the fields the person changed, each merged onto what
// it held as it was sent (start).
func put[T any](ctx context.Context, s *Server, command uint64, sh sheet, form console.Form[T], start console.Answers) (got formed[T], ok bool, err error) {
	id := s.id()
	box := newMailbox()
	s.mu.Lock()
	s.forms[id] = box
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.forms, id)
		s.mu.Unlock()
	}()
	answers := maps.Clone(start)
	for {
		card := []DetailSection{}
		if sh.card != nil {
			card = wireDetail(sh.card(answers))
		}
		s.send(Message{Type: TypeForm, Form: &Form{
			ID: id, Command: command, Title: sh.title, Action: sh.action, About: sh.about, Aside: sh.aside, Leave: sh.leave,
			Fields: fields(form.FieldsFor(answers), answers), Note: form.NoteFor(answers), Card: card, Actions: nonNil(sh.actions),
		}})
		for {
			sub, err := box.take(ctx)
			if err != nil {
				return got, false, err
			}
			if sub.Cancel {
				return got, false, nil
			}
			answers = maps.Clone(start)
			for k, v := range sub.Answers {
				answers[k] = v
			}
			if sub.Draft {
				break
			}
			if !sh.offers(sub.Action) {
				s.send(Message{Type: TypeFieldErrors, FieldErrors: &FieldErrors{Form: id, Errors: []FieldError{{Why: "answer with one of the form's actions"}}}})
				continue
			}
			got = formed[T]{action: sub.Action, answers: answers}
			if sh.checks != nil && !sh.checks(sub.Action) {
				return got, true, nil
			}
			value, refusal := form.Answer(answers)
			if refusal != nil {
				s.send(Message{Type: TypeFieldErrors, FieldErrors: &FieldErrors{Form: id, Errors: []FieldError{{Field: refusal.Field, Why: refusal.Why}}}})
				continue
			}
			got.value = value
			return got, true, nil
		}
	}
}

// fill puts a form with one submit to the app until its answers are
// accepted, or it is dismissed (ok false).
func fill[T any](ctx context.Context, s *Server, command uint64, sh sheet, form console.Form[T]) (answer T, ok bool, err error) {
	got, ok, err := put(ctx, s, command, sh, form, form.Opening())
	return got.value, ok, err
}

// mailbox is what the app submitted to one form, in the order it was sent:
// a draft and the submit that follows it are both taken, never one dropped
// for the other, since the reader of the pipe cannot wait for the form.
type mailbox struct {
	mu    sync.Mutex
	queue []Submit
	ready chan struct{}
}

func newMailbox() *mailbox { return &mailbox{ready: make(chan struct{}, 1)} }

func (m *mailbox) put(sub Submit) {
	m.mu.Lock()
	m.queue = append(m.queue, sub)
	m.mu.Unlock()
	select {
	case m.ready <- struct{}{}:
	default:
	}
}

// take is the oldest submission, waiting for one until ctx ends.
func (m *mailbox) take(ctx context.Context) (Submit, error) {
	for {
		m.mu.Lock()
		if len(m.queue) > 0 {
			sub := m.queue[0]
			m.queue = m.queue[1:]
			m.mu.Unlock()
			return sub, nil
		}
		m.mu.Unlock()
		select {
		case <-m.ready:
		case <-ctx.Done():
			return Submit{}, ctx.Err()
		}
	}
}

func fields(in []console.Field, answers console.Answers) []Field {
	out := make([]Field, 0, len(in))
	for _, f := range in {
		out = append(out, Field{
			Key: f.Key, Label: f.Label, Section: f.Section, Hint: f.Hint, Choices: choices(f.Choices),
			Value: answers[f.Key], Placeholder: f.Placeholder, Suffix: f.Suffix, Masked: f.Masked, Locked: f.Locked,
			Applies: f.Applies(answers), Why: f.Why, Required: f.Required != "",
		})
	}
	return out
}

func choices(in []console.Choice) []Choice {
	out := make([]Choice, 0, len(in))
	for _, c := range in {
		out = append(out, Choice{Key: c.Key, Label: c.Label, Hint: c.Hint, Why: c.Why})
	}
	return out
}

// narrate is a narration for a running command, forwarded to the app until
// the command's starter calls done: steps, warnings and the box a create made
// as progress, questions as questions, a file to edit as an edit. A handover
// — a program for the terminal — is declined: the app has no terminal to give
// it.
func (s *Server) narrate(ctx context.Context, command uint64) (n *console.Narration, done func()) {
	n = console.NewNarration()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for ev := range n.Events() {
			switch {
			case ev.Progress != nil:
				s.send(Message{Type: TypeProgress, Progress: &Progress{Command: command, Text: ev.Progress.Text, Warning: ev.Progress.Warning}})
			case ev.Accepted != nil:
				box := wireBox(*ev.Accepted)
				s.send(Message{Type: TypeProgress, Progress: &Progress{Command: command, Accepted: &box}})
			case ev.Asking != nil:
				key, err := s.ask(ctx, command, ev.Asking.Question)
				if err != nil {
					ev.Asking.Decline(err)
				} else {
					ev.Asking.Answer(key)
				}
			case ev.Handover != nil:
				ev.Handover.Done(console.ErrNoFrontend)
			case ev.Editing != nil:
				ev.Editing.Done(s.edit(ctx, command, ev.Editing))
			}
		}
	}()
	return n, func() {
		n.Close()
		<-finished
	}
}

func failed(why string) Result { return Result{Status: StatusFailed, Why: why} }
