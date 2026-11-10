package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

// A frame is kind, stream and length big-endian, then the payload; a frame
// over the limit is refused rather than read.
func TestFramesRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	w := NewWriter(&wire)
	if err := w.Write(frame.Stdout, 7, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(KindJSON, 0, nil); err != nil {
		t.Fatal(err)
	}
	if got := wire.Bytes()[:9]; !bytes.Equal(got, []byte{1, 0, 0, 0, 7, 0, 0, 0, 5}) {
		t.Fatalf("head %v", got)
	}
	for _, want := range []Frame{{frame.Stdout, 7, []byte("hello")}, {KindJSON, 0, []byte{}}} {
		got, err := ReadFrame(&wire)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, %v; want %+v", got, err, want)
		}
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{255, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff})); err == nil {
		t.Fatal("a 4 GiB frame was read")
	}
}

// Hello is answered with the protocol, the console's spec and the session;
// an app of another protocol gets the protocol alone, and nothing it sends
// after is served.
func TestHello(t *testing.T) {
	a := dial(t, newFakeCore("home", running("b1")))
	w := a.hello()
	if w.Protocol != Protocol || w.Spec == nil || w.Session == nil {
		t.Fatalf("welcome %+v", w)
	}
	if !reflect.DeepEqual(*w.Spec, console.NewSpec()) {
		t.Error("the welcome's spec is not the console's")
	}
	if w.Session.PrimaryServer != "home" || w.Session.Directory != "/src" || w.Session.Branch != "main" {
		t.Errorf("session %+v", w.Session)
	}

	old := dial(t, newFakeCore("home", running("b1")))
	old.send(Message{Type: TypeHello, Hello: &Hello{Protocol: 1}})
	got := old.expect(TypeWelcome).Welcome
	if got.Protocol != Protocol || got.Spec != nil || got.Session != nil {
		t.Fatalf("welcome to another protocol %+v", got)
	}
	old.command(Command{Command: "refresh"})
	old.quiet(200 * time.Millisecond)
}

// Every command of the registry is reachable: each is answered, by its ID,
// and an ID the registry does not have is refused rather than dropped.
func TestEveryCommandIsReachable(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()
	ctx := Context{Workspace: "b1", HasWorkspace: true, WorkspaceShown: true, PaneFocused: true, Tree: true, TreeShown: true, TreeFits: true}
	var stream uint32
	for _, c := range console.Commands() {
		stream++
		id := a.command(Command{Command: c.ID, Targets: []string{"b1"}, Context: ctx, Args: Args{Stream: stream, Exec: "sh9"}})
		var r Result
	answer:
		for {
			m := a.message()
			switch {
			case m.Type == TypeQuestion:
				a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: m.Question.ID, Declined: true}})
			case m.Type == TypeForm:
				a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: m.Form.ID, Answers: map[string]string{}, Cancel: true}})
			case m.Type == TypeResult && m.Result.ID == id:
				r = *m.Result
				break answer
			}
		}
		if !slices.Contains([]Status{StatusDone, StatusFailed, StatusCanceled, StatusEnter}, r.Status) {
			t.Errorf("%s: status %q", c.ID, r.Status)
		}
		if strings.Contains(r.Why, "no command") {
			t.Errorf("%s: %s", c.ID, r.Why)
		}
	}
	id := a.command(Command{Command: "launch rockets"})
	if r := a.result(id); r.Status != StatusFailed || !strings.Contains(r.Why, "launch rockets") {
		t.Fatalf("an unknown command: %+v", r)
	}
}

// A verb runs in the core, its narration reaching the app as progress and
// its question as a question; the answer goes back to the command, and the
// listing is read again after it.
func TestVerbNarratesAndAsks(t *testing.T) {
	core := newFakeCore("home", console.Sandbox{ID: "b1", Name: "b1", State: console.StateStopped})
	core.do = func(ctx context.Context, _ console.Verb, _ string, n *console.Narration) (string, error) {
		n.Report("pulling")
		key, err := n.Ask(ctx, console.Question{Title: "Git credentials", Choices: []console.Choice{{Key: "once", Label: "once"}, {Key: "never", Label: "never"}}})
		if err != nil {
			return "", err
		}
		return "chose " + key, nil
	}
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	a.expect(TypeSnapshot)
	id := a.command(Command{Command: "start", Targets: []string{"b1"}})
	if p := a.expect(TypeProgress).Progress; p.Command != id || p.Text != "pulling" {
		t.Fatalf("progress %+v", p)
	}
	q := a.expect(TypeQuestion).Question
	if q.Command != id || q.Title != "Git credentials" || len(q.Choices) != 2 {
		t.Fatalf("question %+v", q)
	}
	core.setListing(running("b1"))
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: "once"}})
	if r := a.result(id); r.Status != StatusDone || r.Note != "chose once" {
		t.Fatalf("result %+v", r)
	}
	if snap := a.expect(TypeSnapshot).Snapshot; snap.Listing == nil || snap.Listing.Boxes[0].State != "running" {
		t.Errorf("the listing was not read again after the verb: %+v", snap)
	}
}

// What a verb asks before it is sent is the core's: purge is sent only on yes.
func TestPurgeAsksFirst(t *testing.T) {
	archived := console.Sandbox{ID: "b1", Name: "b1", State: console.StateArchived}
	core := newFakeCore("home", archived)
	a := dial(t, core)
	a.hello()
	for _, answer := range []string{console.AnswerNo, console.AnswerYes} {
		id := a.command(Command{Command: "purge", Targets: []string{"b1"}})
		q := a.expect(TypeQuestion).Question
		if !q.DefaultNo || !strings.Contains(q.Body, "b1") {
			t.Fatalf("question %+v", q)
		}
		a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: answer}})
		r := a.result(id)
		purged := slices.Contains(core.called(), "do purge b1")
		if answer == console.AnswerNo && (r.Status != StatusCanceled || purged) {
			t.Fatalf("no: %+v, purged %v", r, purged)
		}
		if answer == console.AnswerYes && (r.Status != StatusDone || !purged) {
			t.Fatalf("yes: %+v, purged %v", r, purged)
		}
	}
}

// A form is the core's description; what it refuses comes back as field
// errors with the form still up, a draft is answered with the form again,
// and a dismissed form cancels its command.
func TestSettingsForm(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.settings = console.BoxSettings{Name: "first"}
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "settings", Targets: []string{"b1"}})
	form := a.expect(TypeForm).Form
	if form.Command != id || !strings.HasPrefix(form.Title, "Settings") {
		t.Fatalf("form %+v", form)
	}
	var name *Field
	for i := range form.Fields {
		if form.Fields[i].Key == "name" {
			name = &form.Fields[i]
		}
	}
	if name == nil || name.Value != "first" || !name.Required || !name.Applies {
		t.Fatalf("name field %+v", name)
	}

	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{"name": " "}, Draft: true}})
	if again := a.expect(TypeForm).Form; again.ID != form.ID {
		t.Fatalf("a draft was answered with form %d", again.ID)
	}

	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{"name": ""}}})
	errs := a.expect(TypeFieldErrors).FieldErrors
	if errs.Form != form.ID || len(errs.Errors) != 1 || errs.Errors[0].Field != "name" {
		t.Fatalf("field errors %+v", errs)
	}

	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{"name": "second"}}})
	if r := a.result(id); r.Status != StatusDone {
		t.Fatalf("result %+v", r)
	}
	if core.update == nil || core.update.Name == nil || *core.update.Name != "second" || core.update.DiscoboxAPI != nil {
		t.Fatalf("update %+v: only what changed is sent", core.update)
	}

	id = a.command(Command{Command: "rename", Targets: []string{"b1"}})
	form = a.expect(TypeForm).Form
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{}, Cancel: true}})
	if r := a.result(id); r.Status != StatusCanceled {
		t.Fatalf("a dismissed rename: %+v", r)
	}
	if slices.ContainsFunc(core.called(), func(c string) bool { return strings.HasPrefix(c, "rename") }) {
		t.Fatal("a dismissed rename renamed")
	}
}

// A question asked by one command waits on its own answer while another
// command runs and finishes.
func TestQuestionWhileAnotherCommandRuns(t *testing.T) {
	core := newFakeCore("home", running("b1"), console.Sandbox{ID: "b2", Name: "b2", State: console.StateArchived})
	a := dial(t, core)
	a.hello()
	purge := a.command(Command{Command: "purge", Targets: []string{"b2"}})
	q := a.expect(TypeQuestion).Question
	refresh := a.command(Command{Command: "refresh"})
	if r := a.result(refresh); r.Status != StatusDone {
		t.Fatalf("refresh %+v", r)
	}
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: console.AnswerYes}})
	if r := a.result(purge); r.Status != StatusDone {
		t.Fatalf("purge %+v", r)
	}
}

// Frames the app sends on a stream before it is attached — before its open
// was even read, and while it opens — reach the terminal in order once it
// is; its output and its end go to the app on the stream.
func TestEarlyStreamFramesAreKept(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.gate = make(chan struct{})
	a := dial(t, core)
	a.hello()
	a.resize(5, 90, 30)
	a.frame(frame.Input, 5, []byte("ls\r"))
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 5, Box: "b1", Exec: "sh1", Cols: 80, Rows: 24}})
	a.frame(frame.Input, 5, []byte("pwd\r"))
	a.frame(frame.Repaint, 5, nil)
	eventually(t, "the open is in flight", func() bool { return slices.Contains(core.called(), "open b1 sh1 80x24") })
	close(core.gate)
	if s := a.expect(TypeStream).Stream; s.Stream != 5 || s.State != StreamAttached {
		t.Fatalf("stream %+v", s)
	}
	term := core.opening("sh1")
	want := []string{"resize 90x30", "input ls\r", "input pwd\r", "repaint"}
	eventually(t, "the early frames reached the terminal", func() bool { return reflect.DeepEqual(term.log(), want) })

	a.frame(frame.Input, 5, []byte("exit\r"))
	term.out <- []byte("bye")
	close(term.out)
	var stdout []byte
	for {
		f := a.next()
		if f.Stream != 5 {
			continue
		}
		if f.Kind == frame.Stdout {
			stdout = append(stdout, f.Payload...)
		}
		if f.Kind == frame.Exit {
			if len(f.Payload) != 0 {
				t.Errorf("exit %q", f.Payload)
			}
			break
		}
	}
	if string(stdout) != "bye" {
		t.Errorf("stdout %q", stdout)
	}
	eventually(t, "the terminal was closed", term.isClosed)
	// A frame on an ended stream is dropped, not kept for a stream that will
	// never attach.
	a.frame(frame.Input, 5, []byte("late"))
	a.quiet(100 * time.Millisecond)
	if got := term.log(); got[len(got)-1] != "input exit\r" {
		t.Errorf("after the end: %q", got)
	}
}

// A stream that fails to open says why, and one the app closed while it
// opened is closed as it attaches.
func TestStreamFailsAndClosesWhileOpening(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 1, Box: "b1", Exec: "missing"}})
	if s := a.expect(TypeStream).Stream; s.State != StreamFailed || !strings.Contains(s.Why, "no such session") {
		t.Fatalf("stream %+v", s)
	}

	core.gate = make(chan struct{})
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 2, Box: "b1", Exec: "sh2"}})
	eventually(t, "the open is in flight", func() bool { return slices.Contains(core.called(), "open b1 sh2 80x24") })
	a.send(Message{Type: TypeClose, Close: &Close{Stream: 2}})
	time.Sleep(20 * time.Millisecond)
	close(core.gate)
	eventually(t, "the terminal opened", func() bool { return core.opening("sh2") != nil })
	eventually(t, "the terminal was closed", core.opening("sh2").isClosed)
	a.quiet(100 * time.Millisecond)
}

// A shell is made in the box and attached to the command's stream, and the
// tab it is drawn as is the core's; ending it ends it in the box.
func TestShellAndEnd(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()
	ctx := Context{Workspace: "b1", HasWorkspace: true, WorkspaceShown: true, PaneFocused: true, Tree: true}
	id := a.command(Command{Command: "shell", Targets: []string{"b1"}, Context: ctx, Args: Args{Stream: 3, Cols: 100, Rows: 40}})
	r := a.result(id)
	if r.Status != StatusDone || r.Session == nil || r.Session.Exec != "sh1" || r.Session.Kind != "shell" || r.Session.Title != "bash" {
		t.Fatalf("result %+v", r)
	}
	if !slices.Contains(core.called(), "shell b1 100x40") {
		t.Fatalf("calls %q", core.called())
	}
	id = a.command(Command{Command: "terminal", Context: ctx, Args: Args{Stream: 4}})
	if r := a.result(id); r.Status != StatusDone || r.Session.Kind != "terminal" {
		t.Fatalf("terminal %+v", r)
	}

	id = a.command(Command{Command: "end", Context: ctx, Args: Args{Stream: 3, Exec: "sh1"}})
	if r := a.result(id); r.Status != StatusDone {
		t.Fatalf("end %+v", r)
	}
	if !slices.Contains(core.called(), "end b1 sh1") || !core.opening("sh1").isClosed() {
		t.Fatalf("calls %q", core.called())
	}
	id = a.command(Command{Command: "end", Context: ctx, Args: Args{Exec: console.ExecPrimary}})
	if r := a.result(id); r.Status != StatusFailed {
		t.Fatalf("ending the primary: %+v", r)
	}
	// From the list, a pane command is the core's to refuse.
	id = a.command(Command{Command: "terminal", Context: Context{Workspace: "b1", HasWorkspace: true, WorkspaceShown: true, ListHasKeys: true}, Args: Args{Stream: 6}})
	if r := a.result(id); r.Status != StatusFailed || !strings.Contains(r.Why, "pane") {
		t.Fatalf("terminal from the list: %+v", r)
	}
}

// A command about a box goes to the server that listed it, and a box is
// known by its server and ID together: two servers can list the same ID.
func TestCommandsGoToTheBoxsServer(t *testing.T) {
	home := newFakeCore("home")
	work := newFakeCore("work")
	home.servers["work"] = work
	home.setListing(running("same"), console.Sandbox{ID: "same", Name: "same", Server: "work", State: console.StateRunning, HasRuntime: true})
	a := dial(t, home)
	a.hello()
	id := a.command(Command{Command: "shell", Targets: []string{"work/same"}, Args: Args{Stream: 1}})
	if r := a.result(id); r.Status != StatusDone || r.Box != "work/same" {
		t.Fatalf("result %+v", r)
	}
	if len(home.called()) != 0 || !slices.Contains(work.called(), "shell same 80x24") {
		t.Fatalf("home %q, work %q", home.called(), work.called())
	}
	id = a.command(Command{Command: "stop", Targets: []string{"gone/x"}})
	if r := a.result(id); r.Status != StatusFailed || !strings.Contains(r.Why, "gone/x") {
		t.Fatalf("a box nobody lists: %+v", r)
	}
}

// A subscription sends its topic whole on subscribe, again only when it
// changes, and nothing once ended.
func TestSubscriptions(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.execs = []console.Exec{
		{ID: "claude", Harness: "claude", Command: []string{"claude"}, Live: true, Tty: true, CreatedAt: time.Unix(3, 0)},
		{ID: "sh", Command: []string{"/bin/bash", "-l"}, Live: true, Tty: true, CreatedAt: time.Unix(2, 0)},
		{ID: "files", FilesChanged: true, Live: true, Tty: true},
	}
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	snap := a.expect(TypeSnapshot).Snapshot
	if snap.Sub != 1 || snap.Listing == nil || len(snap.Listing.Boxes) != 1 || snap.Listing.Boxes[0].Key != "b1" || !snap.Listing.Boxes[0].Attachable {
		t.Fatalf("listing %+v", snap)
	}
	a.command(Command{Command: "refresh"})
	a.expect(TypeResult)
	core.setListing(running("b1"), running("b2"))
	a.command(Command{Command: "refresh"})
	if snap := a.expect(TypeSnapshot).Snapshot; len(snap.Listing.Boxes) != 2 {
		t.Fatalf("after a change: %+v", snap)
	}

	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicSessions, Box: "b1"}})
	sessions := a.expect(TypeSnapshot).Snapshot.Sessions
	var got []string
	for _, s := range sessions.Sessions {
		got = append(got, string(s.Kind)+":"+s.Exec+":"+s.Title)
	}
	if want := []string{"primary:primary:", "shell:sh:bash", "terminal:claude:claude"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sessions %q, want %q", got, want)
	}

	a.send(Message{Type: TypeUnsubscribe, Unsubscribe: &Unsubscribe{Sub: 1}})
	a.send(Message{Type: TypeUnsubscribe, Unsubscribe: &Unsubscribe{Sub: 2}})
	time.Sleep(20 * time.Millisecond)
	core.setListing(running("b3"))
	a.command(Command{Command: "refresh"})
	a.expect(TypeResult)
	a.quiet(100 * time.Millisecond)

	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 3, Topic: TopicResources}})
	if r := a.expect(TypeSnapshot).Snapshot.Resources; r == nil || !r.Known || r.CPUCapacity != 8 {
		t.Fatalf("resources %+v", r)
	}
	core.requests = []console.CredentialRequest{{ID: "r1", SandboxID: "b3", Name: "github", Trust: &console.TrustAsk{}}}
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 4, Topic: TopicRequests}})
	if r := a.expect(TypeSnapshot).Snapshot.Requests; len(r.Requests) != 1 || r.Requests[0].Kind != "trust" || r.Requests[0].Box != "b3" {
		t.Fatalf("requests %+v", r)
	}
	// A topic the schema does not have, and so neither does the app: the
	// server still answers it, with why.
	a.offSchema = true
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 5, Topic: "weather"}})
	if s := a.expect(TypeSnapshot).Snapshot; s.Error == "" {
		t.Fatalf("an unknown topic: %+v", s)
	}
	a.offSchema = false
}

// The palette and the action menu are the core's entries for what the app
// picked, with a reason only on what cannot act.
func TestPaletteAndActionsAreTheCores(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "actions", Targets: []string{"b1"}, Context: Context{ListHasKeys: true}})
	r := a.result(id)
	entries := console.Lifecycle([]console.Sandbox{running("b1")})
	if r.Status != StatusEnter || len(r.Entries) != len(entries) {
		t.Fatalf("actions %+v", r)
	}
	for i, e := range r.Entries {
		if e.Command != entries[i].ID || e.Enabled != entries[i].Enabled || (e.Why == "") != e.Enabled {
			t.Errorf("entry %d %+v, want %s", i, e, entries[i].ID)
		}
	}
	a.result(a.command(Command{Command: "refresh"}))
	id = a.command(Command{Command: "palette", Targets: []string{"b1"}})
	r = a.result(id)
	if r.Status != StatusEnter || len(r.Entries) != len(console.Commands()) {
		t.Fatalf("palette %+v", r)
	}
	for _, e := range r.Entries {
		if e.Command == "refresh" && e.Uses != 1 {
			t.Errorf("refresh used %d times", e.Uses)
		}
	}
}

// A pasted image lands in the box, and its path is the answer.
func TestUpload(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeUpload, Upload: &Upload{ID: 9, Box: "b1", Name: "shot.png", Data: []byte{1, 2, 3}}})
	if r := a.result(9); r.Status != StatusDone || r.Path != "/tmp/shot.png" {
		t.Fatalf("result %+v", r)
	}
	if !slices.Contains(core.called(), "put b1 shot.png 3") {
		t.Fatalf("calls %q", core.called())
	}
}

// A command still running when the app goes is canceled, and the bridge
// ends cleanly.
func TestAppGoingCancelsCommands(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	stopped := make(chan error, 1)
	core.do = func(ctx context.Context, _ console.Verb, _ string, _ *console.Narration) (string, error) {
		<-ctx.Done()
		stopped <- ctx.Err()
		return "", ctx.Err()
	}
	a := dial(t, core)
	a.hello()
	a.command(Command{Command: "stop", Targets: []string{"b1"}})
	eventually(t, "the verb is running", func() bool { return slices.Contains(core.called(), "do stop b1") })
	_ = a.out.Close()
	select {
	case err := <-a.done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge did not end")
	}
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the running verb was not canceled")
	}
}

// Nothing the bridge sends is null: lists are [] when empty.
func TestNoNullsOnTheWire(t *testing.T) {
	l := console.NewList(console.Session{}, console.ListStaleAfter)
	l.SetAll([]console.Sandbox{{ID: "b"}})
	listing := wireListing(&l)
	data, err := json.Marshal(Message{Type: TypeSnapshot, Snapshot: &Snapshot{Listing: &listing}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "null") {
		t.Fatalf("null on the wire: %s", data)
	}
}

// A box command whose flow is the app's is still refused by the core when the
// box cannot take it: a page with no request waiting, an issue with no tag.
func TestAppFlowsAreRuledByTheCore(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()
	for _, id := range []string{"requests", "issue"} {
		if r := a.result(a.command(Command{Command: id, Targets: []string{"b1"}})); r.Status != StatusFailed || r.Why == "" {
			t.Errorf("%s with nothing to show: %+v", id, r)
		}
	}
	core.mu.Lock()
	core.requests = []console.CredentialRequest{{ID: "r1", SandboxID: "b1", Name: "github"}}
	core.mu.Unlock()
	// The app hears of the request from the requests topic; a command after
	// that sees it.
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicRequests}})
	if r := a.expect(TypeSnapshot).Snapshot.Requests; len(r.Requests) != 1 {
		t.Fatalf("requests %+v", r)
	}
	if r := a.result(a.command(Command{Command: "requests", Targets: []string{"b1"}})); r.Status != StatusEnter || r.Box != "b1" {
		t.Errorf("requests with one waiting: %+v", r)
	}
}

// A draft and the submit that follows it at once are both taken, in order:
// the submit is never dropped for the draft.
func TestDraftThenSubmitAtOnce(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "rename", Targets: []string{"b1"}})
	form := a.expect(TypeForm).Form
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{"name": "n"}, Draft: true}})
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{"name": "new name"}}})
	if r := a.result(id); r.Status != StatusDone || r.Note != "renamed to new name" {
		t.Fatalf("result %+v", r)
	}
}

// A session that could not be ended goes on, and so does its stream.
func TestEndThatFailsKeepsTheStream(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 1, Box: "b1", Exec: "stuck"}})
	a.expect(TypeStream)
	ctx := Context{Workspace: "b1", HasWorkspace: true, WorkspaceShown: true, PaneFocused: true}
	if r := a.result(a.command(Command{Command: "end", Context: ctx, Args: Args{Stream: 1, Exec: "stuck"}})); r.Status != StatusFailed {
		t.Fatalf("result %+v", r)
	}
	term := core.opening("stuck")
	if term.isClosed() {
		t.Fatal("the stream of a session still running was closed")
	}
	a.frame(frame.Input, 1, []byte("still here"))
	eventually(t, "input reaches the session", func() bool { return slices.Contains(term.log(), "input still here") })
}

// An app of protocol 1, which had no envelope, is told in its own shape that
// it is older, rather than left waiting.
func TestProtocolOneApp(t *testing.T) {
	a := dial(t, newFakeCore("home", running("b1")))
	a.frame(KindJSON, 0, []byte(`{"id":0,"op":"hello"}`))
	f := a.next()
	var got struct {
		ID    int    `json:"id"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(f.Payload, &got); err != nil || got.ID != 0 || !strings.Contains(got.Error, "the app is older") {
		t.Fatalf("%s: %v", f.Payload, err)
	}
}

// Which boxes a refused credential affects is the core's rule: a harness's
// credential affects every box running that harness, and the command that
// opens it follows the same rule.
func TestRejectionsAreTheCoresRule(t *testing.T) {
	one, two, other := running("b1"), running("b2"), running("b3")
	one.HarnessID, two.HarnessID, other.HarnessID = "h1", "h1", "h2"
	core := newFakeCore("home", one, two, other)
	core.rejections = []console.SecretRejection{{SecretID: "s1", SandboxID: "b1", HarnessConfigID: "h1"}}
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	a.expect(TypeSnapshot)
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicRequests}})
	r := a.expect(TypeSnapshot).Snapshot.Requests
	if len(r.Rejections) != 1 || !reflect.DeepEqual(r.Rejections[0].Boxes, []string{"b1", "b2"}) {
		t.Fatalf("rejections %+v", r.Rejections)
	}
	if got := a.result(a.command(Command{Command: "rejections", Targets: []string{"b2"}})); got.Status != StatusEnter {
		t.Fatalf("rejections on a box of the harness: %+v", got)
	}
	if got := a.result(a.command(Command{Command: "rejections", Targets: []string{"b3"}})); got.Status != StatusFailed {
		t.Fatalf("rejections on another harness's box: %+v", got)
	}
}

// A box that is not running opens its primary, whose attach is what waits for
// it, and nothing else.
func TestSessionsOfABoxNotRunning(t *testing.T) {
	core := newFakeCore("home", console.Sandbox{ID: "b1", Name: "b1", State: console.StateStopped, HasRuntime: true})
	core.stopped = true
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicSessions, Box: "b1"}})
	s := a.expect(TypeSnapshot).Snapshot.Sessions
	if s.Running || len(s.Sessions) != 1 || s.Sessions[0].Kind != "primary" {
		t.Fatalf("sessions %+v", s)
	}
}

// What a box that is not up uses is nothing, and its git state is the core's
// reading of it.
func TestWireBoxReadsTheCore(t *testing.T) {
	b := console.Sandbox{ID: "b", State: console.StateStopped, Commit: "aaa",
		Git: console.GitState{Known: true, Commit: "bbb"}, Usage: console.Usage{Known: true, CPUPercent: 50, MemoryBytes: 9}}
	w := wireBox(b)
	if w.CPU != 0 || w.MemoryBytes != 0 || !w.Ahead || w.Dirty {
		t.Fatalf("%+v", w)
	}
}

// A topic about a box no server lists says so once, rather than reading the
// listing again on every beat.
func TestTopicAboutAGoneBox(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicSessions, Box: "gone"}})
	if s := a.expect(TypeSnapshot).Snapshot; !strings.Contains(s.Error, "gone") {
		t.Fatalf("snapshot %+v", s)
	}
	a.quiet(100 * time.Millisecond)
}

// A topic about a box whose server could not be reached says why, and follows
// the box once the server answers again.
func TestTopicAboutABoxWhoseServerComesBack(t *testing.T) {
	home := newFakeCore("home")
	work := newFakeCore("work")
	home.setListing(console.Sandbox{ID: "w1", Name: "w1", Server: "work", State: console.StateRunning, HasRuntime: true})
	a := dial(t, home)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicSessions, Box: "work/w1"}})
	if s := a.expect(TypeSnapshot).Snapshot; !strings.Contains(s.Error, "not answering") {
		t.Fatalf("snapshot %+v", s)
	}
	home.mu.Lock()
	home.servers["work"] = work
	home.mu.Unlock()
	if s := a.expect(TypeSnapshot).Snapshot; s.Sessions == nil || s.Sessions.Sessions[0].Kind != "primary" {
		t.Fatalf("after the server came back: %+v", s)
	}
}

// A box whose server did not answer the listing is missing from it, not gone:
// a topic about it says why and follows it once its server answers again.
func TestTopicAboutABoxOnAServerNotAnswering(t *testing.T) {
	home := newFakeCore("home")
	work := newFakeCore("work")
	home.servers["work"] = work
	home.mu.Lock()
	home.listing = console.Listing{Unreachable: []string{"work"}}
	home.mu.Unlock()
	a := dial(t, home)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicSessions, Box: "work/w1"}})
	if s := a.expect(TypeSnapshot).Snapshot; !strings.Contains(s.Error, "not answering") {
		t.Fatalf("snapshot %+v", s)
	}
	home.setListing(console.Sandbox{ID: "w1", Name: "w1", Server: "work", State: console.StateRunning, HasRuntime: true})
	if s := a.expect(TypeSnapshot).Snapshot; s.Sessions == nil || s.Sessions.Sessions[0].Kind != "primary" {
		t.Fatalf("after the server answered: %+v", s)
	}
}

// The listing is the core's list: its rows as drawn, a box under the one that
// made it, what the archived would add, and the filter's choices. A filter
// the app subscribes with narrows it, and is saved for the next window, which
// opens on it when it subscribes with none.
func TestTheListingIsTheCoresList(t *testing.T) {
	parent := running("p")
	parent.Tags = []string{"wip"}
	kid := running("k")
	kid.CreatedBy = "p"
	old := console.Sandbox{ID: "old", Name: "old", State: console.StateArchived, Tags: []string{"wip"}}
	core := newFakeCore("home", kid, parent, old)
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	l := a.expect(TypeSnapshot).Snapshot.Listing
	var rows []string
	for _, r := range l.Rows {
		rows = append(rows, fmt.Sprintf("%s:%d:%v", r.Box, r.Depth, r.Last))
	}
	if want := []string{"p:0:false", "k:1:true"}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows %q, want %q (the box under the one that made it)", rows, want)
	}
	if len(l.Boxes) != 3 || l.Archived != 1 || l.Label != "all discoboxes" || l.Filter.Archived {
		t.Fatalf("listing %+v", l)
	}
	var wip *FilterChoice
	for i, c := range l.Choices {
		if c.Group == console.GroupTag && c.Label == "#wip" {
			wip = &l.Choices[i]
		}
	}
	if wip == nil || wip.Marked || !reflect.DeepEqual(wip.Mark.Tags, []string{"wip"}) || wip.Detail != "1 box" {
		t.Fatalf("the #wip choice %+v in %+v", wip, l.Choices)
	}

	// Its mark, with the archived shown: the subscription narrowed to it.
	f := wip.Mark
	f.Archived = true
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicListing, Filter: &f}})
	l = a.expect(TypeSnapshot).Snapshot.Listing
	rows = rows[:0]
	for _, r := range l.Rows {
		rows = append(rows, r.Box)
	}
	if want := []string{"p", "old"}; !reflect.DeepEqual(rows, want) || l.Label != "#wip" || !l.Filter.Archived {
		t.Fatalf("narrowed to #wip with the archived: rows %q, label %q, filter %+v", rows, l.Label, l.Filter)
	}
	core.mu.Lock()
	saved := core.view
	core.mu.Unlock()
	if !reflect.DeepEqual(saved.Tags, []string{"wip"}) {
		t.Fatalf("the filter was not saved for the next window: %+v", saved)
	}

	// The next window opens on it; whether the archived are listed is not kept.
	b := dial(t, core)
	b.hello()
	b.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	if l := b.expect(TypeSnapshot).Snapshot.Listing; l.Label != "#wip" || l.Filter.Archived || len(l.Rows) != 1 {
		t.Fatalf("the next window opened on %q (archived %v, %d rows), want #wip", l.Label, l.Filter.Archived, len(l.Rows))
	}
}

// A listing subscribed with no filter on a server with no boxes still sends
// its snapshot: an empty list is an answer, not a wait.
func TestAnEmptyListingIsAnswered(t *testing.T) {
	a := dial(t, newFakeCore("home"))
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	if l := a.expect(TypeSnapshot).Snapshot.Listing; l == nil || len(l.Rows) != 0 || len(l.Boxes) != 0 {
		t.Fatalf("listing %+v", l)
	}
}
