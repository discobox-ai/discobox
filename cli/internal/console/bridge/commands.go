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

// enterIfAble hands the app a flow about one box whose other preconditions
// — a tag to open, a request waiting — the registry leaves to the flow: the
// app's flow does not rule on them, so they are refused here, in the words
// the registry's entry gives.
func (f *frontend) enterIfAble(box console.Sandbox) outcome {
	if e, ok := f.s.reg.Entry(f.cctx, f.c.Command, []console.Sandbox{box}); ok && !e.Enabled {
		return refused(e.Why)
	}
	return f.enterBox(box)
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
		return f.session(box, console.InteractShell, console.DataSource.NewShell)
	}
}

// NewTerminal makes another of the harness's terminals in the box whose
// workspace is open.
func (f *frontend) NewTerminal() outcome {
	box := f.cctx.Workspace
	if box == nil {
		return refused("requires a workspace")
	}
	return func() Result { return f.session(*box, console.InteractTerminal, console.DataSource.NewTerminal) }
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

func (f *frontend) session(box console.Sandbox, asked console.Interaction, start func(console.DataSource, context.Context, string, int, int) (console.Exec, console.Terminal, error)) Result {
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
		f.s.streams.fail(stream, err, console.SessionTab(console.Exec{}, asked))
		return failed(err.Error())
	}
	made := console.SessionTab(ex, asked)
	tab := wireTab(made, ex)
	if !f.s.streams.attach(stream, term, made) {
		// Closed while it was made: the session goes on in the box, and the
		// app, which closed it, is told what it was.
		return Result{Status: StatusDone, Box: Key(box.Server, box.ID), Session: &tab, Note: "the pane closed before the session was made"}
	}
	return Result{Status: StatusDone, Box: Key(box.Server, box.ID), Session: &tab}
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
		r, ok, err := fill(f.ctx, f.s, f.c.ID, "Rename", "", console.RenameForm(box, box.Title()))
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
		update, ok, err := fill(f.ctx, f.s, f.c.ID, "Settings · "+box.Title(), "", console.SettingsForm(box, was, harnesses))
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
			variant, ok, fillErr := fill(f.ctx, f.s, f.c.ID, "Create variant of "+h.DisplayName(), "", console.HarnessVariantForm(h))
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

func (f *frontend) Tools(box console.Sandbox) outcome               { return f.enterBox(box) }
func (f *frontend) Issue(box console.Sandbox) outcome               { return f.enterIfAble(box) }
func (f *frontend) PullRequest(box console.Sandbox) outcome         { return f.enterIfAble(box) }
func (f *frontend) FilesChanged(box console.Sandbox) outcome        { return f.enterIfAble(box) }
func (f *frontend) Audit(box console.Sandbox) outcome               { return f.enterBox(box) }
func (f *frontend) CredentialRequests(box console.Sandbox) outcome  { return f.enterIfAble(box) }
func (f *frontend) RejectedCredentials(box console.Sandbox) outcome { return f.enterIfAble(box) }

func (f *frontend) Credentials() outcome    { return f.enter() }
func (f *frontend) Quit() outcome           { return f.enter() }
func (f *frontend) Help() outcome           { return f.enter() }
func (f *frontend) Filters() outcome        { return f.enter() }
func (f *frontend) AllBoxes() outcome       { return f.enter() }
func (f *frontend) ToggleArchived() outcome { return f.enter() }
func (f *frontend) Maximize() outcome       { return f.enter() }
func (f *frontend) Detach() outcome         { return f.enter() }
func (f *frontend) ToggleMouse() outcome    { return f.enter() }
func (f *frontend) DismissBanner() outcome  { return f.enter() }
func (f *frontend) NextPane() outcome       { return f.enter() }
func (f *frontend) PreviousPane() outcome   { return f.enter() }
func (f *frontend) NextBox() outcome        { return f.enter() }
func (f *frontend) PreviousBox() outcome    { return f.enter() }
func (f *frontend) ToggleSidebar() outcome  { return f.enter() }
