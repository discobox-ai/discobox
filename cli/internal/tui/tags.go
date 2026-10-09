package tui

import (
	"slices"
	"strings"
)

// The tags are the narrowest of the header filter's three (filter.go), after
// the server and the folder: they narrow the list to the discoboxes carrying
// every tag chosen, the way `discobox list --tag` does when it is repeated. A
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
func (l *sandboxList) tagged(s Sandbox) bool {
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
	key := tagKey(tag)
	// A fresh slice: the filter card works on a copy of the list, and
	// appending into a shared backing array would mark the live one too.
	l.tags = slices.DeleteFunc(slices.Clone(l.tags), func(t string) bool { return tagKey(t) == key })
	l.tags = append(l.tags, tag)
	slices.Sort(l.tags)
}

// tagKey is the key a tag sets, whether or not it gives a value.
func tagKey(tag string) string {
	key, _, _ := strings.Cut(tag, "=")
	return key
}

// tagChoices are what the filter can narrow to: every tag the discoboxes
// inside the other two filters carry, in order, and the chosen ones none of
// them carries any longer — the way the folders keep the one chosen, so a
// choice does not vanish from under the list it is showing.
func (l *sandboxList) tagChoices() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range l.all {
		if !l.onServer(s) || !l.folder.holds(s, l.session) {
			continue
		}
		if !l.showArchived && s.State == StateArchived {
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

// githubTagPaths are the tag keys that number something in the discobox's
// GitHub repository, and where on GitHub that number is: `issue=4` is issue 4
// and `pr=12` is pull request 12.
var githubTagPaths = map[string]string{"issue": "issues", "pr": "pull"}

// tagURL is where a tag points, for one that numbers an issue or a pull
// request in the repository the discobox was cut from, and empty for any other
// tag — including one of those keys on a discobox with no GitHub repository to
// number it in, since `issue=4` alone does not say whose issue 4.
func (s Sandbox) tagURL(tag string) string {
	key, number, ok := strings.Cut(tag, "=")
	where, linked := githubTagPaths[key]
	if !ok || !linked || s.Repository == "" || !githubNumber(number) {
		return ""
	}
	return s.Repository + "/" + where + "/" + number
}

// githubNumber reports whether value can be an issue or pull request number:
// a positive decimal, written without a leading zero.
func githubNumber(value string) bool {
	if value == "" || value[0] == '0' {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
