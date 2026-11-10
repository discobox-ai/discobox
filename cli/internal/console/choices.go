package console

import (
	"fmt"
	"slices"
	"strings"
)

// The filter's choices: what the list can be narrowed to, as one card offers
// them (the TUI's filter card, the desktop's filter menu). The server, the
// folder and the tags are one filter (List.Rows), so each choice is counted
// against the others as they stand, and says the filter it makes.

// AllServers is the server choice that is not a server: every discobox the
// window can reach, in its sections. It leads the servers, where AllFolders
// trails the folders, for the one reason either order has — the choice the
// window opens on is the one to lead with, and this is where it opens.
const AllServers = "all servers"

// AllTags is the tag choice that is not a tag: every discobox, tagged or not.
const AllTags = "all tags"

// TagLabel is how a tag reads in a filter and on its card: the tag as the
// rows draw it, so the eye matches the one to the other.
func TagLabel(tag string) string {
	if tag == "" {
		return AllTags
	}
	return "#" + tag
}

// TagsLabel is how the chosen tags read, each as the rows draw it.
func TagsLabel(tags []string) string {
	labels := make([]string, len(tags))
	for i, tag := range tags {
		labels[i] = TagLabel(tag)
	}
	return strings.Join(labels, " ")
}

// ChoiceGroup is which of the filter's three parts a choice is in.
type ChoiceGroup string

// The groups, in the order a card offers them.
const (
	GroupServer ChoiceGroup = "server"
	GroupFolder ChoiceGroup = "folder"
	GroupTag    ChoiceGroup = "tag"
)

// FilterChoice is one way to narrow the list: what it reads as, what is worth
// knowing about it (how many discoboxes it lists alongside the rest of the
// filter), whether it is the one marked, and the filter marking it makes.
// The tags take several marks at once (Several): Unmark is the filter with
// a marked tag let go, which `all tags` has none of — marking another is the
// only way off it.
type FilterChoice struct {
	Group   ChoiceGroup
	Label   string
	Detail  string
	Marked  bool
	Several bool
	// Mark is the filter with this choice marked, in place of whatever its
	// group had marked — among the tags, alongside the others.
	Mark Filter
	// Unmark is the filter with a marked tag let go; it is Mark for every
	// other choice.
	Unmark Filter
}

// FilterChoices are the filter's choices, group by group: the servers when
// the window lists more than one, the folders on the server marked, and the
// tags inside both once anything there is tagged.
func (l *List) FilterChoices() []FilterChoice {
	out := l.ServerChoices(true)
	for _, f := range append(l.Folders(), EveryFolder) {
		mark := l.Filter
		mark.Folder = f
		detail := plural(l.countWith(mark))
		if f.Key != "" && f.Key == l.Session.Folder().Key {
			detail += " · where this window is running"
		}
		out = append(out, FilterChoice{
			Group: GroupFolder, Label: f.Label, Detail: detail,
			Marked: l.Filter.Folder.Key == f.Key, Mark: mark, Unmark: mark,
		})
	}
	tags := l.TagChoices()
	if len(tags) == 0 {
		return out
	}
	none := l.Filter
	none.Tags = nil
	out = append(out, FilterChoice{
		Group: GroupTag, Label: AllTags, Detail: plural(l.countWith(none)),
		Marked: len(l.Filter.Tags) == 0, Several: true, Mark: none, Unmark: none,
	})
	for _, tag := range tags {
		mark, unmark := l.Filter, l.Filter
		mark.AddTag(tag)
		unmark.ToggleTag(tag)
		if !slices.Contains(l.Filter.Tags, tag) {
			unmark = mark
		}
		detail := plural(l.countWith(mark))
		if key, _, ok := strings.Cut(tag, "="); ok {
			detail += " · " + key + " set to this value"
		}
		out = append(out, FilterChoice{
			Group: GroupTag, Label: TagLabel(tag), Detail: detail,
			Marked: slices.Contains(l.Filter.Tags, tag), Several: true, Mark: mark, Unmark: unmark,
		})
	}
	return out
}

// ServerChoices are the servers a filter can name, when the window lists more
// than one: every server at once first when every is true, then each server,
// the primary first. A server that did not answer says so in place of its
// count.
func (l *List) ServerChoices(every bool) []FilterChoice {
	servers := l.Session.Servers
	if len(servers) <= 1 {
		return nil
	}
	names := servers
	if every {
		names = append([]string{""}, servers...)
	}
	out := make([]FilterChoice, 0, len(names))
	for _, name := range names {
		mark := l.Filter
		mark.Server = name
		label := name
		var detail string
		switch {
		case name == "":
			label = AllServers
			detail = plural(l.countWith(mark)) + " · created on " + servers[0]
		case slices.Contains(l.unreachable, name):
			detail = "not answering"
		case name == servers[0]:
			detail = plural(l.countWith(mark)) + " · the primary"
		default:
			detail = plural(l.countWith(mark))
		}
		out = append(out, FilterChoice{
			Group: GroupServer, Label: label, Detail: detail,
			Marked: l.Filter.Server == name, Mark: mark, Unmark: mark,
		})
	}
	return out
}

// countWith is how many discoboxes the list would draw with filter f, counted
// the way the list counts its rows, on a copy.
func (l *List) countWith(f Filter) int {
	c := *l
	c.Filter = f
	return c.Count()
}

// Narrowed reports whether the filter leaves any discobox out by what it is
// — a server, a folder or a tag — rather than showing every one there is.
// Whether the archived are listed is not narrowing: they are history, out of
// the way until asked for.
func (l *List) Narrowed() bool {
	return l.Filter.Server != "" && len(l.Session.Servers) > 1 || l.Filter.Folder.Key != "" || len(l.Filter.Tags) > 0
}

// FilterLabel is what the list is narrowed to, as a filter says it: the
// server when the window lists more than one, the folder and the tags — and
// nothing about what it leaves open, which the list's own sections name.
func (l *List) FilterLabel() string {
	var parts []string
	if l.Filter.Server != "" && len(l.Session.Servers) > 1 {
		parts = append(parts, "server "+l.Filter.Server)
	}
	if l.Filter.Folder.Key != "" {
		parts = append(parts, l.Filter.Folder.Label)
	}
	if len(l.Filter.Tags) > 0 {
		parts = append(parts, TagsLabel(l.Filter.Tags))
	}
	if !l.Narrowed() {
		return "all discoboxes"
	}
	return strings.Join(parts, " · ")
}

// plural is a count of discoboxes as the list says it.
func plural(n int) string {
	if n == 1 {
		return "1 box"
	}
	return fmt.Sprintf("%d boxes", n)
}
