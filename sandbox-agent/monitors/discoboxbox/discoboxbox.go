// Package discoboxbox is the image's `discobox-box` monitor, a built-in (see
// builtin): what happens to other discoboxes this box can see through the
// discobox API — a box's state changing, its primary terminal going busy or
// idle, and a secret request from it pending and being answered.
//
// Each run follows the project's sandbox event stream from the cursor the
// last one saved, for its interval, and reports what each event changes for
// the boxes it watches; when requests are watched, it then lists the secret
// requests this box may read (ADR 26-11-01-560). See DESIGN.md.
package discoboxbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/pkg/sandboxmeta"
	agentconfig "github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors/builtin"
)

// Name is the built-in's name, the declaration's id.
const Name = "discobox-box"

// The variables a box is told the discobox API in (ADR 26-10-27-880 §9).
const (
	envAPIURL  = "DISCOBOX_API_URL"
	envServer  = "DISCOBOX_SERVER"
	envProject = "DISCOBOX_PROJECT"
)

// stateFile is the instance's whole memory, in its state directory.
const stateFile = "discobox-box.json"

// defaultHold is how long a run follows the stream when the runner gave it
// no interval: the declaration's.
const defaultHold = 30 * time.Second

// maxHold is the longest a run follows the stream, whatever its interval: the
// pool's gate ends a call to the API after two minutes, body and all
// (proxyagent.gateHTTPTimeout). A longer interval waits out the rest before
// the next run, which resumes from the cursor and misses nothing.
const maxHold = 90 * time.Second

// requestTimeout bounds a run's listing of secret requests.
const requestTimeout = 30 * time.Second

// createdByMe is the created-by value naming this box.
const createdByMe = "me"

// Run is one run of the monitor.
func Run(ctx context.Context, env builtin.Env) error {
	return run(ctx, env, deps{
		http: &http.Client{},
		now:  time.Now,
		self: func() (string, error) {
			cfg, err := agentconfig.Load(agentconfig.DefaultPath)
			if err != nil {
				return "", err
			}
			return cfg.Identity.SandboxID, nil
		},
	})
}

// deps are what a run reaches outside itself.
type deps struct {
	http *http.Client
	now  func() time.Time
	// self is this box's sandbox ID, for created-by=me.
	self func() (string, error)
}

func run(ctx context.Context, env builtin.Env, d deps) error {
	cfg, err := parseConfig(env.Options)
	if err != nil {
		return err
	}
	if cfg.CreatedBy == createdByMe {
		id, err := d.self()
		if err != nil {
			return fmt.Errorf("option created-by: this box's own ID is unknown: %w", err)
		}
		if strings.TrimSpace(id) == "" {
			return errors.New("option created-by: this box's own ID is unknown: its sandbox config names none")
		}
		cfg.CreatedBy = id
	}
	base := strings.TrimSpace(env.Getenv(envAPIURL))
	if base == "" {
		base = strings.TrimSpace(env.Getenv(envServer))
	}
	if base == "" {
		return errors.New("no discobox API in this box: " + envAPIURL + " is not set")
	}
	project := strings.TrimSpace(env.Getenv(envProject))
	if project == "" {
		// A box's own project, to the server (ADR 26-10-27-880 §9).
		project = "default"
	}
	var st state
	if _, err := env.ReadState(stateFile, &st); err != nil {
		return err
	}
	st.init()
	hold := env.Interval
	if hold <= 0 {
		hold = defaultHold
	}
	hold = min(hold, maxHold)
	f := &follower{cfg: cfg, c: &client{base: base, project: project, http: d.http}, env: env, st: &st}
	runErr := f.run(ctx, d.now, hold)
	if err := env.WriteState(stateFile, &st); err != nil {
		return errors.Join(runErr, err)
	}
	return runErr
}

// config is an instance's options.
type config struct {
	IDs       []string
	Tags      []string
	CreatedBy string
	Watch     map[string]bool
	selected  []sandboxmeta.Selector
}

// The kinds of change an instance may watch.
const (
	watchState    = "state"
	watchTerminal = "terminal"
	watchRequests = "requests"
)

func parseConfig(opts map[string]string) (config, error) {
	var cfg config
	for _, id := range strings.FieldsFunc(opts["id"], func(r rune) bool { return r == ',' || r == ' ' }) {
		if !slices.Contains(cfg.IDs, id) {
			cfg.IDs = append(cfg.IDs, id)
		}
	}
	// Selectors are separated by spaces: a comma followed by key= starts
	// another -o option, and a selector is key=value.
	cfg.Tags = strings.Fields(opts["tag"])
	selected, err := sandboxmeta.ParseSelectors(cfg.Tags)
	if err != nil {
		return config{}, fmt.Errorf("option tag: %w", err)
	}
	cfg.selected = selected
	cfg.CreatedBy = strings.TrimSpace(opts["created-by"])
	if strings.ContainsAny(cfg.CreatedBy, ", ") {
		return config{}, fmt.Errorf("option created-by: one box ID, or %s; %q is not", createdByMe, cfg.CreatedBy)
	}
	if len(cfg.IDs) == 0 && len(cfg.Tags) == 0 && cfg.CreatedBy == "" {
		return config{}, errors.New("name the boxes to watch: -o id=sbx_…,sbx_…, -o tag=key=value (selectors separated by spaces, every one matching), or -o created-by=me")
	}
	cfg.Watch = map[string]bool{}
	raw := strings.TrimSpace(opts["watch"])
	if raw == "" {
		raw = strings.Join([]string{watchState, watchTerminal, watchRequests}, ",")
	}
	for _, kind := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
		switch kind {
		case watchState, watchTerminal, watchRequests:
			cfg.Watch[kind] = true
		default:
			return config{}, fmt.Errorf("option watch: %q is not state, terminal or requests", kind)
		}
	}
	return cfg, nil
}

// selects reports whether the options select the box: named by id, or
// matching every tag selector and its creator, those that are given.
func (c config) selects(id string, b *boxState) bool {
	if slices.Contains(c.IDs, id) {
		return true
	}
	if len(c.selected) == 0 && c.CreatedBy == "" {
		return false
	}
	if c.CreatedBy != "" && b.CreatedBy != c.CreatedBy {
		return false
	}
	return sandboxmeta.MatchesAll(b.Tags, c.selected)
}

// onlyIDs reports an instance that names its boxes and selects none: it ends
// once every one of them is gone.
func (c config) onlyIDs() bool { return len(c.IDs) > 0 && len(c.Tags) == 0 && c.CreatedBy == "" }

type follower struct {
	cfg config
	c   *client
	env builtin.Env
	st  *state
	// cursor is the last ID the stream gave this run, which st.Cursor takes
	// whenever it is safe to resume from.
	cursor string
	// replaying is set while the stream sends the project's current state —
	// from a first connection with no cursor, or a reset — until the ID-only
	// event that ends it (ADR 26-11-01-560 §3); replayed once one has ended.
	replaying, replayed bool
}

func (f *follower) run(ctx context.Context, now func() time.Time, hold time.Duration) error {
	first := f.st.Baseline.IsZero()
	if first {
		f.st.Baseline = now()
	}
	f.cursor = f.st.Cursor
	resumed := f.st.Cursor != "" && !f.st.Resetting
	f.replaying = f.st.Cursor == ""
	if f.replaying && !f.st.Resetting {
		// A replay from scratch over boxes already held — a first one cut
		// off before its end — confirms them as a reset's does: one deleted
		// meanwhile is not in it. Unlike a reset's, it marks no box new: what
		// is not held may be what the cut replay never reached.
		f.st.Unconfirmed = f.st.boxIDs()
	}
	// Options changed in place select boxes anew before any event does.
	for _, id := range f.st.boxIDs() {
		if err := f.reselect(id); err != nil {
			return err
		}
	}
	streamCtx, cancel := context.WithTimeout(ctx, hold)
	defer cancel()
	streamErr := f.c.stream(streamCtx, f.st.Cursor, f.handle)
	if ctx.Err() != nil {
		// Stopped from outside: what was handled is saved, and nothing is
		// concluded from a stream cut short.
		return nil
	}
	if !f.replaying {
		f.st.Cursor = f.cursor
	}
	// The project's state is whole once a replay has ended, or when the run
	// went on from a cursor with every box already held; only then does a
	// named box that is not there not exist.
	if streamErr == nil && !f.st.Resetting && (resumed || f.replayed) {
		for _, id := range f.cfg.IDs {
			// A box named at the baseline, or named since by options changed
			// in place, has to exist; one that was there and went is gone.
			if _, known := f.st.Boxes[id]; !known && !slices.Contains(f.st.Gone, id) {
				return fmt.Errorf("option id: no box %s in this project", id)
			}
		}
	}
	// Requests are not on the stream. They are listed at the run's end, once
	// the boxes they come from are known, and also when the stream failed for
	// its own reason, so a stream that keeps failing does not hide them; not
	// when the API refused this box, which it would again.
	var refused *apiError
	if f.cfg.Watch[watchRequests] && !errors.As(streamErr, &refused) {
		listCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		requests, err := f.c.listRequests(listCtx)
		cancel()
		if err != nil {
			return errors.Join(streamErr, err)
		}
		if err := f.observeRequests(requests, first); err != nil {
			return errors.Join(streamErr, err)
		}
	}
	if streamErr != nil {
		return streamErr
	}
	return f.finishIfGone()
}

// handle takes one event off the stream.
func (f *follower) handle(ev streamEvent) error {
	if ev.id != "" {
		f.cursor = ev.id
		// Not part way through a replay, a first one or a reset's: a cursor
		// saved there would skip the rest of it.
		if !f.replaying {
			f.st.Cursor = ev.id
		}
	}
	switch ev.typ {
	case "":
		// Only the cursor moved. After a replay it says where the stream
		// stands, which ends it: the project's current state has all arrived.
		if !f.replaying {
			return nil
		}
		f.replaying, f.replayed = false, true
		return f.confirmReplay()
	case eventReset:
		// Events were missed. The replay that follows is the truth: each box
		// in it is compared with what is held, and the boxes it leaves out
		// are gone once the ID-only event that ends it arrives.
		f.st.Resetting = true
		f.st.Unconfirmed = f.st.boxIDs()
		f.replaying = true
		return nil
	case eventDeleted:
		return f.deleted(ev.data.SandboxID)
	case eventCreated, eventUpdated:
		return f.observe(ev.typ, ev.data)
	}
	return nil
}

// observe records one box's state, and reports what changed for a watched
// box. A box seen for the first time is recorded, not reported, unless it
// was created after the baseline; then it is news whenever it is first
// watched, since a worker usually tags itself only once it runs.
func (f *follower) observe(typ string, data boxEvent) error {
	id := data.SandboxID
	f.st.Unconfirmed = slices.DeleteFunc(f.st.Unconfirmed, func(held string) bool { return held == id })
	prev := f.st.Boxes[id]
	cur := &boxState{
		Name:      data.name(),
		State:     data.DisplayState,
		Tags:      maps.Clone(data.Tags),
		CreatedBy: data.CreatedBySandboxID,
	}
	if data.Program != nil && cur.State == "running" {
		cur.Program, cur.Kind = data.Program.State, data.Program.Kind
	}
	if prev == nil {
		// Every box in the project is held, so one first seen in a reset's
		// replay was created while events were missed, whatever its latest
		// event is; otherwise its creation is.
		cur.New = f.st.Resetting || (typ == eventCreated && data.At.After(f.st.Baseline))
	} else {
		cur.New, cur.Watched = prev.New, prev.Watched
	}
	f.st.Boxes[id] = cur
	if prev == nil || !prev.Watched {
		return f.reselect(id)
	}
	if !f.cfg.selects(id, cur) {
		// No longer selected: it lost its tag, or its options changed.
		cur.Watched = false
		return nil
	}
	if f.cfg.Watch[watchState] && cur.State != prev.State {
		if err := f.emit(monitors.Event{
			Kind:    "box.state",
			Key:     "box-" + id + "-state",
			Title:   fmt.Sprintf("%s is %s", cur.Name, cur.State),
			Summary: "was " + prev.State,
		}); err != nil {
			return err
		}
	}
	if f.cfg.Watch[watchTerminal] && cur.Program != "" && (cur.Program != prev.Program || cur.Kind != prev.Kind) {
		kind := "box.idle"
		if cur.Program == string(programstatus.Working) {
			kind = "box.busy"
		}
		if err := f.emit(monitors.Event{
			Kind:  kind,
			Key:   "box-" + id + "-terminal",
			Title: cur.Name + " " + terminalPhrase(cur),
		}); err != nil {
			return err
		}
	}
	return nil
}

// reselect matches a held box against the options, and reports one newly
// watched that was created since the baseline. One that was already there
// is where things stood, and is recorded quietly.
func (f *follower) reselect(id string) error {
	b := f.st.Boxes[id]
	selected := f.cfg.selects(id, b)
	if selected == b.Watched {
		return nil
	}
	b.Watched = selected
	if !selected || !b.New {
		return nil
	}
	b.New = false
	if !f.cfg.Watch[watchState] {
		return nil
	}
	return f.emit(monitors.Event{
		Kind:  "box.created",
		Key:   "box-" + id + "-state",
		Title: fmt.Sprintf("%s was created: %s", b.Name, b.State),
	})
}

// deleted forgets a box, and reports one that was watched.
func (f *follower) deleted(id string) error {
	f.st.Unconfirmed = slices.DeleteFunc(f.st.Unconfirmed, func(held string) bool { return held == id })
	prev := f.st.Boxes[id]
	if prev == nil {
		return nil
	}
	delete(f.st.Boxes, id)
	if slices.Contains(f.cfg.IDs, id) && !slices.Contains(f.st.Gone, id) {
		f.st.Gone = append(f.st.Gone, id)
	}
	if !prev.Watched || !f.cfg.Watch[watchState] {
		return nil
	}
	return f.emit(monitors.Event{
		Kind:    "box.gone",
		Key:     "box-" + id + "-state",
		Title:   prev.Name + " is gone",
		Summary: "was " + prev.State,
	})
}

// confirmReplay ends a replay that has arrived whole: the boxes held before
// it that it left out were deleted meanwhile, a reset ends, and the cursor
// may move past it.
func (f *follower) confirmReplay() error {
	missing := f.st.Unconfirmed
	f.st.Unconfirmed, f.st.Resetting = nil, false
	f.st.Cursor = f.cursor
	for _, id := range missing {
		if err := f.deleted(id); err != nil {
			return err
		}
	}
	return nil
}

// terminalPhrase says what a box's primary terminal is doing.
func terminalPhrase(s *boxState) string {
	switch programstatus.State(s.Program) {
	case programstatus.Working:
		return "is working"
	case programstatus.Blocked:
		if s.Kind != "" {
			return "is waiting on a " + s.Kind
		}
		return "is waiting"
	case programstatus.Done:
		return "is done"
	case programstatus.Error:
		return "stopped on an error"
	}
	return "is idle"
}

// observeRequests reports a watched box's secret request pending, and
// answered. Only the requests of boxes this box created can be read; others'
// never appear.
func (f *follower) observeRequests(requests []secretRequest, first bool) error {
	seen := map[string]bool{}
	for _, r := range requests {
		b := f.st.Boxes[r.SandboxID]
		if b == nil || !b.Watched {
			continue
		}
		seen[r.ID] = true
		prev, known := f.st.Requests[r.ID]
		f.st.Requests[r.ID] = r.Status
		// One made before the baseline is where things stood, not news;
		// one made since is, from whichever box, however late its box
		// came to be watched: a request waits on someone.
		if first || (!known && !r.CreatedAt.After(f.st.Baseline)) || prev == r.Status {
			continue
		}
		switch {
		case r.Status == "pending":
			routes := make([]string, 0, len(r.Routes))
			for _, rt := range r.Routes {
				routes = append(routes, rt.String())
			}
			if err := f.emit(monitors.Event{
				Kind:    "box.request",
				Key:     "request-" + r.ID,
				Title:   fmt.Sprintf("%s asks for %s", b.Name, r.what()),
				Summary: firstLine(r.Justification, 200),
				Body:    strings.Join(routes, "\n"),
			}); err != nil {
				return err
			}
		case known && prev == "pending":
			title := fmt.Sprintf("%s's request for %s was %s", b.Name, r.what(), r.Status)
			if r.DecidedBy != "" {
				title += " by " + r.DecidedBy
			}
			if err := f.emit(monitors.Event{
				Kind:  "box.request.answered",
				Key:   "request-" + r.ID,
				Title: title,
			}); err != nil {
				return err
			}
		}
	}
	for id := range f.st.Requests {
		if !seen[id] {
			delete(f.st.Requests, id)
		}
	}
	return nil
}

// finishIfGone ends an instance watching only named boxes once every one of
// them is gone.
func (f *follower) finishIfGone() error {
	if !f.cfg.onlyIDs() {
		return nil
	}
	for _, id := range f.cfg.IDs {
		if !slices.Contains(f.st.Gone, id) {
			return nil
		}
	}
	return f.emit(monitors.Event{
		Kind:  "discobox.finished",
		Title: "every box this watches is gone",
		Final: true,
	})
}

// emit prints one event, and saves the state behind it: a run ended after
// this goes on from here rather than reporting it again.
func (f *follower) emit(ev monitors.Event) error {
	if err := f.env.Emit(ev); err != nil {
		return err
	}
	return f.env.WriteState(stateFile, f.st)
}

// firstLine is text's first non-blank line, cut to at most n runes.
func firstLine(text string, n int) string {
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if r := []rune(line); len(r) > n {
				return string(r[:n])
			}
			return line
		}
	}
	return ""
}
