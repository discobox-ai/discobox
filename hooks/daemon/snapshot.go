package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/hooks/models"
	"github.com/discobox-ai/discobox/hooks/store"
	"github.com/discobox-ai/discobox/hooks/watcher"
)

const defaultSnapshotMaxFileBytes int64 = 1 << 20

type workspaceSnapshotCandidate struct {
	path   string
	kind   watcher.ChangeKind
	status string
}

func (r *runtimeState) captureWorkspaceSnapshot(ctx context.Context) (*store.WorkspaceSnapshot, error) {
	baseCommit := gitOutput(ctx, r.cfg.RepoRoot, "rev-parse", "HEAD")
	baseTree := ""
	if baseCommit != "" {
		baseTree = gitOutput(ctx, r.cfg.RepoRoot, "rev-parse", "HEAD^{tree}")
	}
	if baseCommit == "" || baseTree == "" {
		return nil, nil
	}
	candidates, err := workspaceSnapshotCandidates(ctx, r.cfg.RepoRoot)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	maxFileBytes := defaultSnapshotMaxFileBytes
	eligible := make([]workspaceSnapshotCandidate, 0, len(candidates))
	omitted := make([]store.SnapshotOmission, 0)
	for _, candidate := range candidates {
		ok, omission := workspaceSnapshotCandidateAllowed(ctx, r.cfg.RepoRoot, candidate, maxFileBytes)
		if ok {
			eligible = append(eligible, candidate)
			continue
		}
		omitted = append(omitted, omission)
	}
	if len(eligible) == 0 {
		return r.recordWorkspaceSnapshotIfChanged(ctx, store.WorkspaceSnapshot{BaseCommit: baseCommit, TreeHash: baseTree, OmittedFiles: omitted, MaxFileBytes: maxFileBytes})
	}

	tempDir, err := r.workspaceSnapshotTempDir()
	if err != nil {
		return nil, err
	}
	captureDir, err := os.MkdirTemp(tempDir, "snapshot-*")
	if err != nil {
		return nil, fmt.Errorf("create workspace snapshot temporary directory: %w", err)
	}
	defer os.RemoveAll(captureDir)

	tmpIndex, err := os.CreateTemp(captureDir, "index-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary git index: %w", err)
	}
	tmpIndexPath := tmpIndex.Name()
	_ = tmpIndex.Close()
	defer os.Remove(tmpIndexPath)

	tmpObjects, err := os.MkdirTemp(captureDir, "objects-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary git objects dir: %w", err)
	}
	defer os.RemoveAll(tmpObjects)

	objectDir := gitOutput(ctx, r.cfg.RepoRoot, "rev-parse", "--git-path", "objects")
	if objectDir == "" {
		return nil, fmt.Errorf("resolve git objects directory")
	}
	if !filepath.IsAbs(objectDir) {
		objectDir = filepath.Join(r.cfg.RepoRoot, objectDir)
	}
	gitEnv := map[string]string{
		"GIT_INDEX_FILE":                   tmpIndexPath,
		"GIT_OBJECT_DIRECTORY":             tmpObjects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": objectDir,
	}
	if _, err := snapshotGitOutput(ctx, r.cfg.RepoRoot, gitEnv, nil, "read-tree", "HEAD"); err != nil {
		return nil, fmt.Errorf("read HEAD into temporary index: %w", err)
	}
	for _, candidate := range eligible {
		if candidate.kind == watcher.Deleted {
			if _, err := snapshotGitOutput(ctx, r.cfg.RepoRoot, gitEnv, nil, "rm", "--cached", "--ignore-unmatch", "--", candidate.path); err != nil {
				return nil, fmt.Errorf("remove deleted path %s from temporary index: %w", candidate.path, err)
			}
			continue
		}
		if _, err := snapshotGitOutput(ctx, r.cfg.RepoRoot, gitEnv, nil, "add", "--", candidate.path); err != nil {
			return nil, fmt.Errorf("add path %s to temporary index: %w", candidate.path, err)
		}
	}
	tree, err := snapshotGitOutput(ctx, r.cfg.RepoRoot, gitEnv, nil, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("write workspace snapshot tree: %w", err)
	}
	if tree == "" || tree == baseTree {
		return nil, nil
	}
	patch, err := snapshotGitOutput(ctx, r.cfg.RepoRoot, gitEnv, nil, "diff", "--binary", baseTree, tree)
	if err != nil {
		return nil, fmt.Errorf("generate workspace snapshot patch: %w", err)
	}
	changedFiles := make([]models.ChangedFile, 0, len(eligible))
	for _, candidate := range eligible {
		changedFiles = append(changedFiles, models.ChangedFile{Path: candidate.path, Kind: candidate.kind})
	}
	return r.recordWorkspaceSnapshotIfChanged(ctx, store.WorkspaceSnapshot{BaseCommit: baseCommit, TreeHash: tree, Patch: []byte(patch), PatchBytes: int64(len(patch)), ChangedFiles: changedFiles, OmittedFiles: omitted, MaxFileBytes: maxFileBytes, CreatedAt: time.Now().UTC()})
}

func (r *runtimeState) recordWorkspaceSnapshotIfChanged(ctx context.Context, snapshot store.WorkspaceSnapshot) (*store.WorkspaceSnapshot, error) {
	latest, err := r.store.LatestWorkspaceSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if latest != nil && latest.TreeHash == snapshot.TreeHash {
		return nil, nil
	}
	if latest != nil {
		snapshot.ParentID = latest.ID
	}
	return r.store.RecordWorkspaceSnapshot(ctx, snapshot)
}

func (r *runtimeState) workspaceSnapshotTempDir() (string, error) {
	dir := r.cfg.TempDir
	if dir == "" {
		if r.cfg.SocketPath != "" {
			dir = filepath.Join(filepath.Dir(r.cfg.SocketPath), "tmp")
		} else {
			dir = filepath.Join(os.TempDir(), "discobox-hooks", "session-"+strings.TrimSpace(r.cfg.SessionID), "tmp")
		}
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create workspace snapshot temporary directory %s: %w", dir, err)
	}
	return dir, nil
}

func workspaceSnapshotCandidates(ctx context.Context, repoRoot string) ([]workspaceSnapshotCandidate, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	entries := bytes.Split(out, []byte{0})
	candidates := make([]workspaceSnapshotCandidate, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		entry := string(entries[i])
		if entry == "" {
			continue
		}
		if len(entry) < 4 {
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
				candidates = append(candidates, workspaceSnapshotCandidate{path: oldPath, kind: watcher.Deleted, status: status})
			}
			if path != "" {
				candidates = append(candidates, workspaceSnapshotCandidate{path: path, kind: watcher.Created, status: status})
			}
			continue
		}
		if path == "" {
			continue
		}
		candidates = append(candidates, workspaceSnapshotCandidate{path: path, kind: gitStatusChangeKind(status), status: status})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].path == candidates[j].path {
			return candidates[i].kind < candidates[j].kind
		}
		return candidates[i].path < candidates[j].path
	})
	return candidates, nil
}

func workspaceSnapshotCandidateAllowed(ctx context.Context, repoRoot string, candidate workspaceSnapshotCandidate, limit int64) (bool, store.SnapshotOmission) {
	omission := store.SnapshotOmission{Path: candidate.path, Kind: candidate.kind, LimitBytes: limit}
	oldSize, oldOK := gitBlobSize(ctx, repoRoot, "HEAD", candidate.path)
	if oldOK && oldSize > limit {
		omission.Reason = "too_large"
		omission.SizeBytes = oldSize
		return false, omission
	}
	if candidate.kind == watcher.Deleted {
		return true, store.SnapshotOmission{}
	}
	abs := filepath.Join(repoRoot, filepath.FromSlash(candidate.path))
	info, err := os.Lstat(abs)
	if err != nil {
		omission.Reason = "stat_failed"
		return false, omission
	}
	if !info.Mode().IsRegular() {
		omission.Reason = "unsupported_file_type"
		if info.Mode().Type() != 0 {
			omission.SizeBytes = int64(info.Mode().Type())
		}
		return false, omission
	}
	if info.Size() > limit {
		omission.Reason = "too_large"
		omission.SizeBytes = info.Size()
		return false, omission
	}
	return true, store.SnapshotOmission{}
}

func gitBlobSize(ctx context.Context, repoRoot, rev, path string) (int64, bool) {
	//nolint:gosec // Arguments are passed directly to git, not through a shell; path comes from git status output.
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-s", rev+":"+path)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return size, err == nil
}

func snapshotGitOutput(ctx context.Context, dir string, extraEnv map[string]string, stdin []byte, args ...string) (string, error) {
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
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		var exitErr *exec.ExitError
		if argsHaveDiff(args) && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return string(out), nil
		}
		if trimmed == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, trimmed)
	}
	if argsHaveDiff(args) {
		return string(out), nil
	}
	return trimmed, nil
}

func argsHaveDiff(args []string) bool {
	return len(args) > 0 && args[0] == "diff"
}
