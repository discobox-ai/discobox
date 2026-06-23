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
)

type StatusChange struct {
	Path    string
	Deleted bool
}

type WorkspaceTree struct {
	BaseCommit string
	BaseTree   string
	Tree       string
	Dirty      bool
}

func Output(ctx context.Context, dir string, stdin []byte, extraEnv map[string]string, args ...string) (string, error) {
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
