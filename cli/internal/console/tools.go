package console

import (
	"context"
	"errors"
	"fmt"
)

// The tools picker's rules (ADR 26-11-07-876 §1): which of a box's tools it
// lists, why one cannot run, running one that runs on this machine, editing
// one's files, and the box's two addresses beside them. Each renderer draws
// them — the TUI's card, the desktop's menu — and binds its own keys; neither
// decides any of it. Which tools there are is the data source's (ADR 0125);
// what a tool's session is, is the workspace's (ToolTab, Exec.IsTool).

// PickerTools is a catalog as the picker lists it: every tool, and every
// declaration that cannot run with the reason, except one the merge refused
// beside a tool that does run under its id (ADR 0125 §5) — that row would be
// a second entry for the same tool, and choosing either would mean the one
// that runs.
func PickerTools(catalog []Tool) []Tool {
	runs := map[string]bool{}
	for _, t := range catalog {
		if t.Problem == "" {
			runs[t.ID] = true
		}
	}
	out := make([]Tool, 0, len(catalog))
	for _, t := range catalog {
		if t.Problem != "" && runs[t.ID] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ToolIn is the tool with an id in a catalog. A catalog can hold a
// declaration the merge refused beside the tool it tried to replace, under
// the same id (ADR 0125 §5): the one that runs is the tool, and a refused
// one is found only when nothing under the id runs.
func ToolIn(catalog []Tool, id string) (Tool, bool) {
	var refused *Tool
	for i, t := range catalog {
		if t.ID != id {
			continue
		}
		if t.Problem == "" {
			return t, true
		}
		if refused == nil {
			refused = &catalog[i]
		}
	}
	if refused != nil {
		return *refused, true
	}
	return Tool{}, false
}

// Why is why the tool cannot be run against a box, empty when it can: its
// declaration's problem, else why the box cannot be attached.
func (t Tool) Why(box Sandbox) string {
	if t.Problem != "" {
		return t.Problem
	}
	return AttachWhy([]Sandbox{box})
}

// PickerDetail is what the picker says of a tool beside its label: what it
// is, and that it is running (RunningTools).
func (t Tool) PickerDetail(running bool) string {
	if running {
		return t.Detail + " · running"
	}
	return t.Detail
}

// File is one of the files the tool carries, by its name.
func (t Tool) File(name string) (ToolFile, bool) {
	for _, f := range t.Files {
		if f.Name == name {
			return f, true
		}
	}
	return ToolFile{}, false
}

// FilesWhy is why the tool's files cannot be edited, empty when they can.
func (t Tool) FilesWhy() string {
	if len(t.Files) == 0 {
		return t.Label + " carries no config"
	}
	return ""
}

// ToolFileDetail is what a tool file's row says of it: where it lands inside
// a discobox, and where the copy being edited is on this machine (path, from
// DataSource.ToolFilePath; empty for none) — the second because a file only
// reachable through a console is one you cannot put in your dotfiles.
func ToolFileDetail(file ToolFile, path string) string {
	detail := "→ ~/" + file.Home
	if path != "" {
		detail += "  ·  " + path
	}
	return detail
}

// ToolEdited is what the status line says of a tool file edited: a change is
// worth a sentence, because what it did not do is the surprising half — the
// discoboxes already carrying the file keep the copy they have, and this is
// what the next one gets.
func ToolEdited(file ToolFile, changed bool) string {
	if !changed {
		return file.Name + " unchanged"
	}
	return file.Name + " saved — the next discobox to open " + file.Tool + " gets it"
}

// OfferedTool reads a box's tools again and is the one an id names, refusing
// as the picker would: a tool no longer declared, a declaration that cannot
// run, or a box that cannot be attached. Every console runs a tool through it
// (or RunHostTool, which does), never on the strength of a catalog it read
// before.
func OfferedTool(ctx context.Context, src DataSource, box Sandbox, id string) (Tool, error) {
	if why := AttachWhy([]Sandbox{box}); why != "" {
		return Tool{}, fmt.Errorf("%s: %s", id, why)
	}
	catalog, err := src.Tools(ctx, box.ID)
	if err != nil {
		return Tool{}, fmt.Errorf("%s: %w", id, err)
	}
	t, ok := ToolIn(catalog, id)
	switch {
	case !ok:
		return Tool{}, fmt.Errorf("no such tool: %s", id)
	case t.Problem != "":
		return Tool{}, fmt.Errorf("%s: %s", t.Label, t.Problem)
	}
	return t, nil
}

// OpenTool is a session of one of a box's tools that runs in it, as the
// picker offers it (OfferedTool): the tool's live session when the box is
// running one (RunningTools), attached, and otherwise one made for it
// (DataSource.NewTool) — choosing a tool that is running is showing it,
// never a second session. A tool that runs on this machine has none
// (RunHostTool).
func OpenTool(ctx context.Context, src DataSource, box Sandbox, id string, cols, rows int) (Tool, Exec, Terminal, error) {
	t, err := OfferedTool(ctx, src, box, id)
	if err != nil {
		return Tool{}, Exec{}, nil, err
	}
	if t.Host {
		return Tool{}, Exec{}, nil, fmt.Errorf("%s runs on this machine, not in the discobox", t.Label)
	}
	execs, err := src.Execs(ctx, box.ID)
	if err != nil {
		return Tool{}, Exec{}, nil, fmt.Errorf("%s: %w", t.Label, err)
	}
	if e, ok := RunningTools(execs)[t.ID]; ok {
		term, err := src.OpenExec(ctx, box.ID, e.ID, cols, rows)
		return t, e, term, err
	}
	e, term, err := src.NewTool(ctx, box.ID, t.ID, cols, rows)
	return t, e, term, err
}

// RunHostTool hands a box to one of its tools that runs on this machine, as
// the picker offers it (OfferedTool), and is what the status line says of it.
// A failure carries the tool's id, which is the name of the `discobox tools`
// command that ran it: how someone runs it again by hand to read the whole
// error a status line had to cut.
func RunHostTool(ctx context.Context, src DataSource, box Sandbox, id string) (string, error) {
	t, err := OfferedTool(ctx, src, box, id)
	if err != nil {
		return "", err
	}
	if !t.Host {
		return "", fmt.Errorf("%s runs in the discobox, not on this machine", t.Label)
	}
	if err := src.RunHostTool(ctx, box.ID, t.ID); err != nil {
		return "", fmt.Errorf("%s: %w", t.ID, err)
	}
	return "opened " + box.Name + " in " + t.Label, nil
}

// EditTool has one of a tool's files edited in the person's editor, through
// n, as DataSource.EditToolFile does, and is what the status line says of it.
// The tool is read again from the box's catalog, so a file is one a
// declaration carries now; a box need not be up to edit it, since the copy
// is this machine's. An edit given up is context.Canceled.
func EditTool(ctx context.Context, src DataSource, box Sandbox, id, name string, n *Narration) (string, error) {
	catalog, err := src.Tools(ctx, box.ID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", id, err)
	}
	t, ok := ToolIn(catalog, id)
	if !ok {
		return "", fmt.Errorf("no such tool: %s", id)
	}
	if why := t.FilesWhy(); why != "" {
		return "", errors.New(why)
	}
	file, ok := t.File(name)
	if !ok {
		return "", fmt.Errorf("%s carries no file %q", t.Label, name)
	}
	changed, err := src.EditToolFile(ctx, file, n)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		return "", fmt.Errorf("edit %s: %w", file.Name, err)
	}
	return ToolEdited(file, changed), nil
}

// AddressOffer is one of a box's addresses as the picker offers it to copy:
// what it is called, the address, or why there is none.
type AddressOffer struct {
	Label string
	Value string
	Why   string
}

// The address offers' labels.
const (
	AddressSSH = "ssh"
	AddressGit = "git url"
)

// AddressOffers are a box's two addresses as the picker offers them: each
// with its value, or why it has none — the box cannot be reached, the lookup
// failed (err), or it came back without that half (a box whose source never
// landed). known is whether a lookup has answered; one still running is
// "looking it up…".
func AddressOffers(box Sandbox, addr Addresses, err error, known bool) []AddressOffer {
	unreachable := AttachWhy([]Sandbox{box})
	offer := func(label, value string) AddressOffer {
		o := AddressOffer{Label: label}
		switch {
		case unreachable != "":
			o.Why = unreachable
		case value != "":
			o.Value = value
		case err != nil:
			o.Why = err.Error()
		case !known:
			o.Why = "looking it up…"
		default:
			o.Why = "nothing to reach it by"
		}
		return o
	}
	return []AddressOffer{offer(AddressSSH, addr.SSH), offer(AddressGit, addr.Git)}
}
