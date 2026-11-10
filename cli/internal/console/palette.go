package console

import (
	"cmp"
	"slices"
	"strings"
)

// The palette's results (ADR 26-11-06-671 §2, §5, §6): what a query finds
// among the boxes every listed server has and the registry's commands, in the
// order the palette lists them. Both consoles draw these rows; which row is
// picked, and what a key or a press does with it, is each frontend's.

// PaletteQuery is what the palette is asked: the text typed, at most one chip
// (a command or a box), whether archived boxes are in scope, and what ranks an
// empty query — the box first, and the boxes visited, most recent highest.
type PaletteQuery struct {
	Text string
	// Command is the command chip's ID: boxes are evaluated for it.
	Command string
	// Box is the box chip: commands are evaluated for it.
	Box *BoxRef
	// Archived is the palette's own scope, never the list's.
	Archived bool
	// Current is the box the window is on — its workspace, else the list's
	// cursor — which an empty query puts first.
	Current BoxRef
	// Recent is the window's visits, by box: a larger number is a later
	// visit. Navigation records them, not cursor movement (671 §6).
	Recent map[BoxRef]int
}

// PaletteRow is one result: a box or a command, why it cannot be chosen when
// it cannot, and how well it matched.
type PaletteRow struct {
	Box     *Sandbox
	Command *Entry
	// Why is why the row cannot be chosen; a row with one is shown dimmed.
	Why  string
	Rank Rank
}

// Identity is what a row is kept by across live updates: a box by its server
// and full ID, a command by its ID — never its place in the list.
func (r PaletteRow) Identity(session Session) string {
	if r.Box != nil {
		return "box:" + session.ServerName(r.Box.Server) + ":" + r.Box.ID
	}
	return "command:" + r.Command.ID
}

// PaletteResult is the rows a query finds, in order, and the scope they were
// found in.
type PaletteResult struct {
	Rows []PaletteRow
	// Archived is whether archived boxes were searched: asked for, or a chip
	// that acts only on them (ChipShowsArchived).
	Archived bool
}

// ChipShowsArchived reports whether a command chip brings archived boxes into
// the palette's scope: the commands that act only on archived boxes (671 §5).
func ChipShowsArchived(id string) bool { return id == "unarchive" || id == "purge" }

// PaletteCommit is the command a query commits as its chip as it is typed or
// pasted (671 §2): a first word that is exactly a single-word alias of a box
// command, case aside, followed by a space. Rest is what follows that space,
// the chip's query. An ID-shaped first word is never a command.
func PaletteCommit(text string) (id, rest string, ok bool) {
	first, rest, spaced := strings.Cut(text, " ")
	if !spaced || strings.HasPrefix(first, "sbx_") {
		return "", "", false
	}
	for _, c := range registry {
		if c.Target != TargetBox {
			continue
		}
		for _, a := range c.Aliases {
			if !strings.Contains(a, " ") && strings.EqualFold(first, a) {
				return c.ID, rest, true
			}
		}
	}
	return "", "", false
}

// Palette is what a query finds (671 §5–6), for a window in this context:
// with no chip, boxes and commands; with a command chip, the boxes evaluated
// for it; with a box chip, the commands that take a box, evaluated for it.
// When anything can be chosen, what cannot is left out; when nothing can, what
// matched is kept, with why. Usage is the registry's, this window's.
func (r *Registry) Palette(l *List, ctx Context, q PaletteQuery) PaletteResult {
	session := l.Session
	ref := func(s Sandbox) BoxRef { return BoxRef{Server: session.ServerName(s.Server), ID: s.ID} }
	archived := q.Archived || ChipShowsArchived(q.Command)
	query := ParseQuery(q.Text)
	var rows []PaletteRow
	if q.Box == nil {
		for _, hit := range l.Search(query, archived) {
			s := hit.Box
			why := ""
			if q.Command != "" {
				if e, ok := r.Entry(ctx, q.Command, []Sandbox{s}); ok && !e.Enabled {
					why = e.Why
				}
			}
			rows = append(rows, PaletteRow{Box: &s, Rank: hit.Rank, Why: why})
		}
	}
	if q.Command == "" {
		var box *Sandbox
		var targets []Sandbox
		if q.Box != nil {
			box = l.Find(*q.Box)
			if box != nil {
				targets = []Sandbox{*box}
			}
		}
		for _, c := range r.Entries(ctx, targets) {
			if q.Box != nil && c.Target != TargetBox {
				continue
			}
			rank, ok := MatchFields(query, append([]string{c.Label}, c.Aliases...), []string{c.Detail})
			if !ok {
				continue
			}
			why := ""
			switch {
			case q.Box != nil && box == nil:
				why = "the box is no longer listed"
			case q.Box != nil && !c.Enabled, c.Target != TargetBox && !c.Enabled:
				why = c.Why
			}
			rows = append(rows, PaletteRow{Command: &c, Rank: rank, Why: why})
		}
	}
	if slices.ContainsFunc(rows, func(row PaletteRow) bool { return row.Why == "" }) {
		rows = slices.DeleteFunc(rows, func(row PaletteRow) bool { return row.Why != "" })
	}
	// An ID-shaped query puts boxes first: sbx_, or a run of a box's ID.
	idQuery := strings.HasPrefix(q.Text, "sbx_")
	if !query.Empty() {
		for _, row := range rows {
			if row.Box != nil && strings.Contains(strings.ToLower(row.Box.ID), strings.ToLower(q.Text)) {
				idQuery = true
			}
		}
	}
	usage := func(row PaletteRow) int {
		if row.Box != nil {
			return q.Recent[ref(*row.Box)]
		}
		return r.Usage(row.Command.ID)
	}
	slices.SortStableFunc(rows, func(a, b PaletteRow) int {
		if query.Empty() {
			// The box the window is on, the boxes visited, the commands,
			// then the boxes never visited in the list's order.
			priority := func(row PaletteRow) int {
				switch {
				case row.Box == nil:
					return 2
				case ref(*row.Box) == q.Current:
					return 0
				case q.Recent[ref(*row.Box)] > 0:
					return 1
				}
				return 3
			}
			if n := cmp.Compare(priority(a), priority(b)); n != 0 {
				return n
			}
		} else {
			priority := func(row PaletteRow) int {
				switch {
				case idQuery && row.Box != nil, !idQuery && row.Command != nil && row.Rank.Tier == 0:
					return 0
				}
				return 1
			}
			if n := cmp.Compare(priority(a), priority(b)); n != 0 {
				return n
			}
			if n := a.Rank.Compare(b.Rank); n != 0 {
				return n
			}
		}
		if n := cmp.Compare(usage(b), usage(a)); n != 0 {
			return n
		}
		if query.Empty() {
			return 0
		}
		label := func(row PaletteRow) string {
			if row.Box != nil {
				if row.Box.Name != "" {
					return row.Box.Name
				}
				return row.Box.ID
			}
			return row.Command.Label
		}
		if n := strings.Compare(label(a), label(b)); n != 0 {
			return n
		}
		return strings.Compare(a.Identity(session), b.Identity(session))
	})
	return PaletteResult{Rows: rows, Archived: archived}
}
