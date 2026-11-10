package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// A field's value is what the core reads it holding — as one line, a picker
// on the key it opens on, and after a draft what the draft made it — and a
// submit carrying only what changed saves nothing else: a stored name the
// field folds is no rename.
func TestFormValuesAreTheCores(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.settings = console.BoxSettings{Name: "first\tname"}
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "settings", Targets: []string{"b1"}})
	form := a.expect(TypeForm).Form
	value := func(f *Form, key string) string {
		for _, x := range f.Fields {
			if x.Key == key {
				return x.Value
			}
		}
		t.Fatalf("no field %s", key)
		return ""
	}
	if got := value(form, "name"); got != "first name" {
		t.Fatalf("name opens on %q, not as its field holds it", got)
	}
	if got := value(form, "api"); got != "no" {
		t.Fatalf("api opens on %q", got)
	}
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{"api": "yes"}, Draft: true}})
	if again := a.expect(TypeForm).Form; value(again, "api") != "yes" || value(again, "name") != "first name" {
		t.Fatalf("after a draft: api %q, name %q", value(again, "api"), value(again, "name"))
	}
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{"api": "yes"}}})
	if r := a.result(id); r.Status != StatusDone {
		t.Fatalf("result %+v", r)
	}
	if core.update == nil || core.update.Name != nil || core.update.DiscoboxAPI == nil || !*core.update.DiscoboxAPI {
		t.Fatalf("update %+v: only the API access changed", core.update)
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
	// Which take images and the image keys is the core's (console.Tab).
	for _, s := range sessions.Sessions {
		if s.Images != true || s.ImageKeys != (s.Kind != TabShell) {
			t.Errorf("%s: images %v, image keys %v", s.Exec, s.Images, s.ImageKeys)
		}
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

// The action menu, the menus' commands and the palette are the core's: its
// entries for what the app picked, with a reason only on what cannot act, and
// its results for what the palette asks.
func TestActionsCommandsAndPaletteAreTheCores(t *testing.T) {
	stopped := running("b2")
	stopped.State = console.StateStopped
	core := newFakeCore("home", running("b1"), stopped)
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
	if r := a.result(a.command(Command{Command: "palette"})); r.Status != StatusEnter {
		t.Fatalf("palette %+v", r)
	}

	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicCommands, Targets: []string{"b2"}, Context: &Context{ListHasKeys: true}}})
	snap := a.expect(TypeSnapshot).Snapshot
	if snap.Commands == nil || len(snap.Commands.Entries) != len(console.Commands()) {
		t.Fatalf("commands %+v", snap)
	}
	for _, e := range snap.Commands.Entries {
		switch {
		case (e.Command == "refresh" || e.Command == "palette") && e.Uses != 1:
			t.Errorf("%s used %d times", e.Command, e.Uses)
		case e.Command == "stop" && (e.Enabled || e.Why == ""):
			t.Errorf("stop is offered for a stopped box: %+v", e)
		case e.Command == "start" && !e.Enabled:
			t.Errorf("start is not offered for a stopped box: %+v", e)
		}
	}

	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicPalette, Context: &Context{}, Palette: &PaletteQuery{Text: "b2"}}})
	p := a.expect(TypeSnapshot).Snapshot.Palette
	if p == nil || len(p.Rows) == 0 || p.Rows[0].Box != "b2" || p.Commit != nil {
		t.Fatalf("palette for b2: %+v", p)
	}
	// A box command's alias and a space commit it as the chip: the rows are
	// the boxes it can act on.
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 3, Topic: TopicPalette, Context: &Context{}, Palette: &PaletteQuery{Text: "start "}}})
	p = a.expect(TypeSnapshot).Snapshot.Palette
	if p == nil || p.Commit == nil || p.Commit.Command != "start" || len(p.Rows) != 1 || p.Rows[0].Box != "b2" {
		t.Fatalf("palette for start: %+v", p)
	}
	// A box chip offers the commands it can take.
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 4, Topic: TopicPalette, Context: &Context{}, Palette: &PaletteQuery{Box: "b1", Text: "stop"}}})
	p = a.expect(TypeSnapshot).Snapshot.Palette
	if p == nil || len(p.Rows) == 0 || p.Rows[0].Command == nil || p.Rows[0].Command.Command != "stop" || p.Rows[0].Why != "" {
		t.Fatalf("palette on b1 for stop: %+v", p)
	}
}

// The commands and the palette are read from the last listing, and never
// list: a workspace no listing has would list again, which would wake them
// again, without end. They answer what the bridge last read, with the server
// notes of every server searched.
func TestCommandsAndPaletteNeverList(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()
	a.result(a.command(Command{Command: "actions", Targets: []string{"b1"}, Context: Context{ListHasKeys: true}}))
	core.mu.Lock()
	before := core.lists
	core.mu.Unlock()
	gone := &Context{Workspace: "gone", HasWorkspace: true, WorkspaceShown: true, PaneFocused: true}
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicCommands, Context: gone}})
	if s := a.expect(TypeSnapshot).Snapshot; s.Commands == nil {
		t.Fatalf("commands %+v", s)
	}
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicPalette, Context: gone, Palette: &PaletteQuery{Text: "b1"}}})
	p := a.expect(TypeSnapshot).Snapshot.Palette
	if p == nil || len(p.Rows) == 0 || p.Rows[0].Box != "b1" || p.Partial == nil || p.Stale == nil {
		t.Fatalf("palette %+v", p)
	}
	a.quiet(200 * time.Millisecond)
	core.mu.Lock()
	after := core.lists
	core.mu.Unlock()
	if after != before {
		t.Fatalf("the topics listed %d times", after-before)
	}
}

// A pasted image lands in the box under the core's name for it, never the
// name the app sent, and its path is the answer.
func TestUpload(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeUpload, Upload: &Upload{ID: 9, Box: "b1", Name: "../shot.png", Data: []byte{1, 2, 3}}})
	r := a.result(9)
	if r.Status != StatusDone || len(r.Paths) != 1 || !strings.HasSuffix(r.Paths[0], "-0-shot.png") || strings.Contains(r.Paths[0], "..") {
		t.Fatalf("result %+v", r)
	}
	name := strings.TrimPrefix(r.Paths[0], "/tmp/")
	if !slices.Contains(core.called(), "put b1 "+name+" 3") {
		t.Fatalf("calls %q", core.called())
	}

	a.send(Message{Type: TypeUpload, Upload: &Upload{ID: 10, Box: "b1", Name: "empty.png"}})
	if r := a.result(10); r.Status != StatusFailed {
		t.Fatalf("an upload with no image: %+v", r)
	}
}

// Files dropped on a pane are read here, and carried in when every one is an
// image, in the order dropped; a drop that is not all images carries nothing
// in and is answered with no paths, for the app to paste them as text.
func TestUploadDroppedFiles(t *testing.T) {
	dir := t.TempDir()
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	shot, other, notes := filepath.Join(dir, "shot.png"), filepath.Join(dir, "b.png"), filepath.Join(dir, "notes.txt")
	for _, f := range []string{shot, other, notes} {
		if err := os.WriteFile(f, png, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	core := newFakeCore("home", running("b1"))
	a := dial(t, core)
	a.hello()

	a.send(Message{Type: TypeUpload, Upload: &Upload{ID: 4, Box: "b1", Files: []string{shot, other}}})
	r := a.result(4)
	if r.Status != StatusDone || len(r.Paths) != 2 || !strings.HasSuffix(r.Paths[0], "-0-shot.png") || !strings.HasSuffix(r.Paths[1], "-1-b.png") {
		t.Fatalf("result %+v", r)
	}

	before := len(core.called())
	a.send(Message{Type: TypeUpload, Upload: &Upload{ID: 5, Box: "b1", Files: []string{shot, notes}}})
	if r := a.result(5); r.Status != StatusDone || len(r.Paths) != 0 {
		t.Fatalf("a drop that is not all images: %+v", r)
	}
	if after := core.called()[before:]; slices.ContainsFunc(after, func(c string) bool { return strings.HasPrefix(c, "put ") }) {
		t.Fatalf("a drop that is not all images carried something in: %q", after)
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

	// A box revealed for the palette shows whatever the filter hid, and is
	// never saved over it.
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 3, Topic: TopicListing, Filter: &f, Reveal: "k"}})
	l = a.expect(TypeSnapshot).Snapshot.Listing
	if len(l.Rows) != 2 || l.Label != "all discoboxes" || l.Filter.Archived {
		t.Fatalf("revealing k: rows %+v, label %q", l.Rows, l.Label)
	}
	core.mu.Lock()
	saved = core.view
	core.mu.Unlock()
	if !reflect.DeepEqual(saved.Tags, []string{"wip"}) {
		t.Fatalf("a reveal was saved over the filter: %+v", saved)
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

// What a session's ending does is the core's (console.Tab.End), said before
// the stream's Exit frame: the primary ends the workspace, a shell that
// finished is held with its last screen, a tool goes, and a session that
// could not be opened goes with its tab, or with the workspace for the
// primary.
func TestStreamEndingsAreTheCores(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.execs = []console.Exec{{ID: "diff", Tool: "diff", Command: []string{"lazygit"}, Live: true, Tty: true}}
	a := dial(t, core)
	a.hello()
	// The tool is known as one from the box's sessions.
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicSessions, Box: "b1"}})
	a.expect(TypeSnapshot)
	ctx := Context{Workspace: "b1", HasWorkspace: true, WorkspaceShown: true, PaneFocused: true, Tree: true}
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 1, Box: "b1", Exec: console.ExecPrimary}})
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 2, Box: "b1", Exec: "diff"}})
	if r := a.result(a.command(Command{Command: "shell", Targets: []string{"b1"}, Context: ctx, Args: Args{Stream: 3}})); r.Status != StatusDone {
		t.Fatalf("shell %+v", r)
	}
	eventually(t, "every session opened", func() bool {
		return core.opening(console.ExecPrimary) != nil && core.opening("diff") != nil && core.opening("sh1") != nil
	})
	ended := func(stream uint32) Stream {
		t.Helper()
		for {
			m := a.expect(TypeStream)
			if m.Stream.Stream == stream && m.Stream.State == StreamEnded {
				return *m.Stream
			}
		}
	}
	close(core.opening("sh1").out)
	if s := ended(3); s.End != EndHeld || s.Status != "finished" || s.Why != "" {
		t.Fatalf("a shell that finished: %+v", s)
	}
	close(core.opening("diff").out)
	if s := ended(2); s.End != EndTab {
		t.Fatalf("a tool that finished: %+v", s)
	}
	close(core.opening(console.ExecPrimary).out)
	if s := ended(1); s.End != EndWorkspace {
		t.Fatalf("the primary that finished: %+v", s)
	}

	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 4, Box: "b1", Exec: "missing"}})
	if s := a.expect(TypeStream).Stream; s.State != StreamFailed || s.End != EndTab {
		t.Fatalf("a session that could not be opened: %+v", s)
	}
	core.primaryGone = true
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 5, Box: "b1", Exec: console.ExecPrimary}})
	if s := a.expect(TypeStream).Stream; s.State != StreamFailed || s.End != EndWorkspace {
		t.Fatalf("a primary that could not be opened: %+v", s)
	}
}

// The header topic is the core's header of the box as the listing has it,
// with its ports at the local ports of a forward that lasts as long as the
// subscription; a forward that cannot be opened leaves the ports unlinked
// and says why.
func TestHeaderIsTheCores(t *testing.T) {
	box := running("b1")
	box.Repository = "https://github.com/acme/foo"
	box.Tags = []string{"issue=7", "wip", "blocked"}
	box.Branch, box.Commit, box.Dirty = "main", "abc", true
	box.Diff = console.DiffStat{Known: true, Added: 3, Deleted: 1, Files: 1}
	box.Ports = []console.Port{{Number: 5432, Protocol: "tcp"}, {Number: 3000, Protocol: "http"}}
	core := newFakeCore("home", box)
	core.forward = newFakeForward()
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicHeader, Box: "b1"}})
	h := a.expect(TypeSnapshot).Snapshot.Header
	if h == nil || h.ID != "b1" || h.Base != "main@abc*" || h.Tone != ToneDirty || h.Changes != "" || h.Blocked != console.StatusBlocked || !h.Diff || h.Added != 3 {
		t.Fatalf("header %+v", h)
	}
	if len(h.Work) != 1 || h.Work[0].Label != "issue #7" || h.Work[0].URL != "https://github.com/acme/foo/issues/7" {
		t.Fatalf("work %+v", h.Work)
	}
	if !reflect.DeepEqual(h.Tags, []string{"#wip"}) {
		t.Fatalf("tags %q", h.Tags)
	}
	want := []PortGroup{{Protocol: "http", Ports: []Link{{Label: "3000"}}}, {Protocol: "tcp", Ports: []Link{{Label: "5432"}}}}
	if !reflect.DeepEqual(h.Ports, want) {
		t.Fatalf("ports %+v, want %+v", h.Ports, want)
	}
	core.forward.bind(console.Binding{Port: 3000, Local: 3001}, console.Binding{Port: 5432, Local: 5432})
	h = a.expect(TypeSnapshot).Snapshot.Header
	want = []PortGroup{{Protocol: "http", Ports: []Link{{Label: "3001->3000", URL: "http://localhost:3001"}}}, {Protocol: "tcp", Ports: []Link{{Label: "5432"}}}}
	if !reflect.DeepEqual(h.Ports, want) {
		t.Fatalf("forwarded ports %+v, want %+v", h.Ports, want)
	}
	// The forward goes with the subscription.
	a.send(Message{Type: TypeUnsubscribe, Unsubscribe: &Unsubscribe{Sub: 1}})
	eventually(t, "the forward was closed", core.forward.isClosed)

	core.forwardErr = errors.New("no route to the box")
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicHeader, Box: "b1"}})
	h = a.expect(TypeSnapshot).Snapshot.Header
	if h.Unforwarded != "no route to the box" || len(h.Ports) != 2 || h.Ports[0].Ports[0].URL != "" {
		t.Fatalf("an unforwarded header %+v", h)
	}
}

// A box's services are the core's: the tabs of the ones worth one among its
// sessions, in the core's order, each on its run; a service's tab opens on
// its card and last output when it is not running, and on its process after
// its history when it is; the services command lists them for its menu, and
// with a service and a verb runs that verb as the core offers it.
func TestServicesAreTheCores(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	code := 2
	core.services = []console.Service{
		{ID: "web", Name: "Web", Status: "running", ExecID: "exec_web"},
		{ID: "worker", Status: "stopped"},
		{ID: "migrate", Status: "exited", ExitCode: &code},
		{ID: "broken", Status: "stopped", Problem: "it is not executable"},
	}
	core.serviceLogs = map[string]string{"web": "listening on :3000\n", "migrate": "migrated 3 tables\n"}
	core.execs = []console.Exec{{ID: "sh", Command: []string{"bash"}, Live: true, Tty: true, CreatedAt: time.Unix(2, 0)}}
	a := dial(t, core)
	a.hello()

	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicSessions, Box: "b1"}})
	sessions := a.expect(TypeSnapshot).Snapshot.Sessions
	var got []string
	for _, s := range sessions.Sessions {
		got = append(got, fmt.Sprintf("%s:%s:%s:%v:%s", s.Kind, s.Exec, s.Title, s.Live, s.Status))
	}
	want := []string{
		"service:service:web:Web:true:", "service:service:migrate:migrate ·:false:exited", "service:service:broken:broken ✗:false:cannot run",
		"primary:primary::true:", "shell:sh:bash:true:",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sessions %q, want %q", got, want)
	}
	if s := sessions.Sessions[0]; s.Service != "web" || s.Run != core.services[0].RunKey() {
		t.Fatalf("a service's tab %+v, want its service and run", s)
	}

	// Each stream's output, by stream, as it arrives.
	output := func(stream uint32, want string) {
		t.Helper()
		var got []byte
		for !strings.Contains(string(got), want) {
			if f := a.next(); f.Kind == frame.Stdout && f.Stream == stream {
				got = append(got, f.Payload...)
			}
		}
	}
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 1, Box: "b1", Exec: console.ServiceTabID("migrate")}})
	output(1, "migrated 3 tables")
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 2, Box: "b1", Exec: console.ServiceTabID("web")}})
	output(2, "listening on :3000")
	if !slices.Contains(core.called(), "open b1 exec_web 80x24") || slices.ContainsFunc(core.called(), func(c string) bool { return strings.Contains(c, "service:") }) {
		t.Fatalf("calls %q: want the running service's exec opened, and nothing opened by a tab's key", core.called())
	}
	// A service's tab the bridge has no service for is no session to open: it
	// fails, and the tab goes saying why.
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 9, Box: "b1", Exec: console.ServiceTabID("gone")}})
	if st := a.expect(TypeStream).Stream; st.Stream != 9 || st.State != StreamFailed || st.End != EndTab || st.Why != "the service is no longer listed" {
		t.Fatalf("a service no longer listed: %+v", st)
	}

	ctx := Context{Workspace: "b1", HasWorkspace: true, WorkspaceShown: true, PaneFocused: true, Tree: true}
	r := a.result(a.command(Command{Command: "services", Targets: []string{"b1"}, Context: ctx}))
	if r.Status != StatusEnter || r.Box != "b1" || len(r.Services) != 4 {
		t.Fatalf("services %+v", r)
	}
	if s := r.Services[3]; s.ID != "broken" || s.Detail != "cannot run: it is not executable" || len(s.Actions) != 3 || s.Actions[0].Enabled || s.Actions[0].Why != "it is not executable" {
		t.Fatalf("a service that cannot run %+v", s)
	}
	if s := r.Services[1]; s.Actions[0].Verb != console.ServiceStart || !s.Actions[0].Enabled {
		t.Fatalf("a stopped service %+v", s)
	}

	r = a.result(a.command(Command{Command: "services", Targets: []string{"b1"}, Context: ctx, Args: Args{Service: "worker", Verb: console.ServiceStart}}))
	if r.Status != StatusDone || r.Note != "started worker" || !slices.Contains(core.called(), "start b1 worker") {
		t.Fatalf("start %+v, calls %q", r, core.called())
	}
	for _, args := range []Args{{Service: "broken", Verb: console.ServiceStart}, {Service: "gone", Verb: console.ServiceStop}} {
		if r := a.result(a.command(Command{Command: "services", Targets: []string{"b1"}, Context: ctx, Args: args})); r.Status != StatusFailed || !strings.Contains(r.Why, args.Service) {
			t.Errorf("%+v: %+v", args, r)
		}
	}
	if slices.Contains(core.called(), "start b1 broken") {
		t.Fatal("a service that cannot run was started")
	}

	core.mu.Lock()
	core.services = nil
	core.mu.Unlock()
	if r := a.result(a.command(Command{Command: "services", Targets: []string{"b1"}, Context: ctx})); r.Status != StatusEnter || len(r.Services) != 0 || r.Note != console.NoServices {
		t.Fatalf("a box with no services: %+v", r)
	}
}

// The harnesses page is the core's: its harnesses, of the server the app
// names, each with its actions as the core offers them; each action done as
// the core says, answered with the harnesses read again; a reset asked
// first; a file edited through edit and saved; a variant through its form.
func TestHarnessesAreTheCores(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.harnesses = []console.Harness{
		{ID: "hc_claude", Name: "Claude", Slug: "claude", Default: true, BuiltIn: true, Updated: time.Now().Add(-2 * time.Hour),
			Files: []console.HarnessFile{{Path: ".claude.json", Content: "{}", Source: console.HarnessFileSaved}}},
		{ID: "hc_codex", Name: "Codex", Slug: "codex", Secrets: []console.HarnessSecret{{Name: "OPENAI_API_KEY"}}},
	}
	core.harnessSecrets = []console.HarnessSecret{{Name: "ANTHROPIC_API_KEY", Declared: true, SecretID: "sec_1"}}
	a := dial(t, core)
	a.hello()

	r := a.result(a.command(Command{Command: "harnesses", Context: Context{Tree: true}, Args: Args{Server: "work"}}))
	if r.Status != StatusEnter || len(r.Harnesses) != 2 {
		t.Fatalf("harnesses %+v", r)
	}
	if !slices.Contains(core.called(), "harnesses@work") {
		t.Fatalf("calls %q, want the harnesses of the server the app named", core.called())
	}
	claude, codex := r.Harnesses[0], r.Harnesses[1]
	if claude.Name != "Claude" || !claude.Default || !claude.BuiltIn || claude.Age != "2h ago" || len(claude.Files) != 1 || claude.Files[0].Size != 2 ||
		!slices.Equal(claude.Files[0].Notes, []string{"saved from a discobox"}) || codex.Secrets != 1 {
		t.Fatalf("rows %+v / %+v", claude, codex)
	}
	enabled := map[console.HarnessAction]bool{}
	for _, o := range claude.Actions {
		enabled[o.Action] = o.Enabled
	}
	if len(claude.Actions) != 5 || enabled[console.HarnessActionDefault] || !enabled[console.HarnessActionFiles] {
		t.Fatalf("actions %+v, want the core's, default refused on the default", claude.Actions)
	}

	act := func(h string, action console.HarnessAction, path string) uint64 {
		return a.command(Command{Command: "harnesses", Context: Context{Tree: true}, Args: Args{Harness: h, Action: action, Path: path}})
	}
	// A refused action says why in the core's words, and does nothing.
	if r := a.result(act("hc_claude", console.HarnessActionDefault, "")); r.Status != StatusFailed || !strings.Contains(r.Why, "already the default") {
		t.Fatalf("default on the default: %+v", r)
	}
	if r := a.result(act("hc_codex", console.HarnessActionDefault, "")); r.Status != StatusDone || r.Note != "Codex is now the default" || r.Harness != "hc_codex" || !r.Harnesses[1].Default {
		t.Fatalf("default: %+v", r)
	}
	if r := a.result(act("hc_claude", console.HarnessActionConfig, "")); r.Status != StatusDone || len(r.Card) == 0 || r.Card[0].Label != "Run as" && r.Card[0].Label != "Role" {
		t.Fatalf("config: %+v", r)
	}

	// A reset asks first; no resets nothing.
	for _, answer := range []string{console.AnswerNo, console.AnswerYes} {
		id := act("hc_codex", console.HarnessActionReset, "")
		q := a.expect(TypeQuestion).Question
		if q.Command != id || !strings.Contains(q.Body, "Reset Codex") || len(q.Choices) != 0 {
			t.Fatalf("reset question %+v", q)
		}
		a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: answer}})
		r := a.result(id)
		reset := slices.Contains(core.called(), "harness reset hc_codex")
		if answer == console.AnswerNo && (r.Status != StatusCanceled || reset) || answer == console.AnswerYes && (r.Status != StatusDone || !reset) {
			t.Fatalf("%s: %+v, reset %v", answer, r, reset)
		}
	}

	// A file is handed to the app to edit; what is saved there is read back.
	id := act("hc_claude", console.HarnessActionFiles, ".claude.json")
	e := a.expect(TypeEdit).Edit
	if e.Command != id || e.Title != "Editing .claude.json" || e.Path == "" {
		t.Fatalf("edit %+v", e)
	}
	if err := os.WriteFile(e.Path, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.send(Message{Type: TypeSaved, Saved: &Saved{Edit: e.ID}})
	if r := a.result(id); r.Status != StatusDone || r.Note != "updated .claude.json" || len(r.Harnesses) != 2 {
		t.Fatalf("edit: %+v", r)
	}
	// One given up saves nothing, and is canceled.
	id = act("hc_claude", console.HarnessActionFiles, ".claude.json")
	e = a.expect(TypeEdit).Edit
	a.send(Message{Type: TypeSaved, Saved: &Saved{Edit: e.ID, Canceled: true}})
	if r := a.result(id); r.Status != StatusCanceled {
		t.Fatalf("an edit given up: %+v", r)
	}
	// A file the harness does not carry is refused before anything opens.
	if r := a.result(act("hc_claude", console.HarnessActionFiles, "nope")); r.Status != StatusFailed {
		t.Fatalf("a file not carried: %+v", r)
	}

	// A variant is the core's form.
	id = act("hc_claude", console.HarnessActionVariant, "")
	f := a.expect(TypeForm).Form
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: f.ID, Answers: map[string]string{"name": "Work", "slug": "work"}}})
	if r := a.result(id); r.Status != StatusDone || r.Harness != "hc_work" || len(r.Harnesses) != 3 {
		t.Fatalf("variant: %+v", r)
	}
	if !slices.Contains(core.called(), "variant hc_claude Work work") {
		t.Fatalf("calls %q", core.called())
	}
}

// A box's harness changes are read for its review, with what the core
// offers to do with them; a save or discard names the candidate reviewed.
func TestHarnessChangesReview(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.changes = console.HarnessChanges{
		CandidateID: "cand_1", State: "pending", Revision: 3,
		Files:       []console.HarnessChangedFile{{Path: "settings.json", Action: "update", Diff: "-a\n+b\n"}},
		Credentials: []string{"claude login"},
	}
	a := dial(t, core)
	a.hello()
	r := a.result(a.command(Command{Command: "review config", Targets: []string{"b1"}}))
	c := r.Changes
	if r.Status != StatusEnter || r.Box != "b1" || c == nil || c.Candidate != "cand_1" || c.Summary != "State: pending · revision 3" ||
		len(c.Files) != 1 || c.Files[0].Diff != "-a\n+b\n" || len(c.Offers) != 2 || !c.Offers[0].Enabled || !c.Offers[1].Enabled {
		t.Fatalf("review %+v / %+v", r, c)
	}
	for _, change := range []console.HarnessChange{console.HarnessChangeSave, console.HarnessChangeDiscard} {
		r := a.result(a.command(Command{Command: "review config", Targets: []string{"b1"}, Args: Args{Change: change, Candidate: "cand_1"}}))
		if r.Status != StatusDone || !strings.HasPrefix(r.Note, "harness changes: ") {
			t.Fatalf("%s: %+v", change, r)
		}
	}
	if !slices.Contains(core.called(), "save changes b1 cand_1") || !slices.Contains(core.called(), "discard changes b1 cand_1") {
		t.Fatalf("calls %q", core.called())
	}
	if r := a.result(a.command(Command{Command: "review config", Targets: []string{"b1"}, Args: Args{Change: console.HarnessChangeSave}})); r.Status != StatusFailed {
		t.Fatalf("a save with no candidate: %+v", r)
	}
}

// A box's tools are the core's: listed as the picker lists them, each with
// why it cannot run, running when its session is live, its files and the
// box's addresses; a tool run as the core offers it — one on this machine
// handed the box, any other's session made on the command's stream as its
// tab — and a tool's file edited through edit and saved.
func TestToolsAreTheCores(t *testing.T) {
	core := newFakeCore("home", running("b1"), console.Sandbox{ID: "b2", Name: "b2", State: console.StateStopped})
	core.tools = []console.Tool{
		{ID: "diff", Label: "Diff", Detail: "a diff viewer"},
		{ID: "fresh", Label: "Fresh", Detail: "an editor", Files: []console.ToolFile{{Tool: "fresh", Name: "config.json", Home: ".config/fresh/config.json", Default: "{}"}}},
		{ID: "fresh", Label: "Fresh", Problem: "refused: shadows the CLI's"},
		{ID: "ide", Label: "IDE", Host: true},
		{ID: "broken", Label: "Broken", Problem: "its command is empty"},
	}
	core.addresses = console.Addresses{SSH: "ssh b1.discobox", Git: "ssh://b1.discobox/work"}
	core.execs = []console.Exec{{ID: "tool-diff", Tool: "diff", Command: []string{"lazygit"}, Live: true, Tty: true}}
	a := dial(t, core)
	a.hello()
	ctx := Context{Workspace: "b1", HasWorkspace: true, WorkspaceShown: true, PaneFocused: true, Tree: true}
	tools := func(args Args) uint64 {
		return a.command(Command{Command: "tools", Targets: []string{"b1"}, Context: ctx, Args: args})
	}

	r := a.result(tools(Args{}))
	var got []string
	for _, tl := range r.Tools {
		got = append(got, fmt.Sprintf("%s:%s:%v:%v:%s:%d", tl.ID, tl.Detail, tl.Host, tl.Enabled, tl.Why, len(tl.Files)))
	}
	want := []string{"diff:a diff viewer · running:false:true::0", "fresh:an editor:false:true::1", "ide::true:true::0", "broken::false:false:its command is empty:0"}
	if r.Status != StatusEnter || r.Box != "b1" || !reflect.DeepEqual(got, want) {
		t.Fatalf("tools %+v: %q, want %q", r, got, want)
	}
	if f := r.Tools[1].Files[0]; f.Name != "config.json" || f.Detail != "→ ~/.config/fresh/config.json  ·  /config/discobox/tools/fresh/config.json" {
		t.Fatalf("a tool's file %+v", f)
	}
	if !reflect.DeepEqual(r.Addresses, []Address{{Label: "ssh", Value: "ssh b1.discobox"}, {Label: "git url", Value: "ssh://b1.discobox/work"}}) {
		t.Fatalf("addresses %+v", r.Addresses)
	}

	// One that runs on this machine is handed the box; the stream the app
	// opened for a session is let go.
	if r := a.result(tools(Args{Tool: "ide", Stream: 5})); r.Status != StatusDone || r.Note != "opened b1 in IDE" || r.Session != nil {
		t.Fatalf("a host tool: %+v", r)
	}
	if !slices.Contains(core.called(), "host b1 ide") {
		t.Fatalf("calls %q", core.called())
	}
	core.hostErr = errors.New("no IDE found")
	if r := a.result(tools(Args{Tool: "ide"})); r.Status != StatusFailed || r.Why != "ide: no IDE found" {
		t.Fatalf("a host tool failing: %+v", r)
	}

	// One the box is running is its live session, opened, never a second.
	r = a.result(tools(Args{Tool: "diff", Stream: 9, Cols: 100, Rows: 30}))
	if r.Status != StatusDone || r.Session == nil || r.Session.Exec != "tool-diff" || r.Session.Tool != "diff" {
		t.Fatalf("a running tool: %+v", r)
	}
	if slices.ContainsFunc(core.called(), func(c string) bool { return strings.HasPrefix(c, "tool b1 diff") }) || !slices.Contains(core.called(), "open b1 tool-diff 100x30") {
		t.Fatalf("calls %q, want the running session opened", core.called())
	}

	// Any other's session is made on the stream, as its tab.
	r = a.result(tools(Args{Tool: "fresh", Stream: 6, Cols: 120, Rows: 40}))
	if r.Status != StatusDone || r.Session == nil || r.Session.Kind != TabTool || r.Session.Tool != "fresh" || r.Session.Title != "Fresh" || r.Session.Exec != "tool-fresh" {
		t.Fatalf("a tool's session: %+v", r)
	}
	if !slices.Contains(core.called(), "tool b1 fresh 120x40") {
		t.Fatalf("calls %q", core.called())
	}
	// A declaration that cannot run, or none, is refused before anything is
	// made, and its stream fails.
	for _, args := range []Args{{Tool: "broken", Stream: 7}, {Tool: "gone", Stream: 8}} {
		id := tools(args)
		if st := a.expect(TypeStream).Stream; st.Stream != args.Stream || st.State != StreamFailed {
			t.Fatalf("%s's stream: %+v", args.Tool, st)
		}
		if r := a.result(id); r.Status != StatusFailed || !strings.Contains(r.Why, args.Tool) && !strings.Contains(r.Why, "Broken") {
			t.Fatalf("%s: %+v", args.Tool, r)
		}
	}
	if slices.ContainsFunc(core.called(), func(c string) bool { return strings.HasPrefix(c, "tool b1 broken") }) {
		t.Fatal("a tool that cannot run was made")
	}

	// A file is handed to the app to edit; what is saved there is read back.
	id := tools(Args{Tool: "fresh", File: "config.json"})
	e := a.expect(TypeEdit).Edit
	if e.Command != id || e.Title != "Editing config.json" || !strings.HasSuffix(e.Path, ".json") {
		t.Fatalf("edit %+v", e)
	}
	if err := os.WriteFile(e.Path, []byte(`{"theme":"mine"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a.send(Message{Type: TypeSaved, Saved: &Saved{Edit: e.ID}})
	if r := a.result(id); r.Status != StatusDone || r.Note != "config.json saved — the next discobox to open fresh gets it" {
		t.Fatalf("edit: %+v", r)
	}
	id = tools(Args{Tool: "fresh", File: "config.json"})
	e = a.expect(TypeEdit).Edit
	a.send(Message{Type: TypeSaved, Saved: &Saved{Edit: e.ID, Canceled: true}})
	if r := a.result(id); r.Status != StatusCanceled {
		t.Fatalf("an edit given up: %+v", r)
	}
	for _, args := range []Args{{Tool: "fresh", File: "nope"}, {Tool: "diff", File: "x"}} {
		if r := a.result(tools(args)); r.Status != StatusFailed {
			t.Fatalf("%+v: %+v", args, r)
		}
	}

	// A box that is not up has its tools listed, none runnable, and no
	// addresses to copy.
	r = a.result(a.command(Command{Command: "tools", Targets: []string{"b2"}, Context: Context{Tree: true, ListHasKeys: true}}))
	if r.Status != StatusEnter || r.Tools[0].Enabled || r.Tools[0].Why == "" || r.Addresses[0].Value != "" || r.Addresses[0].Why == "" {
		t.Fatalf("a stopped box's tools: %+v", r)
	}
}

// The app is handed a copy of a file to edit, never the file: what was saved
// there is written back once it is done with, and nothing once it is given
// up, so the app's Cancel keeps the file as it was even for a tool's file,
// which is edited in place. The copy is under a text format's extension,
// since the app opens a file with the handler for its type: a name a box's
// declaration chose (run.desktop) is a .txt copy.
func TestAnEditIsHandedOverAsACopy(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.toolDir = t.TempDir()
	core.tools = []console.Tool{{ID: "launcher", Label: "launcher", Files: []console.ToolFile{
		{Tool: "launcher", Name: "run.desktop", Default: "before"},
		{Tool: "launcher", Name: "config.json", Default: "before"},
	}}}
	a := dial(t, core)
	a.hello()
	for _, c := range []struct{ name, ext string }{{"run.desktop", ".txt"}, {"config.json", ".json"}} {
		file := filepath.Join(core.toolDir, c.name)
		edit := func(answer Saved, write string) Result {
			id := a.command(Command{Command: "tools", Targets: []string{"b1"}, Args: Args{Tool: "launcher", File: c.name}})
			e := a.expect(TypeEdit).Edit
			if e.Path == file || !strings.HasSuffix(e.Path, c.ext) {
				t.Fatalf("%s: handed %q, want a %s copy", c.name, e.Path, c.ext)
			}
			if got, _ := os.ReadFile(e.Path); string(got) != "before" {
				t.Fatalf("%s: the copy holds %q", c.name, got)
			}
			if err := os.WriteFile(e.Path, []byte(write), 0o600); err != nil {
				t.Fatal(err)
			}
			answer.Edit = e.ID
			a.send(Message{Type: TypeSaved, Saved: &answer})
			r := a.result(id)
			if _, err := os.Stat(e.Path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: the copy %s was left behind: %v", c.name, e.Path, err)
			}
			return r
		}
		if r := edit(Saved{Canceled: true}, "never saved"); r.Status != StatusCanceled {
			t.Fatalf("%s given up: %+v", c.name, r)
		}
		if got, _ := os.ReadFile(file); string(got) != "before" {
			t.Fatalf("%s: an edit given up left %q in the file", c.name, got)
		}
		if r := edit(Saved{}, "after"); r.Status != StatusDone || !strings.HasPrefix(r.Note, c.name+" saved") {
			t.Fatalf("%s saved: %+v", c.name, r)
		}
		if got, _ := os.ReadFile(file); string(got) != "after" {
			t.Fatalf("%s: saved %q, want what the copy held", c.name, got)
		}
	}
}
