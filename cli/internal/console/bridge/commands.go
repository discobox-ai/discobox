package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// run rules on one command through the core's registry, then carries out
// what it rules: the core's own work here (a verb, a form, a session made or
// ended), and the app's flow — focus, a page, a menu — handed back to it as
// StatusEnter.
func (s *Server) run(ctx context.Context, c Command) Result {
	if _, ok := console.Lookup(c.Command); !ok {
		return failed(fmt.Sprintf("there is no command %q", c.Command))
	}
	targets := make([]console.Sandbox, 0, len(c.Targets))
	for _, key := range c.Targets {
		b, err := s.box(ctx, key)
		if err != nil {
			return failed(err.Error())
		}
		targets = append(targets, b)
	}
	cctx := s.context(ctx, c)
	f := &frontend{s: s, ctx: ctx, c: c, cctx: cctx, targets: targets}
	s.regMu.Lock()
	outcome := console.Dispatch(s.reg, f, cctx, c.Command, targets)
	s.regMu.Unlock()
	return outcome()
}

// context is the core's Context for a command: the app's navigation as it
// sent it, the box whose workspace it holds, and the credential inbox's
// answers for a box, from what waits on a person as last read (inbox).
func (s *Server) context(ctx context.Context, c Command) console.Context {
	// A workspace whose box is no longer listed is none: the window's
	// commands still run, and the box's are refused for want of one.
	var workspace *console.Sandbox
	if c.Context.Workspace != "" {
		if b, err := s.box(ctx, c.Context.Workspace); err == nil {
			workspace = &b
		}
	}
	// What waits on a person is read here, before the registry is held: a
	// rule asks it while the registry rules, and a server slow to answer
	// must not hold up every other command.
	requests, rejections := s.inbox(ctx)
	return s.contextOf(c.Context, workspace, requests, rejections)
}

// cachedContext is the core's Context for where a topic asks from, from
// what the bridge last read alone: the workspace's box from the last listing,
// and what last waited on a person. A topic that waits for the next listing
// never lists, nor reads the servers at all: a box missing from the listing
// would list again, which wakes the topic again, without end.
func (s *Server) cachedContext(x Context) console.Context {
	var workspace *console.Sandbox
	if b := s.cached([]string{x.Workspace}); x.Workspace != "" && len(b) == 1 {
		workspace = &b[0]
	}
	s.mu.Lock()
	read := s.inboxed
	s.mu.Unlock()
	return s.contextOf(x, workspace, read.requests, read.rejections)
}

// contextOf is the core's Context for the app's navigation, the box whose
// workspace it holds, and what waits on a person.
func (s *Server) contextOf(x Context, workspace *console.Sandbox, requests []console.CredentialRequest, rejections []console.SecretRejection) console.Context {
	s.mu.Lock()
	session := s.session
	s.mu.Unlock()
	return console.Context{
		Session: session, HasWorkspace: x.HasWorkspace, WorkspaceShown: x.WorkspaceShown, PaneFocused: x.PaneFocused,
		ListHasKeys: x.ListHasKeys, Tree: x.Tree, TreeShown: x.TreeShown, TreeFits: x.TreeFits, PageShown: x.PageShown,
		Workspace: workspace,
		Waiting:   func(b console.Sandbox) bool { return len(console.RequestsFor(session, b, requests)) > 0 },
		Refused:   func(b console.Sandbox) bool { return len(console.RejectionsFor(session, b, rejections)) > 0 },
	}
}

// outcome is what the frontend does for a command once the registry has
// ruled on it, run after the registry is let go.
type outcome func() Result

func refused(why string) outcome { return func() Result { return failed(why) } }

// frontend is the bridge as the registry's Frontend, for one command.
type frontend struct {
	s       *Server
	ctx     context.Context
	c       Command
	cctx    console.Context
	targets []console.Sandbox
}

var _ console.Frontend[outcome] = (*frontend)(nil)

// enter hands a window command's flow to the app.
func (f *frontend) enter() outcome { return func() Result { return Result{Status: StatusEnter} } }

// enterBox hands the app a flow about one box.
func (f *frontend) enterBox(box console.Sandbox) outcome {
	return func() Result { return Result{Status: StatusEnter, Box: Key(box.Server, box.ID)} }
}

func (f *frontend) Unavailable(why string) outcome { return refused(why) }

func (f *frontend) Cannot(c console.Command, why string) outcome {
	return refused("cannot " + c.Label + ": " + why)
}

// OfferUpgrade asks the core's question for a box whose image is gone, and
// upgrades it on yes.
func (f *frontend) OfferUpgrade(box console.Sandbox) outcome {
	return func() Result {
		q, ok := console.UpgradeOffer(box)
		if !ok {
			return failed(q.Body)
		}
		return f.confirm(q, console.VerbUpgrade, []console.Sandbox{box})
	}
}

// Attach delivers the source a box is still owed, narrating it, and then
// hands the app the box's workspace to open.
func (f *frontend) Attach(box console.Sandbox) outcome {
	return func() Result {
		if err := f.deliver(box); err != nil {
			return failed(err.Error())
		}
		return f.enterBox(box)()
	}
}

// Shell makes a shell in the box, attached to the command's stream.
func (f *frontend) Shell(box console.Sandbox) outcome {
	return func() Result {
		if err := f.deliver(box); err != nil {
			return failed(err.Error())
		}
		return f.session(box, sessionTab(console.InteractShell), console.DataSource.NewShell)
	}
}

// NewTerminal makes another of the harness's terminals in the box whose
// workspace is open.
func (f *frontend) NewTerminal() outcome {
	box := f.cctx.Workspace
	if box == nil {
		return refused("requires a workspace")
	}
	return func() Result {
		return f.session(*box, sessionTab(console.InteractTerminal), console.DataSource.NewTerminal)
	}
}

// sessionTab is the tab a shell or terminal made as asked is (console.SessionTab).
func sessionTab(asked console.Interaction) func(console.Exec) console.Tab {
	return func(ex console.Exec) console.Tab { return console.SessionTab(ex, asked) }
}

// deliver delivers what the box is owed before it is opened (ADR
// 26-09-24-005), narrating it.
func (f *frontend) deliver(box console.Sandbox) error {
	if !box.AwaitsDelivery {
		return nil
	}
	src := f.s.ds.ForServer(box.Server)
	n, finished := f.s.narrate(f.ctx, f.c.ID)
	err := src.DeliverSource(f.ctx, box.ID, n)
	finished()
	return err
}

// session makes a session in the box with start, attached to the command's
// stream as the tab tab says it is.
func (f *frontend) session(box console.Sandbox, tab func(console.Exec) console.Tab, start func(console.DataSource, context.Context, string, int, int) (console.Exec, console.Terminal, error)) Result {
	stream := f.c.Args.Stream
	if stream == 0 {
		return failed("no stream to attach the session to")
	}
	if f.s.streams.get(stream) == nil {
		return failed(fmt.Sprintf("stream %d has ended", stream))
	}
	cols, rows := size(f.c.Args.Cols, f.c.Args.Rows)
	ex, term, err := start(f.s.ds.ForServer(box.Server), f.ctx, box.ID, cols, rows)
	if err != nil {
		// Nothing was made: the tab is what was asked for.
		f.s.streams.fail(stream, err, tab(console.Exec{}))
		return failed(err.Error())
	}
	made := tab(ex)
	wired := wireTab(made, ex)
	var ended func()
	if made.Files != 0 {
		ended = f.s.endFiles(f.s.ds.ForServer(box.Server), box.ID, ex.ID)
	}
	if !f.s.streams.attach(stream, term, made, nil, ended) {
		if ended != nil {
			// Nothing else would ever end it.
			ended()
		}
		// Closed while it was made: the session goes on in the box, and the
		// app, which closed it, is told what it was.
		return Result{Status: StatusDone, Box: Key(box.Server, box.ID), Session: &wired, Note: "the pane closed before the session was made"}
	}
	return Result{Status: StatusDone, Box: Key(box.Server, box.ID), Session: &wired}
}

// EndSession ends the session named by the command, in the box whose
// workspace is open, and closes its stream.
func (f *frontend) EndSession() outcome {
	box := f.cctx.Workspace
	if box == nil {
		return refused("requires a workspace")
	}
	exec := f.c.Args.Exec
	switch exec {
	case "":
		return refused("no session to end")
	case console.ExecPrimary:
		return refused("the primary session ends with its box; stop the box instead")
	}
	return func() Result {
		// Ended in the box first: a session that could not be ended goes on,
		// and so does the pane showing it.
		if err := f.s.ds.ForServer(box.Server).EndExec(f.ctx, box.ID, exec); err != nil {
			return failed(err.Error())
		}
		if f.c.Args.Stream != 0 {
			f.s.streams.close(f.c.Args.Stream)
		}
		return Result{Status: StatusDone, Box: Key(box.Server, box.ID)}
	}
}

// Repaint asks the stream named by the command for its screen again.
func (f *frontend) Repaint() outcome {
	stream := f.c.Args.Stream
	if stream == 0 {
		return refused("no pane to repaint")
	}
	return func() Result {
		if !f.s.streams.repaint(stream) {
			return failed("that pane has no stream")
		}
		return Result{Status: StatusDone}
	}
}

// Rename asks the core's rename form and renames the box.
func (f *frontend) Rename(box console.Sandbox) outcome {
	return func() Result {
		r, ok, err := fill(f.ctx, f.s, f.c.ID, sheet{title: "Rename"}, console.RenameForm(box, box.Title()))
		switch {
		case err != nil:
			return failed(err.Error())
		case !ok:
			return Result{Status: StatusCanceled}
		case r.Unchanged:
			return Result{Status: StatusDone, Note: "name unchanged"}
		}
		if err := f.s.ds.ForServer(box.Server).Rename(f.ctx, box.ID, r.Name); err != nil {
			return failed("rename: " + err.Error())
		}
		f.s.relist()
		if r.Name == "" {
			return Result{Status: StatusDone, Note: "name cleared"}
		}
		return Result{Status: StatusDone, Note: "renamed to " + r.Name}
	}
}

// Settings reads the box's settings, asks the core's settings form, and
// saves what changed.
func (f *frontend) Settings(box console.Sandbox) outcome {
	return func() Result {
		src := f.s.ds.ForServer(box.Server)
		was, err := src.BoxSettings(f.ctx, box.ID)
		if err != nil {
			return failed(err.Error())
		}
		// The harnesses only fill the harness row; a listing that fails
		// leaves it on the box's own.
		harnesses, _ := src.Harnesses(f.ctx, box.Server)
		update, ok, err := fill(f.ctx, f.s, f.c.ID, sheet{title: "Settings · " + box.Title()}, console.SettingsForm(box, was, harnesses))
		switch {
		case err != nil:
			return failed(err.Error())
		case !ok:
			return Result{Status: StatusCanceled}
		case update.Empty():
			return Result{Status: StatusDone, Note: "settings unchanged"}
		}
		if err := src.UpdateBox(f.ctx, box.ID, update); err != nil {
			return failed("settings: " + err.Error())
		}
		f.s.relist()
		return Result{Status: StatusDone, Note: "settings saved"}
	}
}

// Do sends a verb to every target, narrating each.
func (f *frontend) Do(verb console.Verb, targets []console.Sandbox) outcome {
	return func() Result { return f.do(verb, targets) }
}

// Confirm asks the question the core puts before a verb, and sends it only
// on yes.
func (f *frontend) Confirm(q console.Question, verb console.Verb, targets []console.Sandbox) outcome {
	return func() Result { return f.confirm(q, verb, targets) }
}

func (f *frontend) confirm(q console.Question, verb console.Verb, targets []console.Sandbox) Result {
	key, err := f.s.ask(f.ctx, f.c.ID, q)
	if r, over := f.asked(err); over {
		return r
	}
	if key != console.AnswerYes {
		return Result{Status: StatusCanceled}
	}
	return f.do(verb, targets)
}

func (f *frontend) do(verb console.Verb, targets []console.Sandbox) Result {
	defer f.s.relist()
	var notes, failures []string
	for _, b := range targets {
		n, finished := f.s.narrate(f.ctx, f.c.ID)
		note, err := f.s.ds.ForServer(b.Server).Do(f.ctx, verb, b.ID, n)
		finished()
		name := b.Title()
		switch {
		case err != nil:
			failures = append(failures, fmt.Sprintf("%s %s: %v", verb, name, err))
		case note != "":
			notes = append(notes, note)
		default:
			notes = append(notes, fmt.Sprintf("%s: %s", verb, name))
		}
	}
	if len(failures) > 0 {
		return Result{Status: StatusFailed, Why: strings.Join(failures, "; "), Note: strings.Join(notes, "; ")}
	}
	return Result{Status: StatusDone, Note: strings.Join(notes, "; ")}
}

// Refresh reads the listing again now, for every listing subscription.
func (f *frontend) Refresh() outcome {
	return func() Result {
		f.s.relist()
		return Result{Status: StatusDone}
	}
}

// Palette opens the app's palette, counted as the palette's use: its flow
// counts it (the TUI's as it opens), and what the palette finds is the
// palette topic's.
func (f *frontend) Palette() outcome {
	f.s.reg.Use("palette")
	return f.enter()
}

// Actions hands the app a box's action menu, as the core rules on it for the
// targets the app asked with.
func (f *frontend) Actions() outcome {
	targets := f.actionTargets()
	if len(targets) == 0 {
		return refused("nothing selected")
	}
	entries := console.Lifecycle(targets)
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, wireEntry(e, 0))
	}
	r := Result{Status: StatusEnter, Box: Key(targets[0].Server, targets[0].ID), Entries: out}
	return func() Result { return r }
}

// Services is the box's services: read as the menu opens, for the app's
// services menu on the box's workspace — counted as the command's use, which
// its flow is — or, with a service and a verb in the command's args, that
// verb run on that service by the core (console.RunServiceVerb). What the
// service does next is the sessions topic's to say.
func (f *frontend) Services(box console.Sandbox) outcome {
	if f.c.Args.Service == "" {
		f.s.reg.Use("services")
	}
	return func() Result {
		src := f.s.ds.ForServer(box.Server)
		if f.c.Args.Service != "" {
			note, err := console.RunServiceVerb(f.ctx, src, box.ID, f.c.Args.Service, f.c.Args.Verb)
			if err != nil {
				return failed(err.Error())
			}
			return Result{Status: StatusDone, Box: Key(box.Server, box.ID), Note: note}
		}
		services, err := src.Services(f.ctx, box.ID)
		if err != nil {
			return failed("services: " + err.Error())
		}
		r := Result{Status: StatusEnter, Box: Key(box.Server, box.ID), Services: make([]Service, 0, len(services))}
		for _, svc := range services {
			r.Services = append(r.Services, wireService(svc))
		}
		if len(services) == 0 {
			r.Note = console.NoServices
		}
		return r
	}
}

// Harnesses is the harnesses page: the harnesses of the server the app names
// (the list's; the primary for none), read as the page opens — counted as
// the command's use, which its flow is — or, with a harness and an action in
// the args, that action done to it as the core offers it: the core reads the
// harness again and refuses in the offer's words (console.OfferedHarness,
// RunHarnessVerb, EditHarness). Every action but config is answered with the
// harnesses read again, since each changes what the rows say.
func (f *frontend) Harnesses() outcome {
	a := f.c.Args
	if a.Harness == "" {
		f.s.reg.Use("harnesses")
		return func() Result { return f.harnesses(a.Server, StatusEnter, "", "") }
	}
	return func() Result {
		h, err := console.OfferedHarness(f.ctx, f.s.ds, a.Server, a.Harness, a.Action)
		if err != nil {
			return failed(err.Error())
		}
		var note string
		switch a.Action {
		case console.HarnessActionConfig:
			secrets, err := f.s.ds.HarnessSecrets(f.ctx, a.Server, h.ID)
			if err != nil {
				return failed("cannot read " + h.DisplayName() + ": " + err.Error())
			}
			return Result{Status: StatusDone, Harness: h.ID, Card: wireCard(console.HarnessCard(h, secrets))}
		case console.HarnessActionDefault:
			note, err = console.RunHarnessVerb(f.ctx, f.s.ds, a.Server, h.ID, console.HarnessSetDefault)
		case console.HarnessActionReset:
			key, askErr := f.s.ask(f.ctx, f.c.ID, h.ResetQuestion())
			if r, over := f.asked(askErr); over {
				return r
			}
			verb, ok := h.ResetVerb(key)
			if !ok {
				return Result{Status: StatusCanceled}
			}
			note, err = console.RunHarnessVerb(f.ctx, f.s.ds, a.Server, h.ID, verb)
		case console.HarnessActionFiles:
			n, finished := f.s.narrate(f.ctx, f.c.ID)
			note, err = console.EditHarness(f.ctx, f.s.ds, a.Server, h.ID, a.Path, n)
			finished()
		case console.HarnessActionVariant:
			variant, ok, fillErr := fill(f.ctx, f.s, f.c.ID, sheet{title: "Create variant of " + h.DisplayName()}, console.HarnessVariantForm(h))
			switch {
			case fillErr != nil:
				return failed(fillErr.Error())
			case !ok:
				return Result{Status: StatusCanceled}
			}
			created, err := f.s.ds.CreateHarnessVariant(f.ctx, a.Server, variant)
			if err != nil {
				return failed("variant not created: " + err.Error())
			}
			return f.harnesses(a.Server, StatusDone, created.ID, fmt.Sprintf("created variant %q", created.Name))
		}
		if r, over := f.asked(err); over {
			return r
		}
		return f.harnesses(a.Server, StatusDone, h.ID, note)
	}
}

// asked is what a question's dismissal or failure ends the command with, and
// whether it did: one the app declined, or an edit it gave up, is canceled,
// unless the bridge itself is going.
func (f *frontend) asked(err error) (Result, bool) {
	switch {
	case err == nil:
		return Result{}, false
	case errors.Is(err, context.Canceled) && f.ctx.Err() == nil:
		return Result{Status: StatusCanceled}, true
	}
	return failed(err.Error()), true
}

// harnesses is a result carrying a server's harnesses as read now, about one
// of them. One that cannot be read fails the page it would open, and leaves
// an action that has run saying what it did.
func (f *frontend) harnesses(server string, status Status, about, note string) Result {
	// An action changed the harnesses: the run options' harness row, and the
	// composer's line, follow them as the TUI's follow its listing.
	if status == StatusDone {
		f.s.readOptions(f.ctx)
	}
	listed, err := f.s.ds.Harnesses(f.ctx, server)
	if err != nil {
		why := "cannot list the harnesses: " + err.Error()
		if status == StatusEnter {
			return failed(why)
		}
		return Result{Status: status, Harness: about, Note: note + " — " + why}
	}
	now := time.Now()
	out := Result{Status: status, Harness: about, Note: note, Harnesses: make([]Harness, 0, len(listed))}
	for _, h := range listed {
		out.Harnesses = append(out.Harnesses, wireHarness(h, now))
	}
	return out
}

// ReviewConfig is a box's harness changes: read for the app's review as it
// opens — counted as the command's use, which its flow is — or, with a change
// and the candidate the review showed in the args, that candidate saved or
// discarded by the core (console.RunHarnessChange), never a newer one the box
// captured meanwhile.
func (f *frontend) ReviewConfig(box console.Sandbox) outcome {
	a := f.c.Args
	key := Key(box.Server, box.ID)
	src := f.s.ds.ForServer(box.Server)
	if a.Change != "" {
		return func() Result {
			state, err := console.RunHarnessChange(f.ctx, src, box.ID, a.Change, a.Candidate)
			if err != nil {
				return failed("harness changes: " + err.Error())
			}
			// The candidate is answered, but the listing goes on naming it
			// until the box next reports: its notice is taken down as a
			// dismissal of it, as the TUI's band is, so a newer one the box
			// captures still raises it.
			f.s.cover(box, console.NoticeHarnessChanges, a.Candidate)
			f.s.relist()
			return Result{Status: StatusDone, Box: key, Note: "harness changes: " + state}
		}
	}
	f.s.reg.Use("review config")
	return func() Result {
		changes, err := src.HarnessChanges(f.ctx, box.ID)
		if err != nil {
			return failed("harness changes: " + err.Error())
		}
		c := wireChanges(changes)
		return Result{Status: StatusEnter, Box: key, Changes: &c}
	}
}

// Tools is a box's tools: read as the tools menu opens — counted as the
// command's use, which its flow is — with the box's addresses beside them;
// or, with a tool in the args, that tool run as the core offers it
// (console.OfferedTool): one that runs on this machine handed the box
// (console.RunHostTool), any other's session made on the command's stream,
// as its tab (console.ToolTab) — its live session when it is running one
// (console.OpenTool). With a file too, that file of the tool is
// edited through edit and saved (console.EditTool).
func (f *frontend) Tools(box console.Sandbox) outcome {
	a := f.c.Args
	key := Key(box.Server, box.ID)
	src := f.s.ds.ForServer(box.Server)
	switch {
	case a.Tool == "":
		f.s.reg.Use("tools")
		return func() Result { return f.tools(box) }
	case a.File != "":
		return func() Result {
			n, finished := f.s.narrate(f.ctx, f.c.ID)
			note, err := console.EditTool(f.ctx, src, box, a.Tool, a.File, n)
			finished()
			if r, over := f.asked(err); over {
				return r
			}
			return Result{Status: StatusDone, Box: key, Note: note}
		}
	}
	return func() Result {
		t, err := console.OfferedTool(f.ctx, src, box, a.Tool)
		if err != nil {
			f.release(a.Stream, err)
			return failed(err.Error())
		}
		if t.Host {
			// Nothing in the box is made for it: a stream the app opened
			// for a session is let go.
			f.release(a.Stream, nil)
			note, err := console.RunHostTool(f.ctx, src, box, t.ID)
			if err != nil {
				return failed(err.Error())
			}
			return Result{Status: StatusDone, Box: key, Note: note}
		}
		tab := func(ex console.Exec) console.Tab { return console.ToolTab(ex, t.ID, t.Label) }
		return f.session(box, tab, func(_ console.DataSource, ctx context.Context, _ string, cols, rows int) (console.Exec, console.Terminal, error) {
			_, ex, term, err := console.OpenTool(ctx, src, box, t.ID, cols, rows)
			return ex, term, err
		})
	}
}

// Audit is a box's audit timeline: its view entered — counted as the
// command's use, which its flow is — whose records the audit topic follows;
// or, with a record in the args, that record read in full, with the
// recordings it offers; with a part too, that recording of it read.
func (f *frontend) Audit(box console.Sandbox) outcome {
	a := f.c.Args
	key := Key(box.Server, box.ID)
	src := f.s.ds.ForServer(box.Server)
	switch {
	case a.Record == "":
		f.s.reg.Use("audit")
		return f.enterBox(box)
	case a.Part == "":
		return func() Result {
			d, err := src.AuditDetail(f.ctx, box.ID, a.Record)
			if err != nil {
				return failed(console.AuditRecordUnreadable(a.Record, err))
			}
			text := AuditText{Record: a.Record, Title: console.AuditRecordTitle, Text: d.Text, Recordings: []AuditRecording{}}
			for _, r := range d.AuditRecordings() {
				text.Recordings = append(text.Recordings, AuditRecording{Part: r.Part, Label: r.Label})
			}
			return Result{Status: StatusDone, Box: key, Audit: &text}
		}
	}
	return func() Result {
		body, err := src.AuditBody(f.ctx, box.ID, a.Record, a.Part)
		if err != nil {
			return failed(console.AuditRecordingUnreadable(a.Part, err))
		}
		return Result{Status: StatusDone, Box: key, Audit: &AuditText{
			Record: a.Record, Part: a.Part, Title: console.AuditRecordingTitle(a.Part), Text: body, Recordings: []AuditRecording{},
		}}
	}
}

// release lets go of a stream the app allocated for a session that is not
// made: one refused fails with why, as a shell's does, and one no session
// was wanted on is forgotten.
func (f *frontend) release(stream uint32, why error) {
	switch {
	case stream == 0:
	case why != nil:
		f.s.streams.fail(stream, why, console.Tab{})
	default:
		f.s.streams.retire(stream)
	}
}

// tools is the tools menu's result: the box's tools, each as it stands for
// the box and running when its session is live there, and its addresses.
// One whose tools cannot be read fails the menu it would open; addresses
// that cannot be read are said on their rows.
func (f *frontend) tools(box console.Sandbox) Result {
	src := f.s.ds.ForServer(box.Server)
	catalog, err := src.Tools(f.ctx, box.ID)
	if err != nil {
		return failed("tools: " + err.Error())
	}
	var running map[string]console.Exec
	if execs, err := src.Execs(f.ctx, box.ID); err == nil {
		running = console.RunningTools(execs)
	}
	r := Result{Status: StatusEnter, Box: Key(box.Server, box.ID), Tools: []Tool{}, Addresses: []Address{}}
	for _, t := range console.PickerTools(catalog) {
		_, live := running[t.ID]
		r.Tools = append(r.Tools, wireTool(t, box, live, src.ToolFilePath))
	}
	if len(r.Tools) == 0 {
		r.Note = "no tools are declared for " + box.Title()
	}
	var addr console.Addresses
	if box.Attachable() {
		addr, err = src.Addresses(f.ctx, box.ID)
	}
	for _, o := range console.AddressOffers(box, addr, err, true) {
		r.Addresses = append(r.Addresses, Address{Label: o.Label, Value: o.Value, Why: o.Why})
	}
	return r
}

// Credentials is the secrets page: the secrets of the server the app names
// (the list's; the primary for none), read as the page opens — counted as the
// command's use, which its flow is — or, with a SecretAction in the args, that
// action done as the TUI's screen does it, every one the call the `discobox
// secret` commands make: a new secret, an edit or a binding through the
// core's form, a delete or an unbind asked first, a secret's binding history.
// The secret, and a binding, are read again for what is asked first
// (console.OfferedSecret, OfferedBinding), and the core's action reads them
// again as it acts. Read reads the page's secrets again, which is no use of
// the command. Every action but history is answered with the secrets read
// again, since each changes what the rows say.
func (f *frontend) Credentials() outcome {
	a := f.c.Args
	if a.SecretAction == "" {
		f.s.reg.Use("credentials")
		return func() Result { return f.secrets(a.Server, StatusEnter, "", "") }
	}
	return func() Result {
		ds := f.s.ds
		switch a.SecretAction {
		case console.SecretActionRead:
			return f.secrets(a.Server, StatusDone, "", "")
		case console.SecretActionAnswer:
			r := f.answerOn(a.Server, a.Request)
			if r.Status != StatusDone {
				return r
			}
			return f.secrets(a.Server, StatusDone, "", r.Note)
		case console.SecretActionNew:
			secret, ok, err := fill(f.ctx, f.s, f.c.ID, sheet{title: "New secret"}, console.NewSecretForm(console.NewSecretAsk{}))
			if r, over := f.filled(ok, err); over {
				return r
			}
			note, err := console.StoreSecret(f.ctx, ds, a.Server, secret)
			if err != nil {
				return failed("cannot store " + secret.Name + ": " + err.Error())
			}
			return f.secrets(a.Server, StatusDone, "", note)
		}
		secret, err := console.OfferedSecret(f.ctx, ds, a.Server, a.Secret)
		if err != nil {
			return failed(err.Error())
		}
		var note string
		switch a.SecretAction {
		case console.SecretActionEdit:
			page, readErr := f.secretsPage(a.Server)
			if readErr != nil {
				return failed(readErr.Error())
			}
			update, ok, fillErr := fill(f.ctx, f.s, f.c.ID, sheet{title: "Edit " + secret.Name, about: page.editAbout(secret)}, console.EditSecretForm(secret))
			if r, over := f.filled(ok, fillErr); over {
				return r
			}
			note, err = console.SaveSecret(f.ctx, ds, a.Server, secret.ID, update)
		case console.SecretActionDelete:
			page, readErr := f.secretsPage(a.Server)
			if readErr != nil {
				return failed(readErr.Error())
			}
			live := len(console.LiveBindings(page.bindings, secret.ID, page.now))
			if r, over := f.yes(console.DeleteSecretQuestion(secret.Name, live)); over {
				return r
			}
			note, err = console.DeleteSecret(f.ctx, ds, a.Server, secret.ID)
		case console.SecretActionBind:
			page, readErr := f.secretsPage(a.Server)
			if readErr != nil {
				return failed(readErr.Error())
			}
			binding, ok, fillErr := fill(f.ctx, f.s, f.c.ID, sheet{title: "Bind " + secret.Name, about: console.BindingFormNote}, console.BindingForm(secret, page.harnesses, page.boxes))
			if r, over := f.filled(ok, fillErr); over {
				return r
			}
			note, err = console.Bind(f.ctx, ds, a.Server, binding)
		case console.SecretActionUnbind:
			page, readErr := f.secretsPage(a.Server)
			if readErr != nil {
				return failed(readErr.Error())
			}
			b, bindErr := console.OfferedBinding(f.ctx, ds, a.Server, secret.ID, a.Binding, page.now)
			if bindErr != nil {
				return failed(bindErr.Error())
			}
			if r, over := f.yes(console.UnbindQuestion(secret.Name, b, page.target(b))); over {
				return r
			}
			note, err = console.Unbind(f.ctx, ds, a.Server, secret.ID, b.ID)
		case console.SecretActionHistory:
			return f.history(a.Server, secret)
		default:
			return failed(fmt.Sprintf("there is no secret action %q", a.SecretAction))
		}
		if err != nil {
			return failed(err.Error())
		}
		return f.secrets(a.Server, StatusDone, secret.ID, note)
	}
}

// filled is what a form left unanswered ends the command with, and whether
// it did: one dismissed is canceled, one that could not be filled failed.
func (f *frontend) filled(ok bool, err error) (Result, bool) {
	switch {
	case err != nil:
		return failed(err.Error()), true
	case !ok:
		return Result{Status: StatusCanceled}, true
	}
	return Result{}, false
}

// yes asks a yes or no and is what anything but yes ends the command with,
// and whether it did: no, or dismissed, is canceled.
func (f *frontend) yes(q console.Question) (Result, bool) {
	key, err := f.s.ask(f.ctx, f.c.ID, q)
	if r, over := f.asked(err); over {
		return r, true
	}
	if key != console.AnswerYes {
		return Result{Status: StatusCanceled}, true
	}
	return Result{}, false
}

// secretsPage reads what the secrets page says about a server's secrets
// beside them: the project's bindings, which of that server's secrets an
// upstream refuses (from what last waited on a person), and the harnesses
// and listed boxes on that server a binding's target is named from.
func (f *frontend) secretsPage(server string) (secretsPage, error) {
	bindings, err := f.s.ds.Bindings(f.ctx, server, console.BindingFilter{})
	if err != nil {
		return secretsPage{}, fmt.Errorf("cannot read the bindings: %w", err)
	}
	// The harnesses only name a binding's target; one that cannot be read
	// is named by its ID.
	harnesses, _ := f.s.ds.Harnesses(f.ctx, server)
	_, rejections := f.s.inbox(f.ctx)
	session, boxes := f.s.listed()
	return secretsPage{
		bindings: bindings, harnesses: harnesses, now: time.Now(),
		refused: console.RefusedSecrets(session, server, rejections),
		boxes:   console.BindableBoxes(session, server, boxes),
	}, nil
}

// secrets is a result carrying a server's secrets as read now, about one of
// them, with the page's words. One that cannot be read fails, whatever came
// before: the app keeps the rows it has rather than take none for a project
// with no secrets, and an action that has run says what it did in the why.
func (f *frontend) secrets(server string, status Status, about, note string) Result {
	cannot := func(err error) Result {
		if note == "" {
			return failed(err.Error())
		}
		return failed(note + " — " + err.Error())
	}
	listed, err := f.s.ds.Secrets(f.ctx, server)
	if err != nil {
		return cannot(fmt.Errorf("cannot read the secrets: %w", err))
	}
	page, err := f.secretsPage(server)
	if err != nil {
		return cannot(err)
	}
	out := Result{Status: status, Secret: about, Note: note, Secrets: make([]Secret, 0, len(listed)), SecretWords: secretWords()}
	for _, s := range listed {
		out.Secrets = append(out.Secrets, page.wireSecret(s))
	}
	if out.Requests, err = f.pageRequests(server, page.now); err != nil {
		return cannot(err)
	}
	out.SecretWords.Waiting = console.RequestsWaiting(len(out.Requests))
	return out
}

// history is a secret's binding history, each decision read in full, or what
// a secret with none says.
func (f *frontend) history(server string, secret console.Secret) Result {
	entries, err := f.s.ds.BindingHistory(f.ctx, server, secret.ID)
	if err != nil {
		return failed("cannot read binding history: " + err.Error())
	}
	if len(entries) == 0 {
		return Result{Status: StatusDone, Secret: secret.ID, Note: console.NoBindingHistory}
	}
	page, err := f.secretsPage(server)
	if err != nil {
		return failed(err.Error())
	}
	out := Result{Status: StatusDone, Secret: secret.ID, History: make([]Decision, 0, len(entries)), SecretWords: secretWords()}
	for _, e := range entries {
		out.History = append(out.History, page.wireDecision(e))
	}
	return out
}

// actionTargets are the boxes the app named for a window's command that
// lists commands for them: the registry ignores a window command's targets,
// and the action menu is about the ones the app has picked.
func (f *frontend) actionTargets() []console.Sandbox {
	out := make([]console.Sandbox, 0, len(f.c.Targets))
	for _, key := range f.c.Targets {
		f.s.mu.Lock()
		b, ok := f.s.boxes[key]
		f.s.mu.Unlock()
		if ok {
			out = append(out, b)
		}
	}
	return out
}

// The flows that are the app's: the core has ruled the command can run, and
// the app enters it.

func (f *frontend) Quit() outcome           { return f.enter() }
func (f *frontend) Help() outcome           { return f.enter() }
func (f *frontend) Filters() outcome        { return f.enter() }
func (f *frontend) AllBoxes() outcome       { return f.enter() }
func (f *frontend) ToggleArchived() outcome { return f.enter() }
func (f *frontend) Maximize() outcome       { return f.enter() }
func (f *frontend) Detach() outcome         { return f.enter() }
func (f *frontend) ToggleMouse() outcome    { return f.enter() }
func (f *frontend) NextPane() outcome       { return f.enter() }
func (f *frontend) PreviousPane() outcome   { return f.enter() }
func (f *frontend) NextBox() outcome        { return f.enter() }
func (f *frontend) PreviousBox() outcome    { return f.enter() }
func (f *frontend) ToggleSidebar() outcome  { return f.enter() }

// Issue is the box's issue: its tab handed to the app, named (Result.Work) —
// the issue topic reads it — or, with an act in the args, that act done to
// it (work).
func (f *frontend) Issue(box console.Sandbox) outcome { return f.work(box, false) }

// PullRequest is the same for the box's pull request.
func (f *frontend) PullRequest(box console.Sandbox) outcome { return f.work(box, true) }

// workNumber is the number a box's issue or pull request tag gives it on its
// GitHub repository, or why it has none (console.Sandbox.Work), as the
// registry's entry refuses it.
func (f *frontend) workNumber(box console.Sandbox, pull bool) (int, string) {
	if e, ok := f.s.reg.Entry(f.cctx, f.c.Command, []console.Sandbox{box}); ok && !e.Enabled {
		return 0, e.Why
	}
	number, err := box.Work(pull)
	if err != nil {
		return 0, err.Error()
	}
	return number, ""
}

// work is the issue or pull request command: its tab, counted as the
// command's use, which its flow is; or an act on it, as the person at this
// machine, done by the core (console.PostComment, ChangeIssueState,
// MergeIssue) once the core's question is answered — asked of the issue read
// again here, since the bridge holds no read of its own, and a merge of the
// head the app showed — after which every subscription following it reads it
// again.
func (f *frontend) work(box console.Sandbox, pull bool) outcome {
	number, why := f.workNumber(box, pull)
	if why != "" {
		return refused(why)
	}
	a := f.c.Args
	key := Key(box.Server, box.ID)
	if a.Act == "" {
		f.s.reg.Use(f.c.Command)
		r := Result{Status: StatusEnter, Box: key, Work: &Link{Label: console.WorkLabel(number, pull), URL: console.WorkAddress(box.Repository, number, pull)}}
		return func() Result { return r }
	}
	src := f.s.ds.ForServer(box.Server)
	repository := box.Repository
	return func() Result {
		note, err := f.act(src, repository, number, a)
		if r, over := f.asked(err); over {
			return r
		}
		f.s.reissue(repository, number)
		return Result{Status: StatusDone, Box: key, Note: note}
	}
}

// act does one act to an issue: a comment as written, or a close, a reopen
// or a merge once its question is answered.
func (f *frontend) act(src console.DataSource, repository string, number int, a Args) (string, error) {
	if a.Act == ActComment {
		ctx, cancel := context.WithTimeout(f.ctx, console.IssueTimeout)
		defer cancel()
		return console.PostComment(ctx, src, repository, number, a.Comment)
	}
	read, cancel := context.WithTimeout(f.ctx, console.IssueTimeout)
	issue, err := src.Issue(read, repository, number, true)
	cancel()
	if err != nil {
		return "", fmt.Errorf("could not read #%d: %w", number, err)
	}
	var q console.Question
	head := a.Head
	switch a.Act {
	case ActState:
		q, err = issue.StateQuestion()
	case ActMerge:
		if head == "" && issue.Pull != nil {
			head = issue.Pull.HeadSHA
		}
		q, err = issue.MergeQuestion(head, a.Pushed)
	default:
		return "", fmt.Errorf("there is nothing to %q", a.Act)
	}
	if err != nil {
		return "", err
	}
	answer, err := f.s.ask(f.ctx, f.c.ID, q)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(f.ctx, console.IssueTimeout)
	defer cancel()
	if a.Act == ActState {
		return console.ChangeIssueState(ctx, src, repository, issue, answer)
	}
	return console.MergeIssue(ctx, src, repository, issue, answer, head)
}

// FilesChanged is the box's pull request's files changed: discobox-review in
// the box over the pull request's base and head (DataSource.PullReview),
// counted as the command's use, its session on the command's stream as its
// tab (console.FilesTab). Head and Base are the commits the app showed, so a
// push it has not read never moves what it reviews; without them it is the
// head the pull request is read at now. The session is the tab's own: it is
// ended in the box when its stream is let go (Server.endFiles).
func (f *frontend) FilesChanged(box console.Sandbox) outcome {
	number, why := f.workNumber(box, true)
	if why != "" {
		f.release(f.c.Args.Stream, errors.New(why))
		return refused(why)
	}
	f.s.reg.Use("files")
	a := f.c.Args
	src := f.s.ds.ForServer(box.Server)
	return func() Result {
		ctx, cancel := context.WithTimeout(f.ctx, console.IssueTimeout)
		issue, err := src.Issue(ctx, box.Repository, number, false)
		cancel()
		switch {
		case err != nil:
			err = fmt.Errorf("could not read #%d: %w", number, err)
		case issue.Pull == nil:
			err = fmt.Errorf("#%d is not a pull request", number)
		}
		if err != nil {
			f.release(a.Stream, err)
			return failed(err.Error())
		}
		pull := *issue.Pull
		if a.Head != "" {
			pull.HeadSHA, pull.BaseSHA = a.Head, a.Base
		}
		// Counted from before its start, not from its attach: nothing will
		// ever list it to end it, so a bridge going while it starts waits for
		// it, and one that lands once the command is over is ended here.
		if !f.s.beginFiles() {
			f.release(a.Stream, errors.New("the window is closing"))
			return failed("the window is closing")
		}
		defer f.s.files.Done()
		tab := func(ex console.Exec) console.Tab { return console.FilesTab(ex, number) }
		return f.session(box, tab, func(_ console.DataSource, ctx context.Context, id string, cols, rows int) (console.Exec, console.Terminal, error) {
			// The session's terminal lives on the context it was started
			// on, so the bound cancels only a start still under way: one
			// that has started runs until its stream is let go.
			start, cancel := context.WithCancel(ctx)
			timer := time.AfterFunc(console.ReviewStartTimeout, cancel)
			ex, term, err := src.PullReview(start, id, pull, cols, rows)
			late := !timer.Stop()
			switch {
			case err != nil && late && ctx.Err() == nil:
				cancel()
				return ex, term, console.ErrReviewNotStarted
			case err != nil:
				cancel()
			case ctx.Err() != nil || late:
				_ = term.Close()
				cancel()
				f.s.endFiles(src, id, ex.ID)()
				if ctx.Err() != nil {
					return console.Exec{}, nil, ctx.Err()
				}
				return console.Exec{}, nil, console.ErrReviewNotStarted
			}
			return ex, term, err
		})
	}
}
