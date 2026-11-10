package bridge

import (
	"context"
	"errors"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The run options and creating a box. The window's run options
// (console.RunOptions) are held here, as the TUI's window holds its own:
// what the app's composer creates with, changed by the options command and by
// answering a create, and said under the composer (the composer topic). Which
// rows there are, what each offers and the request they make are the core's;
// the bridge reads them what they are read against — the sources the last
// listing holds, the folder and server the app's list is on, the harnesses
// and pools of that server — and runs the create.

// runOptions is the window's run options and what they were last read
// against; optsMu guards it.
type runOptions struct {
	opts *console.RunOptions
	// folder and server are the folder's source and the server the app's
	// list was on when the options were last read against it: the header
	// moving is what moves the Source and Server rows (RunOptions.SetFolder,
	// SetServer), never a read that finds it where it was.
	folder, server string
	// changed is closed when the options change, and the composer topic
	// sends again.
	changed chan struct{}
}

// openOptions is the run options a window opens on, in the TUI's order: the
// folder and server of the list as it opens (the view the last window left),
// then cut from where the last window's last create was (Session.LastSource),
// which the folder would otherwise move it off.
func openOptions(session console.Session, shown console.Filter) runOptions {
	o := console.NewRunOptions(session)
	o.SetFolder(shown.Folder.Source)
	o.OpenOnLastCut(session.LastSource)
	o.SetServer(shown.Server)
	return runOptions{opts: o, folder: shown.Folder.Source, server: shown.Server, changed: make(chan struct{})}
}

// optionsChanged wakes the composer topic.
func (s *Server) optionsChanged() {
	s.optsMu.Lock()
	close(s.options.changed)
	s.options.changed = make(chan struct{})
	s.optsMu.Unlock()
}

// readOptions brings the run options up to what they are read against, and
// returns a copy to build a form on, with the harnesses of the server a
// create goes to and whether they could be listed: the sources the last
// listing holds, the folder and server the app's list shows, the harnesses
// and pools of the server the Server row is on, and the Git credential
// preference the command preview names. A listing that fails leaves its row
// as it was, as the TUI's does. The composer topic is told.
//
// The Server row follows the list's server, as the TUI's does; on the desktop
// the row does not move the list back, since which servers the list shows is
// the app's navigation. Its rows are read for the row's server either way.
func (s *Server) readOptions(ctx context.Context) (o *console.RunOptions, server string, harnesses []console.Harness, listed bool) {
	session, listing := s.lastListing()
	s.mu.Lock()
	shown := s.shown
	s.mu.Unlock()
	list := console.NewList(session, console.ListStaleAfter)
	list.SetAll(listing.Sandboxes)

	s.optsMu.Lock()
	held := s.options.opts
	if shown.Folder.Source != s.options.folder {
		s.options.folder = shown.Folder.Source
		held.SetFolder(shown.Folder.Source)
	}
	if shown.Server != s.options.server {
		s.options.server = shown.Server
		held.SetServer(shown.Server)
		held.ClearPools()
	}
	held.SetSources(list.Sources())
	server = session.ServerName(held.Request("", "").Server)
	s.optsMu.Unlock()

	harnesses, herr := s.ds.Harnesses(ctx, server)
	pools, perr := s.ds.Pools(ctx, server)
	policy, gerr := s.ds.GitCredentialPolicy(ctx)

	s.optsMu.Lock()
	if herr == nil {
		held.SetHarnesses(harnesses)
	}
	if perr == nil {
		held.SetPools(pools)
	}
	held.GitPolicyError = gerr != nil
	if gerr == nil {
		held.Session.GitCredentials = policy
	}
	o = held.Clone()
	s.optsMu.Unlock()
	s.optionsChanged()
	return o, server, harnesses, herr == nil
}

// resolve checks a source typed by hand, for a run options form.
func (s *Server) resolve(ctx context.Context) console.Resolve {
	return func(source string) (console.Source, error) { return s.ds.ResolveSource(ctx, source) }
}

// RunOptions asks the run options as the core's form, and keeps what was
// answered for the creates that follow. With a source in its args — a folder
// dropped on the composer — it asks nothing: the source is checked as one
// typed on the form is, and the Source row set to it.
func (f *frontend) RunOptions() outcome {
	if f.c.Args.Source != "" {
		return f.dropSource(f.c.Args.Source)
	}
	return func() Result {
		o, _, _, _ := f.s.readOptions(f.ctx)
		answer, ok, err := fill(f.ctx, f.s, f.c.ID, "Run options", "", o.OptionsForm(f.c.Args.Prompt, f.s.resolve(f.ctx)))
		switch {
		case err != nil:
			return failed(err.Error())
		case !ok:
			return Result{Status: StatusCanceled}
		}
		f.s.optsMu.Lock()
		f.s.options.opts.TakeOptions(answer)
		f.s.optsMu.Unlock()
		f.s.optionsChanged()
		return Result{Status: StatusDone, Note: "run options saved"}
	}
}

// dropSource sets the Source row to a folder dropped on the composer, once
// the creation path has checked it (DataSource.ResolveSource), as a source
// typed on the run options' form is: where the creates that follow are cut
// from. The list is not moved, as a source answered on a form never moves it.
func (f *frontend) dropSource(folder string) outcome {
	return func() Result {
		s := f.s
		source, err := s.ds.ResolveSource(f.ctx, folder)
		if err != nil {
			return failed(err.Error())
		}
		s.optsMu.Lock()
		s.options.opts.NameSource(source)
		said := s.options.opts.Opts[console.OptSource].Display()
		s.optsMu.Unlock()
		s.optionsChanged()
		return Result{Status: StatusDone, Note: "new discoboxes are cut from " + said}
	}
}

// NewBox is a create from the composer: the create card as the core's form
// — where it is cut from, and the card's settings — then the questions a
// create asks before it is sent (which harness a project with no default
// runs, what of the working tree it carries), then the create itself,
// narrated: its steps as progress, the box the moment the server takes it.
// It ends entering the box's workspace, or done for a create that asked to be
// left detached. The source answered is where this one box is cut from, and
// where the next create opens; the list's filter is not moved.
func (f *frontend) NewBox() outcome {
	return func() Result {
		s, ctx, id := f.s, f.ctx, f.c.ID
		prompt := f.c.Args.Prompt
		// A harness's own way out typed into the composer is the window's
		// too: the app leaves, creating nothing.
		if console.LeavesWindow(prompt) {
			return Result{Status: StatusEnter}
		}
		o, server, harnesses, listed := s.readOptions(ctx)
		answer, ok, err := fill(ctx, s, id, "New discobox", "Create", o.CreateForm(prompt, s.resolve(ctx)))
		switch {
		case err != nil:
			return failed(err.Error())
		case !ok:
			return Result{Status: StatusCanceled}
		}
		s.optsMu.Lock()
		held := s.options.opts
		held.TakeCard(answer)
		req := held.CardRequest(prompt, answer.Source, nil)
		session := held.Session
		s.optsMu.Unlock()
		s.optionsChanged()
		if err := s.ds.SaveLastSource(ctx, answer.Source); err != nil {
			s.send(Message{Type: TypeProgress, Progress: &Progress{Command: id, Warning: true,
				Text: "cannot save where this was cut from: " + err.Error()}})
		}

		// The card can move the create to another server than its rows were
		// read for: its harnesses are that server's.
		if to := session.ServerName(req.Server); to != server {
			server = to
			var herr error
			harnesses, herr = s.ds.Harnesses(ctx, server)
			listed = herr == nil
		}
		q, ask, err := console.DefaultHarnessQuestion(req, harnesses, listed)
		if err != nil {
			return failed(err.Error())
		}
		if ask {
			key, err := s.ask(ctx, id, q)
			if err != nil {
				return declined(err)
			}
			harness, ok := console.ChosenHarness(harnesses, key)
			if !ok {
				return failed("no such harness")
			}
			if _, err := console.RunHarnessVerb(ctx, s.ds, server, harness.ID, console.HarnessSetDefault); err != nil {
				return failed(err.Error())
			}
			// The harness rows, and the composer's line, name the new default.
			s.readOptions(ctx)
		}

		if console.WorkspaceChecked(req) {
			s.send(Message{Type: TypeProgress, Progress: &Progress{Command: id, Text: "checking the working tree"}})
			ws, err := s.ds.Workspace(ctx, req.Source)
			if err != nil {
				return failed("cannot read the working tree: " + err.Error())
			}
			if q, ask := console.AskAboutWorkspace(req, ws); ask {
				key, err := s.ask(ctx, id, q.Question)
				if err != nil && ctx.Err() != nil {
					return declined(ctx.Err())
				}
				if err != nil {
					var ok bool
					if key, ok = q.Dismissed(); !ok || !errors.Is(err, context.Canceled) {
						return declined(err)
					}
				}
				req = q.Answer(req, key)
			}
		}

		s.send(Message{Type: TypeProgress, Progress: &Progress{Command: id, Text: "creating the discobox"}})
		n, done := s.narrate(ctx, id)
		box, err := s.ds.Run(ctx, req, n)
		done()
		if box.ID != "" {
			s.relist()
		}
		switch {
		case err != nil && box.ID == "":
			return failed(err.Error())
		case err != nil:
			// The server took it, and the rest failed: the box exists, and
			// is what the failure is about.
			return Result{Status: StatusFailed, Why: err.Error(), Box: Key(box.Server, box.ID)}
		case req.Detach:
			return Result{Status: StatusDone, Box: Key(box.Server, box.ID), Note: "created " + box.Title() + ", detached"}
		}
		return Result{Status: StatusEnter, Box: Key(box.Server, box.ID)}
	}
}

// declined is a command whose question was left: canceled when it was
// dismissed, failed otherwise.
func declined(err error) Result {
	if errors.Is(err, context.Canceled) {
		return Result{Status: StatusCanceled}
	}
	return failed(err.Error())
}

// composer follows the run options' summary: read against the harnesses and
// the list as it is subscribed — the line names the project's harness before
// anything is created — and sent again whenever the options change, which a
// new filter on the list also reads them for (Server.filter). It never reads
// the servers on a listing's beat.
func (f *follower) composer(ctx context.Context) {
	f.s.readOptions(ctx)
	for {
		f.s.optsMu.Lock()
		summary := f.s.options.opts.Summary()
		changed := f.s.options.changed
		f.s.optsMu.Unlock()
		chips := make([]Chip, 0, len(summary))
		for _, c := range summary {
			chips = append(chips, Chip{Text: c.Text, Chosen: c.Chosen})
		}
		f.send(Snapshot{Composer: &Composer{Chips: chips}})
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// saveDraft keeps the composer's prompt for the next window
// (DataSource.SaveDraft). When is the app's to say; the last window to say one
// is the one whose prompt comes back. A write that fails has no reply to be
// said in, and is tried again with the next.
func (s *Server) saveDraft(ctx context.Context, d Draft) {
	_ = s.ds.SaveDraft(ctx, d.Prompt)
}
