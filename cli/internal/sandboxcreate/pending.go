package sandboxcreate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	api "github.com/discobox-ai/discobox/api/gen"
	model "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/internal/filelock"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/discobox/internal/privatefs"
	"github.com/discobox-ai/x/gitutil"
)

// pendingManifest deliberately contains no transport configuration or credentials.
// Source identity excludes the server-owned delivery/cache fields.
type pendingManifest struct {
	Version  int             `json:"version"`
	Endpoint string          `json:"endpoint"`
	Project  string          `json:"project"`
	Host     string          `json:"host"`
	Sandbox  string          `json:"sandbox,omitempty"`
	Key      string          `json:"key"`
	Source   pendingIdentity `json:"source"`
	Snapshot string          `json:"snapshot"`
}
type pendingIdentity struct {
	Directory        string `json:"directory"`
	Slug             string `json:"slug"`
	Commit           string `json:"commit"`
	RefName          string `json:"refName"`
	RefType          string `json:"refType"`
	WorkspaceMode    string `json:"workspaceMode"`
	WorkspaceBase    string `json:"workspaceBase"`
	SnapshotRef      string `json:"snapshotRef"`
	Destination      string `json:"destination"`
	WorkingDirectory string `json:"workingDirectory"`
	Purpose          string `json:"purpose"`
	Kind             string `json:"kind"`
}

func sourceIdentity(s model.GitSource, key string) pendingIdentity {
	slug := s.Slug.Or("")
	if slug == "" && key == "" {
		slug = "primary"
	}
	c, w, d := s.Checkout.Value, s.Workspace.Value, s.Destination.Value
	return pendingIdentity{Directory: s.LocalDirectory.Or(""), Slug: slug, Commit: c.Commit.Or(""), RefName: c.RefName.Or(""), RefType: c.RefType.Or(""), WorkspaceMode: string(w.Mode.Or(api.GitSourceWorkspaceModeClean)), WorkspaceBase: w.BaseCommit.Or(""), SnapshotRef: w.SnapshotRef.Or(""), Destination: d.Directory.Or(""), WorkingDirectory: d.WorkingDirectory.Or(""), Purpose: string(s.Purpose.Or(api.GitSourcePurposeWork)), Kind: string(s.Kind)}
}
func pendingRoot() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "discobox", "client", "pending-sources"), nil
}
func pendingEndpoint(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" {
		return "", fmt.Errorf("invalid source recovery endpoint")
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}
func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	if err := privatefs.Restrict(path); err != nil {
		return err
	}
	return privatefs.Check(path)
}
func writeManifest(dir string, m pendingManifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "manifest.new")
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := privatefs.Restrict(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "manifest.json"))
}
func readManifest(dir string) (pendingManifest, error) {
	var m pendingManifest
	if err := privatefs.Check(dir); err != nil {
		return m, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, err
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if m.Version != 1 || m.Endpoint == "" || m.Project == "" || m.Host == "" || m.Source.Directory == "" || m.Source.Commit == "" {
		return m, errors.New("invalid pending source manifest")
	}
	return m, nil
}
func pendingLock(ctx context.Context, dir string) (*filelock.Lock, error) {
	for {
		lock, err := filelock.TryAcquire(dir + ".lock")
		if !errors.Is(err, filelock.ErrBusy) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// stagePending publishes self-contained repositories before create can be accepted.
func (s *LocalSources) stagePending(ctx context.Context, endpoint, project string, body *model.CreateSandboxBody) error {
	if s == nil {
		return nil
	}
	sort.SliceStable(s.sources, func(i, j int) bool { return s.sources[i].key < s.sources[j].key })
	for i := range s.sources {
		local := &s.sources[i]
		source := body.Config.Source.Value
		if local.key != "" {
			source = body.Config.SourceCodeReferences.Value[local.key]
		}
		if !source.NoLocalRepository.Or(false) {
			continue
		}
		canonical, err := pendingEndpoint(endpoint)
		if err != nil {
			return err
		}
		root, err := pendingRoot()
		if err != nil {
			return err
		}
		if err = privateDirectory(root); err != nil {
			return err
		}
		staged, err := os.MkdirTemp(root, ".stage-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(staged) }()
		if err = privatefs.Restrict(staged); err != nil {
			return err
		}
		m := pendingManifest{Version: 1, Endpoint: canonical, Project: project, Host: body.Origin.Value.HostId, Key: local.key, Source: sourceIdentity(source, local.key)}
		repo := filepath.Join(staged, "repo")
		if _, err = gitutil.Output(ctx, staged, nil, nil, "init", "--bare", repo); err != nil {
			return err
		}
		commit, _, snapshot, err := pushRefs(source)
		if err != nil {
			return err
		}
		refs := []string{"fetch", "--no-tags", local.repoRoot, commit + ":refs/discobox/pending/base"}
		if snapshot != "" {
			refs = append(refs, snapshot+":"+snapshot)
		}
		if _, err = gitutil.Output(ctx, repo, nil, nil, refs...); err != nil {
			return err
		}
		if snapshot != "" {
			out, e := gitutil.Output(ctx, repo, nil, nil, "rev-parse", "--verify", snapshot+"^{commit}")
			if e != nil {
				return e
			}
			m.Snapshot = strings.TrimSpace(out)
		}
		if err = writeManifest(staged, m); err != nil {
			return err
		}
		// Restrict each object as well as its enclosing directory, on both platforms.
		stagedRoot, err := os.OpenRoot(staged)
		if err != nil {
			return err
		}
		defer stagedRoot.Close()
		if err = filepath.WalkDir(staged, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			mode := os.FileMode(0600)
			if d.IsDir() {
				mode = 0700
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("unexpected symlink in retained repository: %s", path)
			}
			rel, e := filepath.Rel(staged, path)
			if e != nil {
				return e
			}
			if e = stagedRoot.Chmod(rel, mode); e != nil {
				return e
			}
			return privatefs.Restrict(path)
		}); err != nil {
			return err
		}
		_ = stagedRoot.Close()
		published := filepath.Join(root, strings.TrimPrefix(filepath.Base(staged), ".stage-"))
		if err = os.Rename(staged, published); err != nil {
			return err
		}
		if local.cleanup != nil {
			local.cleanup()
			local.cleanup = nil
		}
		local.repoRoot = filepath.Join(published, "repo")
		local.pending = published
	}
	return nil
}
func (s *LocalSources) bindPending(ctx context.Context, sandbox *model.Sandbox) error {
	if s == nil {
		return nil
	}
	for _, local := range s.sources {
		if local.pending == "" {
			continue
		}
		lock, err := pendingLock(ctx, local.pending)
		if err != nil {
			return err
		}
		m, err := readManifest(local.pending)
		if err == nil {
			source := sandbox.Config.Source.Value
			if local.key != "" {
				source = sandbox.Config.SourceCodeReferences.Value[local.key]
			}
			if m.Source != sourceIdentity(source, local.key) {
				err = errors.New("created source does not match retained snapshot")
			} else {
				m.Sandbox = sandbox.ID
				err = writeManifest(local.pending, m)
			}
		}
		_ = lock.Release()
		if err != nil {
			return err
		}
	}
	return nil
}

// ResolvePendingSource is the single recovery lookup, used before repair or Git
// directory discovery. The returned release keeps cleanup from racing delivery.
func resolvePendingSource(ctx context.Context, endpoint string, sandbox *model.Sandbox, entry PendingSourcePush) (string, func(), error) {
	project := sandbox.ProjectId
	identity := sourceIdentity(entry.Source, entry.Key)
	unavailable := func(err error) (string, func(), error) {
		return "", nil, fmt.Errorf("source %q pinned to %s: original snapshot unavailable on this client (%w); retry on the original client if it retains the snapshot, otherwise create a new discobox from the current directory", entry.Slug, identity.Commit, err)
	}
	canonical, err := pendingEndpoint(endpoint)
	if err != nil {
		return unavailable(err)
	}
	root, err := pendingRoot()
	if err != nil {
		return unavailable(err)
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return unavailable(err)
	}
	var matches []string
	for _, dir := range dirs {
		if !dir.IsDir() || strings.HasPrefix(dir.Name(), ".") {
			continue
		}
		path := filepath.Join(root, dir.Name())
		m, e := readManifest(path)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			continue
		}
		if m.Endpoint == canonical && m.Project == project && m.Host == sandbox.Origin.Value.HostId && m.Key == entry.Key && (m.Sandbox == "" || m.Sandbox == sandbox.ID) && m.Source == identity {
			matches = append(matches, path)
		}
	}
	if len(matches) != 1 {
		return unavailable(fmt.Errorf("found %d matching retained snapshots", len(matches)))
	}
	path := matches[0]
	lock, err := pendingLock(ctx, path)
	if err != nil {
		return unavailable(err)
	}
	release := func() { _ = lock.Release() }
	m, err := readManifest(path)
	if err != nil {
		release()
		return unavailable(err)
	}
	if m.Endpoint != canonical || m.Project != project || m.Host != sandbox.Origin.Value.HostId || m.Key != entry.Key || m.Source != identity || (m.Sandbox != "" && m.Sandbox != sandbox.ID) {
		release()
		return unavailable(errors.New("snapshot belongs to another discobox"))
	}
	repo := filepath.Join(path, "repo")
	err = validatePending(ctx, repo, m)
	if err != nil {
		release()
		return unavailable(err)
	}
	m.Sandbox = sandbox.ID
	if err = writeManifest(path, m); err != nil {
		release()
		return unavailable(err)
	}
	return repo, release, nil
}

// RetainedSources describes the recovery data on failure, including ambiguous creates.
func (s *LocalSources) RetainedSources() string {
	if s == nil {
		return ""
	}
	var paths []string
	for _, local := range s.sources {
		if local.pending != "" {
			paths = append(paths, local.pending)
		}
	}
	if len(paths) == 0 {
		return ""
	}
	return fmt.Sprintf("; source snapshots retained at %s; deleting these paths forfeits recovery", strings.Join(paths, ", "))
}

// RecoverSource resolves and locks a retained source for a later delivery.
func (s *LocalSources) RecoverSource(ctx context.Context, endpoint string, sandbox *model.Sandbox, entry PendingSourcePush) (string, error) {
	host, err := hostid.Get()
	if err != nil {
		return "", err
	}
	if host == "" || host != sandbox.Origin.Value.HostId || sandbox.CreatedBySandboxId.Or("") != "" {
		return "", fmt.Errorf("source %q must be delivered from its original client", entry.Slug)
	}
	repo, release, err := resolvePendingSource(ctx, endpoint, sandbox, entry)
	if err != nil {
		return "", err
	}
	s.sources = append(s.sources, localSource{key: entry.Key, repoRoot: repo, pending: filepath.Dir(repo), release: release})
	return repo, nil
}
func (s *LocalSources) lockPending(ctx context.Context, sandbox *model.Sandbox) error {
	if s == nil {
		return nil
	}
	for i := range s.sources {
		local := &s.sources[i]
		if local.pending == "" || local.release != nil {
			continue
		}
		lock, err := pendingLock(ctx, local.pending)
		if err != nil {
			return err
		}
		local.release = func() { _ = lock.Release() }
		m, err := readManifest(local.pending)
		if err != nil {
			return err
		}
		source := sandbox.Config.Source.Value
		if local.key != "" {
			source = sandbox.Config.SourceCodeReferences.Value[local.key]
		}
		if m.Project != sandbox.ProjectId || m.Host != sandbox.Origin.Value.HostId || m.Sandbox != sandbox.ID || m.Key != local.key || m.Source != sourceIdentity(source, local.key) {
			return errors.New("retained source identity does not match discobox")
		}
		if err := validatePending(ctx, local.repoRoot, m); err != nil {
			return err
		}
	}
	return nil
}
func (s *LocalSources) removeConfirmedPending() error {
	if s == nil {
		return nil
	}
	for i := range s.sources {
		local := &s.sources[i]
		if local.pending == "" {
			continue
		}
		if err := os.RemoveAll(local.pending); err != nil {
			return err
		}
		local.pending = ""
	}
	return nil
}

func validatePending(ctx context.Context, repo string, m pendingManifest) error {
	if _, err := os.Stat(filepath.Join(repo, "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("retained source must not depend on an alternate object store")
	}
	if _, err := gitutil.Output(ctx, repo, nil, nil, "cat-file", "-e", m.Source.Commit+"^{commit}"); err != nil {
		return err
	}
	ref := m.Source.SnapshotRef
	if ref == "" {
		if m.Snapshot != "" {
			return errors.New("unexpected captured snapshot")
		}
		return nil
	}
	out, err := gitutil.Output(ctx, repo, nil, nil, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return err
	}
	if m.Snapshot == "" || strings.TrimSpace(out) != m.Snapshot {
		return errors.New("snapshot ref no longer matches captured object")
	}
	return nil
}

// CleanupConfirmedSources accepts only a record reread from the authenticated
// server; missing listings, archive state and elapsed time are never confirmation.
func CleanupConfirmedSources(ctx context.Context, endpoint string, sandbox *model.Sandbox) error {
	if !sandbox.Runtime.SourceDeliveredAt.IsSet() {
		return nil
	}
	canonical, err := pendingEndpoint(endpoint)
	if err != nil {
		return err
	}
	root, err := pendingRoot()
	if err != nil {
		return err
	}
	dirs, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if !dir.IsDir() || strings.HasPrefix(dir.Name(), ".") {
			continue
		}
		path := filepath.Join(root, dir.Name())
		m, err := readManifest(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			continue
		}
		if m.Endpoint != canonical || m.Project != sandbox.ProjectId || m.Host != sandbox.Origin.Value.HostId || m.Sandbox != sandbox.ID {
			continue
		}
		lock, err := pendingLock(ctx, path)
		if err != nil {
			return err
		}
		// The binding is immutable once set. Cleanup and delivery hold this same lock.
		err = os.RemoveAll(path)
		_ = lock.Release()
		if err != nil {
			return err
		}
	}
	return nil
}
