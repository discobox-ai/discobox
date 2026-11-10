package bridge

import (
	"github.com/discobox-ai/discobox/cli/internal/console"
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
// (console.SessionTab), and its column and kind.
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
	return Tab{Exec: e.ID, Kind: kind, Title: t.Title, Live: e.Live, Argv: nonNil(e.Command)}
}

func wireService(s console.Service) Service {
	return Service{ID: s.ID, Name: s.DisplayName(), Status: s.Status, Exec: s.ExecID, Detail: s.Detail()}
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

// nonNil is a copy that is never nil, so the wire carries [] rather than null.
func nonNil[T any](s []T) []T { return append([]T{}, s...) }
