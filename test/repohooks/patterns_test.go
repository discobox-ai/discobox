// Package repohooks_test checks this repository's own hooks in
// .discobox/hooks the way the hook runner reads them: that every file a
// file hook's pattern names is still a file the repository has, so moving
// what a hook watches cannot silently disable it, or part of it.
package repohooks_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/hooks"
	"github.com/discobox-ai/hooks/matcher"
	"github.com/discobox-ai/hooks/parser"
	"github.com/discobox-ai/hooks/watcher"
)

// untrackedInputs names hooks that watch files no commit ever holds.
var untrackedInputs = map[string]bool{
	"wnb-failure-reports": true, // wnb-*-failed.txt, written by the dev loop
}

// TestEveryFileHookPatternMatchesATrackedFile loads .discobox/hooks with the
// hook runner's own parser and matcher and requires each file hook's pattern
// to fire on at least one tracked file, and so each entry of it that names a
// path — an entry of a pattern written "{a,b,...}" whose first segment is a
// directory or file name, not a wildcard. Checking only the whole pattern
// would pass while one entry still matched, and a hook that lists its inputs
// one by one would lose any other of them unnoticed. An entry that starts
// with a wildcard ("**/*.bash", "Dockerfile*") names a kind of file, which the
// repository may have none of, and only the whole pattern is held to it.
func TestEveryFileHookPatternMatchesATrackedFile(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	d, err := parser.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "git", "-C", root, "ls-files").Output()
	if err != nil {
		t.Fatal(err)
	}
	var changes []watcher.Change
	for _, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		changes = append(changes, watcher.Change{Path: filepath.Join(root, p), Kind: watcher.Modified})
	}
	checked := 0
	for _, h := range d.Hooks {
		if !h.AppliesToFiles() || strings.TrimSpace(h.Pattern) == "" || untrackedInputs[h.ID] {
			continue
		}
		checked++
		entries := []string{h.Pattern}
		for _, e := range alternatives(h.Pattern) {
			if first, _, _ := strings.Cut(e, "/"); !strings.ContainsAny(first, "*?[{") && e != h.Pattern {
				entries = append(entries, e)
			}
		}
		for _, e := range entries {
			one := h
			one.Pattern = e
			res, err := matcher.Match(root, []hooks.Hook{one}, changes, d.GlobalIgnore, matcher.Options{DisableGitIgnore: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Matches) == 0 {
				t.Errorf("hook %s (%s): %q in pattern %q matches no tracked file", h.ID, h.Name, e, h.Pattern)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no file hooks in .discobox/hooks")
	}
}

// alternatives splits a pattern written as one brace group, "{a,b,...}",
// into its top-level entries; any other pattern is its own only entry.
func alternatives(pattern string) []string {
	if !strings.HasPrefix(pattern, "{") || !strings.HasSuffix(pattern, "}") {
		return []string{pattern}
	}
	var alts []string
	depth, start := 0, 1
	for i, r := range pattern {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 && i != len(pattern)-1 {
				// "{a}/{b}": the first group closes before the end.
				return []string{pattern}
			}
		case ',':
			if depth == 1 {
				alts = append(alts, pattern[start:i])
				start = i + 1
			}
		}
	}
	return append(alts, pattern[start:len(pattern)-1])
}
