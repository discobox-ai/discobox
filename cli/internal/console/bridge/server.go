package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	// boxes is every box of the last listing, by the key the app knows it by,
	// and unanswered the servers that listing had no answer from, whose boxes
	// are missing from it rather than gone.
	boxes      map[string]console.Sandbox
	unanswered map[string]bool
	// kick is closed to read the listing again now (relist).
	kick chan struct{}
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
}

// NewServer is a bridge serving ds, writing to the app on out.
func NewServer(ds console.DataSource, out io.Writer) *Server {
	s := &Server{
		ds:        ds,
		w:         NewWriter(out),
		reg:       console.NewRegistry(ds),
		boxes:     map[string]console.Sandbox{},
		kick:      make(chan struct{}),
		subs:      map[uint32]context.CancelFunc{},
		forms:     map[uint64]*mailbox{},
		questions: map[uint64]chan Answer{},
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
	s.mu.Lock()
	s.session = session
	s.mu.Unlock()
	spec := console.NewSpec()
	s.send(Message{Type: TypeWelcome, Welcome: &Welcome{Protocol: Protocol, Spec: &spec, Session: &Session{
		PrimaryServer: s.ds.PrimaryServer(), Project: session.Project,
		Directory: session.Directory, Branch: session.Branch, Host: session.Host,
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
	// Saved answers an edit, and nothing the core runs asks for one yet: the
	// harness and tool files are edited by the waves that bring their pages.
}

// open attaches a stream to one of a box's sessions.
func (s *Server) open(ctx context.Context, o Open) {
	box, src, err := s.source(ctx, o.Box)
	if err != nil {
		s.streams.fail(o.Stream, err)
		return
	}
	cols, rows := size(o.Cols, o.Rows)
	term, err := src.OpenExec(ctx, box.ID, o.Exec, cols, rows)
	if err != nil {
		s.streams.fail(o.Stream, err)
		return
	}
	s.streams.attach(o.Stream, term)
}

// size is a pane's size, or a terminal's default for one the app has not
// laid out yet; the resize that follows corrects it.
func size(cols, rows int) (int, int) {
	if cols <= 0 || rows <= 0 {
		return 80, 24
	}
	return cols, rows
}

func (s *Server) upload(ctx context.Context, u Upload) Result {
	box, src, err := s.source(ctx, u.Box)
	if err != nil {
		return failed(err.Error())
	}
	path, err := src.PutImage(ctx, box.ID, u.Name, u.Data)
	if err != nil {
		return failed(err.Error())
	}
	return Result{Status: StatusDone, Path: path}
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
	s.boxes, s.unanswered = boxes, unanswered
	s.mu.Unlock()
	return listing, nil
}

// listed is the session and the boxes of the last listing.
func (s *Server) listed() (console.Session, []console.Sandbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	boxes := make([]console.Sandbox, 0, len(s.boxes))
	for _, b := range s.boxes {
		boxes = append(boxes, b)
	}
	return s.session, boxes
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
	s.send(Message{Type: TypeQuestion, Question: &Question{
		ID: id, Command: command, Title: q.Title, Body: q.Body, Choices: choices(q.Choices), DefaultNo: q.DefaultNo,
	}})
	select {
	case a := <-ch:
		if a.Declined {
			return "", context.Canceled
		}
		return a.Key, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// fill puts a form to the app for a running command until its answers are
// accepted, or it is dismissed (ok false). A refusal goes back as field
// errors, and the form stays up holding what was typed; a draft is answered
// with the form again, its fields' Applies recomputed for the draft.
func fill[T any](ctx context.Context, s *Server, command uint64, title string, form console.Form[T]) (answer T, ok bool, err error) {
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
	answers := form.Opening()
	for {
		s.send(Message{Type: TypeForm, Form: &Form{ID: id, Command: command, Title: title, Fields: fields(form.Fields, answers)}})
		for {
			sub, err := box.take(ctx)
			if err != nil {
				return answer, false, err
			}
			if sub.Cancel {
				return answer, false, nil
			}
			answers = form.Opening()
			for k, v := range sub.Answers {
				answers[k] = v
			}
			if sub.Draft {
				break
			}
			got, refusal := form.Answer(answers)
			if refusal != nil {
				s.send(Message{Type: TypeFieldErrors, FieldErrors: &FieldErrors{Form: id, Errors: []FieldError{{Field: refusal.Field, Why: refusal.Why}}}})
				continue
			}
			return got, true, nil
		}
	}
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
			Value: f.Value, Placeholder: f.Placeholder, Suffix: f.Suffix, Masked: f.Masked, Locked: f.Locked,
			Applies: f.Applies(answers), Why: f.Why, Required: f.Required != "",
		})
	}
	return out
}

func choices(in []console.Choice) []Choice {
	out := make([]Choice, 0, len(in))
	for _, c := range in {
		out = append(out, Choice{Key: c.Key, Label: c.Label, Hint: c.Hint})
	}
	return out
}

// narrate is a narration for a running command, forwarded to the app until
// the command's starter calls done: steps, warnings and the box a create made
// as progress, questions as questions. A handover — a program for the
// terminal — is declined: the app has no terminal to give it.
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
			}
		}
	}()
	return n, func() {
		n.Close()
		<-finished
	}
}

func failed(why string) Result { return Result{Status: StatusFailed, Why: why} }
