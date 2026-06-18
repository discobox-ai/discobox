// Package matcher maps watcher file changes to file hooks.
package matcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	hooks "github.com/obot-platform/discobox/hooks"
	"github.com/obot-platform/discobox/hooks/watcher"
)

// Options configures hook matching.
type Options struct {
	// DisableGitIgnore skips the git check-ignore policy step. This is intended
	// for unit tests and callers that have already applied the authoritative git
	// ignore policy.
	DisableGitIgnore bool
	// GitTimeout bounds git metadata calls. A small default is used when unset.
	GitTimeout time.Duration
}

// Result is the deterministic output of matching one file-change batch.
type Result struct {
	Matches []MatchedHook `json:"matches"`
	Skipped []SkippedPath `json:"skipped,omitempty"`
}

// MatchedHook contains one hook and the changed files that should trigger it.
type MatchedHook struct {
	Hook    hooks.Hook       `json:"hook"`
	HookID  string           `json:"hook_id"`
	Changes []watcher.Change `json:"changes"`
}

// SkippedPath records why a changed path was removed before hook matching.
type SkippedPath struct {
	Path   string             `json:"path"`
	Kind   watcher.ChangeKind `json:"kind"`
	Reason SkipReason         `json:"reason"`
}

// SkipReason describes a matcher diagnostic skip reason.
type SkipReason string

const (
	SkipEmptyPath     SkipReason = "empty_path"
	SkipOutsideRepo   SkipReason = "outside_repo"
	SkipGitIgnored    SkipReason = "git_ignored"
	SkipGlobalIgnored SkipReason = "global_ignored"
)

const defaultGitTimeout = 5 * time.Second

// Match returns file hooks affected by changes, after git-ignore, global-ignore,
// hook pattern, and hook ignore/exclude filtering. Hook order follows the input
// hook order (the parser returns deterministic order); per-hook changes are
// sorted by path and then change kind.
func Match(repoRoot string, hookDefs []hooks.Hook, changes []watcher.Change, globalIgnore []string, opts Options) (*Result, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}

	normalized, skipped := normalizeChanges(repoRoot, changes)
	if len(normalized) == 0 {
		return &Result{Skipped: skipped}, nil
	}

	if !opts.DisableGitIgnore {
		ignored, err := GitIgnored(repoRoot, changePaths(normalized), opts.GitTimeout)
		if err != nil {
			return nil, err
		}
		kept := normalized[:0]
		for _, ch := range normalized {
			if ignored[ch.Path] {
				skipped = append(skipped, SkippedPath{Path: ch.Path, Kind: ch.Kind, Reason: SkipGitIgnored})
				continue
			}
			kept = append(kept, ch)
		}
		normalized = kept
	}

	if len(globalIgnore) > 0 {
		kept := normalized[:0]
		for _, ch := range normalized {
			ignored, err := matchAny(globalIgnore, ch.Path)
			if err != nil {
				return nil, fmt.Errorf("global ignore: %w", err)
			}
			if ignored {
				skipped = append(skipped, SkippedPath{Path: ch.Path, Kind: ch.Kind, Reason: SkipGlobalIgnored})
				continue
			}
			kept = append(kept, ch)
		}
		normalized = kept
	}

	result := &Result{Skipped: skipped}
	for _, hook := range hookDefs {
		if !hook.AppliesToFiles() || strings.TrimSpace(hook.Pattern) == "" {
			continue
		}
		matched := make([]watcher.Change, 0)
		for _, ch := range normalized {
			ok, err := matchPattern(hook.Pattern, ch.Path)
			if err != nil {
				return nil, fmt.Errorf("hook %s pattern %q: %w", hook.ID, hook.Pattern, err)
			}
			if !ok {
				continue
			}
			ignored, err := matchAny(hook.Ignore, ch.Path)
			if err != nil {
				return nil, fmt.Errorf("hook %s ignore: %w", hook.ID, err)
			}
			if ignored {
				continue
			}
			matched = append(matched, ch)
		}
		if len(matched) == 0 {
			continue
		}
		sortChanges(matched)
		result.Matches = append(result.Matches, MatchedHook{Hook: hook, HookID: hook.ID, Changes: matched})
	}
	return result, nil
}

func normalizeChanges(repoRoot string, changes []watcher.Change) ([]watcher.Change, []SkippedPath) {
	out := make([]watcher.Change, 0, len(changes))
	skipped := make([]SkippedPath, 0)
	for _, ch := range changes {
		path, ok := normalizePath(repoRoot, ch.Path)
		if !ok {
			reason := SkipOutsideRepo
			if strings.TrimSpace(ch.Path) == "" || path == "" {
				reason = SkipEmptyPath
			}
			skipped = append(skipped, SkippedPath{Path: filepath.ToSlash(ch.Path), Kind: ch.Kind, Reason: reason})
			continue
		}
		ch.Path = path
		if ch.Entry != nil {
			entry := *ch.Entry
			entry.Path = path
			ch.Entry = &entry
		}
		out = append(out, ch)
	}
	return out, skipped
}

func normalizePath(repoRoot, p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false
	}
	var rel string
	if filepath.IsAbs(p) {
		r, err := filepath.Rel(repoRoot, p)
		if err != nil {
			return filepath.ToSlash(p), false
		}
		rel = r
	} else {
		rel = p
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return rel, false
	}
	return rel, true
}

func changePaths(changes []watcher.Change) []string {
	paths := make([]string, len(changes))
	for i, ch := range changes {
		paths[i] = ch.Path
	}
	return paths
}

func sortChanges(changes []watcher.Change) {
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].Path == changes[j].Path {
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].Path < changes[j].Path
	})
}

func matchAny(patterns []string, path string) (bool, error) {
	for _, pattern := range patterns {
		ok, err := matchPattern(pattern, path)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func matchPattern(pattern, path string) (bool, error) {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" {
		return false, nil
	}
	path = filepath.ToSlash(path)
	ok, err := doublestar.Match(pattern, path)
	if err != nil || ok {
		return ok, err
	}
	// Discobot/picomatch-compatible convenience: a trailing /** pattern also
	// matches the directory itself.
	if strings.HasSuffix(pattern, "/**") {
		dir := strings.TrimSuffix(pattern, "/**")
		return doublestar.Match(dir, path)
	}
	return false, nil
}

// GitIgnored returns paths ignored by Git ignore rules for repoRoot.
func GitIgnored(repoRoot string, paths []string, timeout time.Duration) (map[string]bool, error) {
	if timeout <= 0 {
		timeout = defaultGitTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := ensureGitWorktree(ctx, repoRoot); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "check-ignore", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("git check-ignore timed out: %w", ctx.Err())
			}
			return nil, fmt.Errorf("git check-ignore: %w%s", err, formatStderr(stderr.String()))
		}
	}

	ignored := make(map[string]bool)
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = filepath.ToSlash(strings.TrimSpace(line))
		if line != "" {
			ignored[line] = true
		}
	}
	return ignored, nil
}

func ensureGitWorktree(ctx context.Context, repoRoot string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--is-inside-work-tree")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("git rev-parse timed out: %w", ctx.Err())
		}
		return fmt.Errorf("git worktree required: %w%s", err, formatStderr(stderr.String()))
	}
	if strings.TrimSpace(stdout.String()) != "true" {
		return fmt.Errorf("git worktree required: %s is not inside a git worktree", repoRoot)
	}
	return nil
}

func formatStderr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return ": " + s
}
