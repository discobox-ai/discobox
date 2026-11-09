package bridge

import (
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The core's values as the wire carries them.

// wireBox is a box as the app draws its row. What the box reports as known —
// its git state, its diff, its usage — wins over the listing's own fields.
func wireBox(b console.Sandbox) Box {
	out := Box{
		Key: Key(b.Server, b.ID), ID: b.ID, Server: b.Server, Name: b.Name, State: string(b.State),
		Agent: string(b.Agent.State), AgentMsg: b.Agent.Msg, Harness: b.HarnessName, Source: b.Source,
		Tags: nonNil(b.Tags), Branch: b.Branch, Commit: b.Commit, Dirty: b.Uncommitted(), Ahead: b.Ahead(),
		Created: b.Created, Message: b.Message, Provisioning: b.Provisioning, Attachable: b.Attachable(),
	}
	if out.Harness == "" {
		out.Harness = b.Harness
	}
	if b.CreatedBy != "" {
		// The box that made it is on the same server.
		out.Parent = Key(b.Server, b.CreatedBy)
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

// wireTab is a session as the app draws its tab: what the core says it is
// (console.SessionTab), and its column and kind.
func wireTab(t console.Tab, e console.Exec) Tab {
	kind := "shell"
	switch {
	case t.Primary:
		kind = "primary"
	case t.Tool != "":
		kind = "tool"
	case t.Service != "":
		kind = "service"
	case t.Side() == console.SideTerminals:
		kind = "terminal"
	}
	return Tab{Exec: e.ID, Kind: kind, Title: t.Title, Live: e.Live, Argv: nonNil(e.Command)}
}

func wireService(s console.Service) Service {
	return Service{ID: s.ID, Name: s.DisplayName(), Status: s.Status, Exec: s.ExecID, Detail: s.Detail()}
}

// wireRequest is one item waiting on a person, by what it asks; box is the
// key of the box that asked.
func wireRequest(r console.CredentialRequest, box string) Request {
	kind := "credential"
	switch {
	case r.Refresh != nil:
		kind = "refresh"
	case r.Trust != nil:
		kind = "trust"
	case r.Adoption != nil:
		kind = "adoption"
	case r.APIAccess:
		kind = "apiAccess"
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
func nonNil(s []string) []string { return append([]string{}, s...) }
