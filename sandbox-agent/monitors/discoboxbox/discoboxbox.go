// Package discoboxbox is the image's `discobox-box` monitor, a built-in (see
// builtin): what happens to other discoboxes this box can see through the
// discobox API — a box's state changing, its primary terminal going busy or
// idle, and a secret request from it pending and being answered.
//
// Each run lists the project's boxes (`list-sandboxes`) and, when requests are
// watched, the secret requests this box may read (`list-secret-requests`),
// and reports what differs from the last run. See DESIGN.md.
package discoboxbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/pkg/sandboxmeta"
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

// Run is one run of the monitor.
func Run(ctx context.Context, env builtin.Env) error {
	return run(ctx, env, &http.Client{Timeout: 30 * time.Second}, time.Now)
}

func run(ctx context.Context, env builtin.Env, hc *http.Client, now func() time.Time) error {
	cfg, err := parseConfig(env.Options)
	if err != nil {
		return err
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
	p := &poller{cfg: cfg, c: &client{base: base, project: project, http: hc}, env: env, st: &st, now: now}
	if err := p.poll(ctx); err != nil {
		return err
	}
	return env.WriteState(stateFile, &st)
}

// config is an instance's options.
type config struct {
	IDs      []string
	Tags     []string
	Watch    map[string]bool
	selected []sandboxmeta.Selector
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
	if len(cfg.IDs) == 0 && len(cfg.Tags) == 0 {
		return config{}, errors.New("name the boxes to watch: -o id=sbx_…,sbx_… or -o tag=key=value (selectors separated by spaces, every one matching)")
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

// selects reports whether the options select b: named by id, or matching
// every tag selector.
func (c config) selects(b box) bool {
	if slices.Contains(c.IDs, b.ID) {
		return true
	}
	return len(c.selected) > 0 && sandboxmeta.MatchesAll(b.Meta.Tags, c.selected)
}

// onlyIDs reports an instance that names its boxes and selects none: it ends
// once every one of them is gone.
func (c config) onlyIDs() bool { return len(c.IDs) > 0 && len(c.Tags) == 0 }

type poller struct {
	cfg config
	c   *client
	env builtin.Env
	st  *state
	now func() time.Time
}

func (p *poller) poll(ctx context.Context) error {
	// Tags alone are filtered by the server; with ids, a box named by id
	// need not match them, so the list is the project's.
	var filter []string
	if len(p.cfg.IDs) == 0 {
		filter = p.cfg.Tags
	}
	boxes, err := p.c.listBoxes(ctx, filter)
	if err != nil {
		return err
	}
	watched := map[string]box{}
	for _, b := range boxes {
		if p.cfg.selects(b) {
			watched[b.ID] = b
		}
	}
	var requests []secretRequest
	if p.cfg.Watch[watchRequests] {
		if requests, err = p.c.listRequests(ctx); err != nil {
			return err
		}
	}
	first := p.st.Baseline.IsZero()
	for _, id := range p.cfg.IDs {
		// A box named at the baseline, or named since by options changed in
		// place, has to exist; one that was there and went is gone.
		_, known := p.st.Boxes[id]
		if _, ok := watched[id]; !ok && !known && !slices.Contains(p.st.Gone, id) {
			return fmt.Errorf("option id: no box %s in this project", id)
		}
	}
	if first {
		p.st.Baseline = p.now()
	}

	ids := make([]string, 0, len(watched))
	for id := range watched {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := p.observe(watched[id], first); err != nil {
			return err
		}
	}
	if err := p.observeRequests(watched, requests, first); err != nil {
		return err
	}
	if err := p.letGo(watched); err != nil {
		return err
	}
	return p.finishIfGone()
}

// observe records one box's state and terminal, and reports what changed.
// A box seen for the first time is recorded, not reported, when it already
// existed at the baseline: it is newly selected, by a tag or options changed
// in place. One created after the baseline is news whenever it is first seen
// — a worker usually tags itself only once it runs — and reported created.
func (p *poller) observe(b box, first bool) error {
	prev := p.st.Boxes[b.ID]
	cur := &boxState{Name: b.name(), State: b.displayState()}
	if status, ok := posted(b); ok && cur.State == "running" {
		cur.Program, cur.Kind, cur.Msg = string(status.State), string(status.Kind), status.Msg
	}
	created := prev == nil && !first && b.CreatedAt.After(p.st.Baseline)
	p.st.Boxes[b.ID] = cur
	switch {
	case first, prev == nil && !created:
		return nil
	case prev == nil:
		if !p.cfg.Watch[watchState] {
			return nil
		}
		return p.emit(monitors.Event{
			Kind:  "box.created",
			Key:   "box-" + b.ID + "-state",
			Title: fmt.Sprintf("%s was created: %s", cur.Name, cur.State),
		})
	}
	if p.cfg.Watch[watchState] && cur.State != prev.State {
		if err := p.emit(monitors.Event{
			Kind:    "box.state",
			Key:     "box-" + b.ID + "-state",
			Title:   fmt.Sprintf("%s is %s", cur.Name, cur.State),
			Summary: "was " + prev.State,
		}); err != nil {
			return err
		}
	}
	if p.cfg.Watch[watchTerminal] && cur.Program != "" && (cur.Program != prev.Program || cur.Kind != prev.Kind) {
		kind := "box.idle"
		if cur.Program == string(programstatus.Working) {
			kind = "box.busy"
		}
		if err := p.emit(monitors.Event{
			Kind:  kind,
			Key:   "box-" + b.ID + "-terminal",
			Title: cur.Name + " " + terminalPhrase(cur),
			Body:  cur.Msg,
		}); err != nil {
			return err
		}
	}
	return nil
}

// posted is what the program in a box's primary terminal last reported of
// itself (ADR 26-10-16-424), as `discobox ls` reads it.
func posted(b box) (programstatus.Status, bool) {
	if b.Runtime == nil || b.Runtime.AgentStatus == nil || len(b.Runtime.AgentStatus.Sessions) == 0 {
		return programstatus.Status{}, false
	}
	return programstatus.PrimaryPosted(b.Runtime.AgentStatus.Sessions)
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
func (p *poller) observeRequests(watched map[string]box, requests []secretRequest, first bool) error {
	if !p.cfg.Watch[watchRequests] {
		return nil
	}
	seen := map[string]bool{}
	for _, r := range requests {
		b, ok := watched[r.SandboxID]
		if !ok {
			continue
		}
		seen[r.ID] = true
		prev, known := p.st.Requests[r.ID]
		p.st.Requests[r.ID] = r.Status
		// One made before the baseline is where things stood, not news;
		// one made since is, from whichever box, however late its box
		// came to be watched: a request waits on someone.
		if first || (!known && !r.CreatedAt.After(p.st.Baseline)) || prev == r.Status {
			continue
		}
		name := b.name()
		switch {
		case r.Status == "pending":
			routes := make([]string, 0, len(r.Routes))
			for _, rt := range r.Routes {
				routes = append(routes, rt.String())
			}
			body := strings.Join(routes, "\n")
			if err := p.emit(monitors.Event{
				Kind:    "box.request",
				Key:     "request-" + r.ID,
				Title:   fmt.Sprintf("%s asks for %s", name, r.what()),
				Summary: firstLine(r.Justification, 200),
				Body:    body,
			}); err != nil {
				return err
			}
		case known && prev == "pending":
			title := fmt.Sprintf("%s's request for %s was %s", name, r.what(), r.Status)
			if r.DecidedBy != "" {
				title += " by " + r.DecidedBy
			}
			if err := p.emit(monitors.Event{
				Kind:  "box.request.answered",
				Key:   "request-" + r.ID,
				Title: title,
			}); err != nil {
				return err
			}
		}
	}
	for id := range p.st.Requests {
		if !seen[id] {
			delete(p.st.Requests, id)
		}
	}
	return nil
}

// letGo forgets a box no longer selected, and reports one gone from the
// project.
func (p *poller) letGo(watched map[string]box) error {
	ids := make([]string, 0, len(p.st.Boxes))
	for id := range p.st.Boxes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if _, ok := watched[id]; ok {
			continue
		}
		prev := p.st.Boxes[id]
		delete(p.st.Boxes, id)
		// Named by id and missing is gone; a box only selected may simply
		// have lost its tag, or the options changed in place.
		if !slices.Contains(p.cfg.IDs, id) {
			continue
		}
		p.st.Gone = append(p.st.Gone, id)
		if !p.cfg.Watch[watchState] {
			continue
		}
		if err := p.emit(monitors.Event{
			Kind:    "box.gone",
			Key:     "box-" + id + "-state",
			Title:   prev.Name + " is gone",
			Summary: "was " + prev.State,
		}); err != nil {
			return err
		}
	}
	return nil
}

// finishIfGone ends an instance watching only named boxes once every one of
// them is gone.
func (p *poller) finishIfGone() error {
	if !p.cfg.onlyIDs() {
		return nil
	}
	for _, id := range p.cfg.IDs {
		if !slices.Contains(p.st.Gone, id) {
			return nil
		}
	}
	return p.emit(monitors.Event{
		Kind:  "discobox.finished",
		Title: "every box this watches is gone",
		Final: true,
	})
}

func (p *poller) emit(ev monitors.Event) error { return p.env.Emit(ev) }

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
