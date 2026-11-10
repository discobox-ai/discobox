package bridge

import (
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// The core's values as the wire carries them.

// wireBox is a box as the app draws its row. What the box reports as known —
// its git state, its diff, its usage — wins over the listing's own fields.
func wireBox(b console.Sandbox) Box {
	out := Box{
		Key: Key(b.Server, b.ID), ID: b.ID, Server: b.Server, Name: b.Name, State: b.State,
		Agent: string(b.Agent.State), AgentMsg: b.Agent.Msg, Harness: b.HarnessName, Source: b.Source,
		Tags: nonNil(b.Tags), Branch: b.Branch, Commit: b.Commit, Dirty: b.Uncommitted(), Ahead: b.Ahead(),
		Created: b.Created, Message: b.Message, Provisioning: b.Provisioning, Attachable: b.Attachable(),
	}
	if out.Harness == "" {
		out.Harness = b.Harness
	}
	if b.Git.Known {
		out.Branch, out.Commit = b.Git.Branch, b.Git.Commit
	}
	if b.Diff.Known {
		out.Added, out.Deleted = b.Diff.Added, b.Diff.Deleted
	}
	// A box that is not up uses nothing, and has no sample to report.
	if b.Up() && b.Usage.Known {
		out.CPU, out.MemoryBytes = b.Usage.CPUPercent, b.Usage.MemoryBytes
	}
	return out
}

// wireListing is the core's list as the app draws it: every box listed, and
// the rows the filter shows with their places, notes and choices.
func wireListing(l *console.List) Listing {
	all := l.All()
	out := Listing{
		Boxes: make([]Box, 0, len(all)), Grouped: l.Grouped(), Notes: []Note{},
		Filter: wireFilter(l.Filter), Label: l.FilterLabel(), Narrowed: l.Narrowed(), Archived: l.ArchivedCount(),
		Unreachable: nonNil(l.Unreachable()), Waiting: nonNil(l.Waiting()),
	}
	for _, b := range all {
		out.Boxes = append(out.Boxes, wireBox(b))
	}
	rows := l.Rows()
	out.Rows = make([]Row, 0, len(rows))
	for _, b := range rows {
		n := l.Nesting(b)
		r := Row{
			Box: Key(b.Server, b.ID), Section: l.Section(b), SectionTitle: l.SectionTitle(b),
			Depth: n.Depth, Last: n.Last, Rules: nonNil(n.Rules), Origin: l.Origin(b),
		}
		if rm := b.Remark(); rm.Text != "" {
			r.Remark = &Remark{Text: rm.Text, Command: rm.Command, Verb: rm.Verb}
		}
		out.Rows = append(out.Rows, r)
	}
	for _, n := range l.Notes() {
		out.Notes = append(out.Notes, Note{Server: n.Server, Title: n.Title, Detail: n.Detail})
	}
	choices := l.FilterChoices()
	out.Choices = make([]FilterChoice, 0, len(choices))
	for _, c := range choices {
		out.Choices = append(out.Choices, FilterChoice{
			Group: c.Group, Label: c.Label, Detail: c.Detail, Marked: c.Marked, Several: c.Several,
			Mark: wireFilter(c.Mark), Unmark: wireFilter(c.Unmark),
		})
	}
	return out
}

// wireFilter is a filter as the wire carries it: the view it saves as, and
// whether the archived are listed.
func wireFilter(f console.Filter) Filter {
	v := f.View()
	return Filter{
		Server: v.Server, FolderKey: v.FolderKey, FolderLabel: v.FolderLabel, FolderSource: v.FolderSource,
		FolderLocal: v.FolderLocal, Tags: nonNil(v.Tags), Archived: f.Archived,
	}
}

// consoleFilter is the filter the app sent: no folder key is every folder.
func consoleFilter(f Filter) console.Filter {
	out := console.Filter{Server: f.Server, Tags: f.Tags, Archived: f.Archived, Folder: console.EveryFolder}
	if len(out.Tags) == 0 {
		out.Tags = nil
	}
	if f.FolderKey != "" {
		out.Folder = console.Folder{Key: f.FolderKey, Label: f.FolderLabel, Source: f.FolderSource, Local: f.FolderLocal}
	}
	return out
}

// wireTab is a session as the app draws its tab: what the core says it is
// (console.SessionTab, console.ServiceTab), and its column and kind.
func wireTab(t console.Tab, e console.Exec) Tab {
	kind := TabShell
	switch {
	case t.Primary:
		kind = TabPrimary
	case t.Tool != "":
		kind = TabTool
	case t.Service != "":
		kind = TabService
	case t.Side() == console.SideTerminals:
		kind = TabTerminal
	}
	return Tab{Exec: e.ID, Kind: kind, Title: t.Title, Live: e.Live, Argv: nonNil(e.Command), Service: t.Service, Run: t.ServiceRun, Status: t.Status, Tool: t.Tool,
		Images: t.TakesImages(), ImageKeys: t.TakesImageKeys()}
}

// wireTool is a tool as the tools menu lists it for a box: why it cannot run
// against it, and running when the box has its session live. path is where
// a file's copy is on this machine (DataSource.ToolFilePath).
func wireTool(t console.Tool, box console.Sandbox, running bool, path func(console.ToolFile) string) Tool {
	why := t.Why(box)
	out := Tool{ID: t.ID, Label: t.Label, Detail: t.PickerDetail(running), Host: t.Host, Enabled: why == "", Why: why, Files: make([]ToolFile, 0, len(t.Files))}
	for _, f := range t.Files {
		out.Files = append(out.Files, ToolFile{Name: f.Name, Detail: console.ToolFileDetail(f, path(f))})
	}
	return out
}

// wireEnd is what an ending does, as the wire says it.
func wireEnd(e console.TabEnd) TabEnd {
	switch e {
	case console.EndsWorkspace:
		return EndWorkspace
	case console.EndsHeld:
		return EndHeld
	default:
		return EndTab
	}
}

// wireHeader is the core's header as the app draws it.
func wireHeader(h console.Header) Header {
	out := Header{
		ID: h.ID, Work: make([]Link, 0, len(h.Work)), Base: h.Base, Changes: h.Changes,
		Tone: ToneQuiet, Ready: h.Ready, Desktop: nil, Ports: make([]PortGroup, 0, len(h.Ports)), Tags: nonNil(h.Tags),
	}
	if h.Blocked {
		out.Blocked = console.StatusBlocked
	}
	switch h.Tone {
	case console.ToneDirty:
		out.Tone = ToneDirty
	case console.ToneAhead:
		out.Tone = ToneAhead
	}
	for _, l := range h.Work {
		out.Work = append(out.Work, Link{Label: l.Label, URL: l.URL})
	}
	if h.Diff != nil {
		out.Diff, out.Added, out.Deleted = true, h.Diff.Added, h.Diff.Deleted
	}
	if h.Agent.State != "" {
		out.Agent, out.AgentMsg = string(h.Agent.State), programstatus.Displayable(h.Agent.Msg, programstatus.MaxMsg)
	}
	if h.Desktop != nil {
		out.Desktop = &Link{Label: h.Desktop.Label, URL: h.Desktop.URL}
	}
	for _, g := range h.Ports {
		group := PortGroup{Protocol: g.Protocol, Ports: make([]Link, 0, len(g.Ports))}
		for _, p := range g.Ports {
			group.Ports = append(group.Ports, Link{Label: p.Label, URL: p.URL})
		}
		out.Ports = append(out.Ports, group)
	}
	return out
}

// wireService is a declared service as the services menu lists it, with its
// verbs as the core offers them.
func wireService(s console.Service) Service {
	out := Service{ID: s.ID, Name: s.DisplayName(), Status: s.Status, Detail: s.Detail(), Actions: []ServiceAction{}}
	for _, a := range s.Actions() {
		out.Actions = append(out.Actions, ServiceAction{Verb: a.Verb, Label: a.Label, Detail: a.Detail, Enabled: a.Enabled, Why: a.Why})
	}
	return out
}

// wireHarness is a harness as the harnesses page lists it, its actions as the
// core offers them; now is when it was read, which its age is told from.
func wireHarness(h console.Harness, now time.Time) Harness {
	out := Harness{
		ID: h.ID, Name: h.DisplayName(), Default: h.Default, BuiltIn: h.BuiltIn, Bundle: h.Bundle, Secrets: len(h.Secrets),
		Files: []HarnessFile{}, Actions: []HarnessOffer{},
	}
	if !h.Updated.IsZero() {
		out.Age = console.Ago(h.Updated, now)
	}
	for _, f := range h.Files {
		out.Files = append(out.Files, HarnessFile{Path: f.Path, Size: len(f.Content), Notes: nonNil(f.Notes())})
	}
	for _, o := range h.Actions() {
		out.Actions = append(out.Actions, HarnessOffer{Action: o.Action, Label: o.Label, Detail: o.Detail, Enabled: o.Enabled, Why: o.Why})
	}
	return out
}

// wireCard is a card's rows.
func wireCard(rows []console.CardRow) []CardRow {
	out := make([]CardRow, 0, len(rows))
	for _, r := range rows {
		row := CardRow{Label: r.Label, Spans: []CardSpan{}, Lines: nonNil(r.Lines), More: r.More}
		for _, s := range r.Spans {
			row.Spans = append(row.Spans, CardSpan{Text: s.Text, Tone: s.Tone})
		}
		out = append(out, row)
	}
	return out
}

// wireChanges is a box's harness changes as the review shows them, with what
// it offers to do with them.
func wireChanges(c console.HarnessChanges) HarnessChanges {
	out := HarnessChanges{
		Candidate: c.CandidateID, Summary: c.Summary(), Saving: console.HarnessChangesSaving,
		Files: []ChangedFile{}, Credentials: nonNil(c.Credentials), Diagnostics: nonNil(c.Diagnostics), Offers: []ChangeOffer{},
	}
	if c.Empty() {
		out.Nothing = console.HarnessChangesNothing
	}
	for _, f := range c.Files {
		out.Files = append(out.Files, ChangedFile{Path: f.Path, Label: f.Label(), Diff: f.Diff})
	}
	for _, o := range c.Offers() {
		out.Offers = append(out.Offers, ChangeOffer{Change: o.Change, Label: o.Label, Enabled: o.Enabled, Why: o.Why})
	}
	return out
}

// wireRequest is one item waiting on a person, by what it asks; box is the
// key of the box that asked.
func wireRequest(r console.CredentialRequest, box string) Request {
	kind := RequestCredential
	switch {
	case r.Refresh != nil:
		kind = RequestRefresh
	case r.Trust != nil:
		kind = RequestTrust
	case r.Adoption != nil:
		kind = RequestAdoption
	case r.APIAccess:
		kind = RequestAPIAccess
	}
	return Request{
		ID: r.ID, Box: box, Server: r.Server, Kind: kind, Name: r.Name, EnvVar: r.EnvVar,
		Host: r.Host, Justification: r.Justification, Created: r.Created,
	}
}

// wireRejection is a refused credential, with the keys of the boxes it
// affects.
func wireRejection(r console.SecretRejection, boxes []string) Rejection {
	return Rejection{
		Boxes: nonNil(boxes), Server: r.Server, SecretID: r.SecretID, SecretName: r.SecretName, Host: r.Host,
		Reason: r.Reason, EnvName: r.EnvName, LastSeen: r.LastSeen,
	}
}

// wireEntry is a command as it stands, with a reason only when it cannot act.
func wireEntry(e console.Entry, uses int) Entry {
	out := Entry{Command: e.ID, Label: e.Label, Detail: e.Detail, Enabled: e.Enabled, Uses: uses}
	if !e.Enabled {
		out.Why = e.Why
	}
	return out
}

// wireDetail is a card the core describes, as the wire carries it.
func wireDetail(in []console.DetailSection) []DetailSection {
	out := make([]DetailSection, 0, len(in))
	for _, sec := range in {
		d := DetailSection{Label: sec.Label, Fields: make([]DetailField, 0, len(sec.Fields)), Lines: make([]DetailLine, 0, len(sec.Lines))}
		for _, f := range sec.Fields {
			d.Fields = append(d.Fields, DetailField{Label: f.Label, Value: f.Value, Tone: f.Tone})
		}
		for _, l := range sec.Lines {
			d.Lines = append(d.Lines, DetailLine{Text: l.Text, Tone: l.Tone})
		}
		out = append(out, d)
	}
	return out
}

// secretsPage is what the secrets page knows about a server's secrets
// beside the secrets themselves: every live binding of the project, which
// of them an upstream refuses (console.RefusedSecrets), and the harnesses and
// boxes a binding's target is named from (console.BindableBoxes).
type secretsPage struct {
	bindings  []console.SecretBinding
	refused   map[string]console.SecretRejection
	harnesses []console.Harness
	boxes     []console.Sandbox
	now       time.Time
}

// editAbout is what a secret's edit form says above it
// (console.EditSecretAbout), a line each.
func (p secretsPage) editAbout(s console.Secret) string {
	var refused *console.SecretRejection
	if rejection, ok := p.refused[s.ID]; ok {
		refused = &rejection
	}
	var lines []string
	for _, l := range console.EditSecretAbout(refused) {
		lines = append(lines, l.Text)
	}
	return strings.Join(lines, "\n")
}

// target is what a binding is on, as it is said to a person.
func (p secretsPage) target(b console.SecretBinding) string {
	return console.BindingTarget(b, p.harnesses, p.boxes)
}

// wireSecret is a secret as the page lists and reads it.
func (p secretsPage) wireSecret(s console.Secret) Secret {
	live := console.LiveBindings(p.bindings, s.ID, p.now)
	where, nowhere := console.SecretWhere(s)
	rejection, refused := p.refused[s.ID]
	text, standing := console.SecretStandingOf(len(live), refused)
	out := Secret{
		ID: s.ID, Name: s.Name, Where: where, Nowhere: nowhere, StandingText: text, Standing: standing,
		Age: console.Ago(s.Updated, p.now), Card: wireDetail(console.DescribeSecret(s, p.now)),
		Answer: console.BindingsAnswer(len(live)), Bindings: make([]SecretBinding, 0, len(live)),
	}
	if refused {
		out.Refused = console.RejectionSummary(rejection)
	}
	for _, b := range live {
		target := p.target(b)
		out.Bindings = append(out.Bindings, SecretBinding{
			ID: b.ID, Label: console.BindingLabel(b), Detail: console.BindingDetail(b, target, p.now),
			Card: wireDetail(console.DescribeBinding(s.Name, b, target, p.now)),
		})
	}
	return out
}

// wireDecision is a decision of a binding history, with the delegation it
// was handed on under when that still stands among the project's bindings.
func (p secretsPage) wireDecision(e console.BindingDecision) Decision {
	out := Decision{ID: e.ID, Label: e.Label(), Card: wireDetail(console.DescribeDecision(e)), Delegation: []DetailSection{}}
	for _, b := range p.bindings {
		if e.ParentID != "" && b.ID == e.ParentID {
			out.Delegation = wireDetail(console.DescribeBinding(b.SecretName, b, p.target(b), p.now))
		}
	}
	return out
}

// secretWords are what the secrets page says beside its secrets, in the
// core's words.
func secretWords() *SecretWords {
	return &SecretWords{
		Empty: console.NoSecrets, Bind: console.BindThisSecret, BindDetail: console.BindThisSecretDetail,
		HistoryTitle: console.BindingHistoryTitle, HistoryNote: console.BindingHistoryNote,
	}
}

// nonNil is a copy that is never nil, so the wire carries [] rather than null.
func nonNil[T any](s []T) []T { return append([]T{}, s...) }
