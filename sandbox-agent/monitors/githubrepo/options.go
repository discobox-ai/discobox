package githubrepo

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// config is an instance's options, read.
type config struct {
	Owner, Name string
	// The kinds watched.
	Issues, PRs, Actions bool
	// Ids narrowing a kind; empty watches it whole.
	IssueIDs, PRIDs, RunIDs []int64
	// Head is a branch glob and Author a login, together selecting the PRs
	// that are watched besides PRIDs (both must match when both are given).
	Head, Author string
	// Branch narrows Actions runs to one branch.
	Branch string
}

func (c config) repo() string { return c.Owner + "/" + c.Name }

// filtered reports whether PRs are selected by head or author.
func (c config) filtered() bool { return c.Head != "" || c.Author != "" }

// narrowedPRs reports whether only some PRs are watched.
func (c config) narrowedPRs() bool { return len(c.PRIDs) > 0 || c.filtered() }

// onlyIDs reports whether everything the instance watches is named by id, so
// it is done once each is closed or complete.
func (c config) onlyIDs() bool {
	if c.filtered() {
		return false
	}
	return (!c.Issues || len(c.IssueIDs) > 0) &&
		(!c.PRs || len(c.PRIDs) > 0) &&
		(!c.Actions || len(c.RunIDs) > 0)
}

// wantsIssue reports whether an issue's activity is reported.
func (c config) wantsIssue(n int64) bool {
	return c.Issues && (len(c.IssueIDs) == 0 || slices.Contains(c.IssueIDs, n))
}

// wantsPR reports whether a PR's activity is reported: named, selected by
// head and author, or every PR when neither narrows them.
func (c config) wantsPR(n int64, head, author string) bool {
	if !c.PRs {
		return false
	}
	if !c.narrowedPRs() || slices.Contains(c.PRIDs, n) {
		return true
	}
	return c.selects(head, author)
}

// selects reports whether head and author select a PR.
func (c config) selects(head, author string) bool {
	if !c.filtered() {
		return false
	}
	if c.Head != "" {
		if ok, _ := path.Match(c.Head, head); !ok {
			return false
		}
	}
	return c.Author == "" || strings.EqualFold(c.Author, author)
}

// parseConfig reads an instance's options, refusing what it cannot watch.
func parseConfig(opts map[string]string) (config, error) {
	var c config
	repo := strings.TrimSpace(opts["repo"])
	if !repoName.MatchString(repo) {
		return c, fmt.Errorf("repo must be owner/name, not %q", repo)
	}
	c.Owner, c.Name, _ = strings.Cut(repo, "/")
	var err error
	if c.IssueIDs, err = ids("issue", opts["issue"]); err != nil {
		return c, err
	}
	if c.PRIDs, err = ids("pr", opts["pr"]); err != nil {
		return c, err
	}
	if c.RunIDs, err = ids("run", opts["run"]); err != nil {
		return c, err
	}
	c.Head = strings.TrimSpace(opts["head"])
	if c.Head != "" {
		if _, err := path.Match(c.Head, ""); err != nil {
			return c, fmt.Errorf("head %q is not a glob: %w", c.Head, err)
		}
	}
	c.Author = strings.TrimPrefix(strings.TrimSpace(opts["author"]), "@")
	c.Branch = strings.TrimSpace(opts["branch"])

	watch := strings.TrimSpace(opts["watch"])
	if watch == "" {
		// The ids and filters given choose the kinds; with none, all three.
		c.Issues = len(c.IssueIDs) > 0
		c.PRs = c.narrowedPRs()
		c.Actions = len(c.RunIDs) > 0 || c.Branch != ""
		if !c.Issues && !c.PRs && !c.Actions {
			c.Issues, c.PRs, c.Actions = true, true, true
		}
		return c, nil
	}
	for _, kind := range strings.FieldsFunc(watch, func(r rune) bool { return r == ',' || r == ' ' }) {
		switch kind {
		case "issues":
			c.Issues = true
		case "prs":
			c.PRs = true
		case "actions":
			c.Actions = true
		default:
			return c, fmt.Errorf("watch takes issues, prs and actions, not %q", kind)
		}
	}
	return c, nil
}

func ids(option, raw string) ([]int64, error) {
	var out []int64
	for _, item := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
		n, err := strconv.ParseInt(strings.TrimPrefix(item, "#"), 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%s: %q is not a number", option, item)
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}
