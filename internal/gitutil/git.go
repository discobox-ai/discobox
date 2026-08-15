package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type StatusChange struct {
	Path    string
	Deleted bool
}

// Tracer observes every git command this package runs, so a caller that wants
// to show its work — `disco apply --debug` — can print the real commands
// instead of a paraphrase. Args are already redacted (see redactArg).
type Tracer func(dir string, args []string)

type tracerKey struct{}

// WithTracer returns a context whose git commands are reported to tracer.
func WithTracer(ctx context.Context, tracer Tracer) context.Context {
	if tracer == nil {
		return ctx
	}
	return context.WithValue(ctx, tracerKey{}, tracer)
}

func trace(ctx context.Context, dir string, args []string) {
	tracer, _ := ctx.Value(tracerKey{}).(Tracer)
	if tracer == nil {
		return
	}
	redacted := make([]string, len(args))
	for i, arg := range args {
		redacted[i] = redactArg(arg)
	}
	tracer(dir, redacted)
}

// redactArg strips credentials out of a traced argument. Fetch and push against
// the sandbox git proxy carry a bearer token in an http.extraHeader config
// argument, and remote URLs can carry userinfo; neither belongs in output a
// user may paste into a bug report. Redacting here rather than at each call
// site means no new git command can leak by forgetting to.
func redactArg(arg string) string {
	if strings.HasPrefix(arg, "http.extraHeader=") {
		name, _, ok := strings.Cut(strings.TrimPrefix(arg, "http.extraHeader="), ":")
		if !ok {
			return "http.extraHeader=[REDACTED]"
		}
		return "http.extraHeader=" + name + ": [REDACTED]"
	}
	if scheme, rest, ok := strings.Cut(arg, "://"); ok && strings.Contains(rest, "@") {
		userinfo, host, _ := strings.Cut(rest, "@")
		if name, _, hasPassword := strings.Cut(userinfo, ":"); hasPassword {
			return scheme + "://" + name + ":[REDACTED]@" + host
		}
	}
	return arg
}

// Commit is one commit as reported by Log.
type Commit struct {
	SHA     string    `json:"sha"`
	Subject string    `json:"subject"`
	Author  string    `json:"author"`
	Date    time.Time `json:"date"`
}

// Log lists the commits in revRange (any `git log` range, e.g. "base..tip"),
// oldest first — the order they would be replayed in.
func Log(ctx context.Context, repoRoot, revRange string) ([]Commit, error) {
	// %x1f/%x1e separate fields and records: subjects and author names contain
	// anything, including tabs and newlines, so neither can be the delimiter.
	out, err := Output(ctx, repoRoot, nil, nil, "log", "--reverse", "--format=%H%x1f%an%x1f%aI%x1f%s%x1e", revRange)
	if err != nil {
		return nil, fmt.Errorf("list commits in %s: %w", revRange, err)
	}
	var commits []Commit
	for _, record := range strings.Split(out, "\x1e") {
		record = strings.TrimLeft(record, "\r\n")
		if strings.TrimSpace(record) == "" {
			continue
		}
		fields := strings.Split(record, "\x1f")
		if len(fields) != 4 {
			continue
		}
		commit := Commit{SHA: fields[0], Author: fields[1], Subject: fields[3]}
		if at, err := time.Parse(time.RFC3339, fields[2]); err == nil {
			commit.Date = at
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

type WorkspaceTree struct {
	BaseCommit string
	BaseTree   string
	Tree       string
	Dirty      bool
}

func Output(ctx context.Context, dir string, stdin []byte, extraEnv map[string]string, args ...string) (string, error) {
	trace(ctx, dir, args)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Env = os.Environ()
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, trimmed)
	}
	return string(out), nil
}

func Root(ctx context.Context, dir string) (string, error) {
	out, err := Output(ctx, dir, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve git root: %w", err)
	}
	return filepath.Clean(strings.TrimSpace(out)), nil
}

func ResolveCommit(ctx context.Context, repoRoot, rev string) (string, error) {
	out, err := Output(ctx, repoRoot, nil, nil, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve git ref %q: %w", rev, err)
	}
	return strings.TrimSpace(out), nil
}

func CurrentBranch(ctx context.Context, repoRoot string) (string, bool) {
	out, err := Output(ctx, repoRoot, nil, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", false
	}
	branch := strings.TrimSpace(out)
	return branch, branch != ""
}

// ConfigValue reads one config key with git's own resolution, run from dir: a
// repository-local value wins over the global one, which is how work-versus-
// personal identity is normally separated. An empty dir uses the process
// working directory.
//
// Unset is not an error -- `git config --get` simply exits 1 -- so this returns
// an ok bool like CurrentBranch rather than an error nobody would act on. git
// is the authority on whether a key is configured; a caller that gets false
// must leave the value absent rather than substitute one of its own.
func ConfigValue(ctx context.Context, dir, key string) (string, bool) {
	out, err := Output(ctx, dir, nil, nil, "config", "--get", key)
	if err != nil {
		return "", false
	}
	value := strings.TrimSpace(out)
	return value, value != ""
}

func StatusChanges(ctx context.Context, repoRoot string) ([]StatusChange, error) {
	out, err := Output(ctx, repoRoot, nil, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("read git status: %w", err)
	}
	entries := bytes.Split([]byte(out), []byte{0})
	changes := make([]StatusChange, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		entry := string(entries[i])
		if entry == "" || len(entry) < 4 {
			continue
		}
		status := entry[:2]
		path := filepath.ToSlash(strings.TrimSpace(entry[3:]))
		if status[0] == 'R' || status[0] == 'C' {
			oldPath := ""
			if i+1 < len(entries) {
				oldPath = filepath.ToSlash(strings.TrimSpace(string(entries[i+1])))
				i++
			}
			if status[0] == 'R' && oldPath != "" {
				changes = append(changes, StatusChange{Path: oldPath, Deleted: true})
			}
			if path != "" {
				changes = append(changes, StatusChange{Path: path})
			}
			continue
		}
		if path == "" {
			continue
		}
		changes = append(changes, StatusChange{Path: path, Deleted: status[0] == 'D' || status[1] == 'D'})
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path == changes[j].Path {
			return !changes[i].Deleted && changes[j].Deleted
		}
		return changes[i].Path < changes[j].Path
	})
	return changes, nil
}

func CurrentWorkspaceTree(ctx context.Context, repoRoot string) (WorkspaceTree, func(), error) {
	baseCommit, err := ResolveCommit(ctx, repoRoot, "HEAD")
	if err != nil {
		return WorkspaceTree{}, func() {}, err
	}
	baseTreeOut, err := Output(ctx, repoRoot, nil, nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return WorkspaceTree{}, func() {}, fmt.Errorf("resolve HEAD tree: %w", err)
	}
	baseTree := strings.TrimSpace(baseTreeOut)
	tempDir, err := os.MkdirTemp("", "discobox-git-index-*")
	if err != nil {
		return WorkspaceTree{}, func() {}, fmt.Errorf("create temporary git index directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	indexFile, err := os.CreateTemp(tempDir, "index-*")
	if err != nil {
		cleanup()
		return WorkspaceTree{}, func() {}, fmt.Errorf("create temporary git index: %w", err)
	}
	indexPath := indexFile.Name()
	_ = indexFile.Close()
	env := map[string]string{"GIT_INDEX_FILE": indexPath}
	if _, err := Output(ctx, repoRoot, nil, env, "read-tree", "HEAD"); err != nil {
		cleanup()
		return WorkspaceTree{}, func() {}, fmt.Errorf("read HEAD into temporary index: %w", err)
	}
	changes, err := StatusChanges(ctx, repoRoot)
	if err != nil {
		cleanup()
		return WorkspaceTree{}, func() {}, err
	}
	for _, change := range changes {
		if change.Deleted {
			if _, err := Output(ctx, repoRoot, nil, env, "rm", "--cached", "--ignore-unmatch", "--", change.Path); err != nil {
				cleanup()
				return WorkspaceTree{}, func() {}, fmt.Errorf("remove deleted path %s from temporary index: %w", change.Path, err)
			}
			continue
		}
		if _, err := Output(ctx, repoRoot, nil, env, "add", "--", change.Path); err != nil {
			cleanup()
			return WorkspaceTree{}, func() {}, fmt.Errorf("add path %s to temporary index: %w", change.Path, err)
		}
	}
	treeOut, err := Output(ctx, repoRoot, nil, env, "write-tree")
	if err != nil {
		cleanup()
		return WorkspaceTree{}, func() {}, fmt.Errorf("write current workspace tree: %w", err)
	}
	tree := strings.TrimSpace(treeOut)
	return WorkspaceTree{
		BaseCommit: baseCommit,
		BaseTree:   baseTree,
		Tree:       tree,
		Dirty:      tree != "" && tree != baseTree,
	}, cleanup, nil
}

func CommitTree(ctx context.Context, repoRoot, tree, parent, message string) (string, error) {
	args := []string{"commit-tree", tree}
	if strings.TrimSpace(parent) != "" {
		args = append(args, "-p", parent)
	}
	env := map[string]string{
		"GIT_AUTHOR_NAME":     "Discobox",
		"GIT_AUTHOR_EMAIL":    "discobox@example.invalid",
		"GIT_COMMITTER_NAME":  "Discobox",
		"GIT_COMMITTER_EMAIL": "discobox@example.invalid",
	}
	out, err := Output(ctx, repoRoot, []byte(message), env, args...)
	if err != nil {
		return "", fmt.Errorf("create git commit from tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func UpdateRef(ctx context.Context, repoRoot, ref, commit string) error {
	if _, err := Output(ctx, repoRoot, nil, nil, "update-ref", ref, commit); err != nil {
		return fmt.Errorf("update git ref %q: %w", ref, err)
	}
	return nil
}
