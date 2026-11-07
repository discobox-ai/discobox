package tui

import (
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The tags are the narrowest of the header filter's three (filter.go), after
// the server and the folder: they narrow the list to the discoboxes carrying
// every tag chosen, the way `discobox ls --tag` does when it is repeated. A
// discobox's tags are its own, in the meta file inside it, and the rows show
// the copy the server last heard (ADR 0136). The filter matches a tag as the
// row spells it — `wip`, or `ticket=ENG-12` — so what you pick is what you see
// after the name.
//
// They are only offered once there is a tag to pick: a project nobody tags has
// no use for a choice that can only say "all tags". They are not offered over
// the harnesses and secrets screens, which list no discoboxes.

// allTags is the choice that is not a tag: every discobox, tagged or not.
const allTags = "all tags"

// tagged reports whether a discobox carries every tag the list is filtered
// to. Every discobox carries none at all.
func (l *sandboxList) tagged(s console.Sandbox) bool {
	for _, tag := range l.tags {
		if !slices.Contains(s.Tags, tag) {
			return false
		}
	}
	return true
}

// toggleTag chooses a tag, or lets go of it when it is chosen already. The
// chosen tags are kept in order, so two lists holding the same ones are equal.
func (l *sandboxList) toggleTag(tag string) {
	if i := slices.Index(l.tags, tag); i >= 0 {
		l.tags = slices.Delete(slices.Clone(l.tags), i, i+1)
		return
	}
	l.addTag(tag)
}

// addTag chooses a tag alongside the ones chosen already, in place of any
// other value of its key: a discobox's tags are a map, one value to a key and
// a bare `key` its empty one (sandboxmeta.TagStrings), so no discobox carries
// `ticket=ENG-12` and `ticket=ENG-13` both, and marking the two would list
// nothing.
func (l *sandboxList) addTag(tag string) {
	if slices.Contains(l.tags, tag) {
		return
	}
	key := console.TagKey(tag)
	// A fresh slice: the filter card works on a copy of the list, and
	// appending into a shared backing array would mark the live one too.
	l.tags = slices.DeleteFunc(slices.Clone(l.tags), func(t string) bool { return console.TagKey(t) == key })
	l.tags = append(l.tags, tag)
	slices.Sort(l.tags)
}

// tagChoices are what the filter can narrow to: every tag the discoboxes
// inside the other two filters carry, in order, and the chosen ones none of
// them carries any longer — the way the folders keep the one chosen, so a
// choice does not vanish from under the list it is showing.
func (l *sandboxList) tagChoices() []string {
	seen := map[string]bool{}
	var out []string
	// The folder's own discoboxes, not the ones nested there under a creator:
	// a tag only those carry would narrow the list to nothing, since the
	// creator they are drawn under does not carry it.
	for _, s := range l.all {
		if !l.onServer(s) || !l.folder.Holds(s, l.session) {
			continue
		}
		if !l.showArchived && s.State == console.StateArchived {
			continue
		}
		for _, tag := range s.Tags {
			if !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
		}
	}
	slices.Sort(out)
	for _, tag := range l.tags {
		if !seen[tag] {
			out = append(out, tag)
		}
	}
	return out
}

// tagLabel is how a tag reads in the header and on the filter's card: the tag
// as the rows draw it, so the eye matches the one to the other.
func tagLabel(tag string) string {
	if tag == "" {
		return allTags
	}
	return "#" + tag
}

// tagsLabel is how the chosen tags read in the header, each as the rows draw
// it.
func tagsLabel(tags []string) string {
	labels := make([]string, len(tags))
	for i, tag := range tags {
		labels[i] = tagLabel(tag)
	}
	return strings.Join(labels, " ")
}
