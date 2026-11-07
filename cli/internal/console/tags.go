package console

import (
	"slices"
	"strconv"
	"strings"
)

// TagKey is the key a tag sets, whether or not it gives a value.
func TagKey(tag string) string {
	key, _, _ := strings.Cut(tag, "=")
	return key
}

// A discobox's status tags say where it stands with a person, as the worker
// skills tag it (worker, "Tags"): blocked on somebody, waiting for the owner to
// answer a question on its screen, or ready for a human to merge its pull
// request. They are the tags somebody scans a list of boxes for, so they are
// drawn bold in color rather than dim — red for the two waiting on a person,
// green for ready — and first among a row's tags so they are the last to give
// way (sandboxList.row). `merged` is not among them: the work is done and asks
// nothing of anyone.
const (
	StatusBlocked    = "blocked"
	StatusNeedsInput = "needs-input"
	StatusReady      = "ready"
)

// statusTags are the status tag keys in the order a row draws them: the two
// waiting on a person first, blocked ahead of a question since it is not on
// the screen to be seen, then ready.
var statusTags = []string{StatusBlocked, StatusNeedsInput, StatusReady}

// specialTags are the tag keys a row draws after its status and before the
// rest, in this order: the pull request and the issue, which link to GitHub
// (githubTagPaths) and say what the box is for, the pull request first since
// it is the one most worth opening.
var specialTags = []string{"pr", "issue"}

// Status is the status a discobox carries, the first of statusTags it is
// tagged with, and empty for none.
func (s Sandbox) Status() string {
	for _, v := range statusTags {
		if slices.ContainsFunc(s.Tags, func(tag string) bool { return TagKey(tag) == v }) {
			return v
		}
	}
	return ""
}

// RowTags are the tags in the order a row draws them: its status tags
// (statusTags), then its special ones (specialTags), each group in its own
// fixed order, then the rest as they are kept.
func (s Sandbox) RowTags() []string {
	out := make([]string, 0, len(s.Tags))
	for _, key := range slices.Concat(statusTags, specialTags) {
		for _, tag := range s.Tags {
			if TagKey(tag) == key {
				out = append(out, tag)
			}
		}
	}
	for _, tag := range s.Tags {
		if !IsStatusTag(tag) && !slices.Contains(specialTags, TagKey(tag)) {
			out = append(out, tag)
		}
	}
	return out
}

// IsStatusTag reports whether a tag is a status tag.
func IsStatusTag(tag string) bool {
	return slices.Contains(statusTags, TagKey(tag))
}

// githubTagPaths are the tag keys that number something in the discobox's
// GitHub repository, and where on GitHub that number is: `issue=4` is issue 4
// and `pr=12` is pull request 12.
var githubTagPaths = map[string]string{"issue": "issues", "pr": "pull"}

// TagURL is where a tag points, for one that numbers an issue or a pull
// request in the repository the discobox was cut from, and empty for any other
// tag — including one of those keys on a discobox with no GitHub repository to
// number it in, since `issue=4` alone does not say whose issue 4.
func (s Sandbox) TagURL(tag string) string {
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

// WorkNumber is the issue (`issue=`) or the pull request (`pr=`) the discobox
// is tagged with — the first, when it is tagged with several — for a discobox
// whose repository is on GitHub.
func (s Sandbox) WorkNumber(pull bool) (int, bool) {
	prefix := "issue="
	if pull {
		prefix = "pr="
	}
	for _, tag := range s.Tags {
		if !strings.HasPrefix(tag, prefix) || s.TagURL(tag) == "" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(tag, prefix))
		if err == nil {
			return n, true
		}
	}
	return 0, false
}
