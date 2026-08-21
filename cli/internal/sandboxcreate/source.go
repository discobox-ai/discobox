package sandboxcreate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/origin"
	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/internal/gitutil"
)

const (
	runSourceKindGit       = "git"
	runWorkspaceModeClean  = "clean"
	runWorkspaceModeDirty  = "dirty"
	runSourceRefTypeBranch = "branch"
	runSourceRefTypeTag    = "tag"
	runSourceRefTypeCommit = "commit"
	runSnapshotRefPrefix   = "refs/discobox/run/"
	defaultRunSourceDir    = "/workspace/source"
	defaultRunWorkingDir   = "/workspace/source"
	defaultRemoteBranch    = "HEAD"
	// referenceRunSourceRoot holds an extra source that has no host path of its
	// own to keep, which is every remote one.
	referenceRunSourceRoot = "/workspace"
	// maxRunSourceSlugLen is the API's slug limit.
	maxRunSourceSlugLen      = 63
	runSnapshotCommitMessage = "discobox run workspace snapshot\n"
	runEmptyBaseMessage      = "discobox run empty base\n"
)

// IncludeDirty decides whether uncommitted local work is carried into the
// sandbox as a workspace snapshot. It implements pflag.Value so frontends can
// bind it directly to a --include-dirty flag.
type IncludeDirty string

const (
	// IncludeDirtyAuto asks the user when the local workspace is dirty.
	IncludeDirtyAuto IncludeDirty = "auto"
	// IncludeDirtyAlways always snapshots a dirty local workspace.
	IncludeDirtyAlways IncludeDirty = "true"
	// IncludeDirtyNever starts from the checked-out commit and leaves
	// uncommitted work behind.
	IncludeDirtyNever IncludeDirty = "false"
)

func (m *IncludeDirty) String() string { return string(*m) }

func (m *IncludeDirty) Type() string { return "true|false|auto" }

func (m *IncludeDirty) Set(value string) error {
	switch IncludeDirty(strings.ToLower(strings.TrimSpace(value))) {
	case "":
		*m = IncludeDirtyAuto
	case IncludeDirtyAuto:
		*m = IncludeDirtyAuto
	case IncludeDirtyAlways, "yes", "y", "1":
		*m = IncludeDirtyAlways
	case IncludeDirtyNever, "no", "n", "0":
		*m = IncludeDirtyNever
	default:
		return fmt.Errorf("invalid value %q: want true, false, or auto", value)
	}
	return nil
}

// DirtyWorkspace describes the uncommitted work found in a local source repo,
// so a frontend can show it while asking whether to include it.
type DirtyWorkspace struct {
	RepoRoot   string
	BaseCommit string
	Changes    []gitutil.StatusChange
}

// ConfirmIncludeDirtyFunc asks the user whether to carry a dirty workspace into
// the sandbox. A nil func means nobody can be asked, and the uncommitted work is
// included rather than silently dropped.
type ConfirmIncludeDirtyFunc func(context.Context, DirtyWorkspace) (bool, error)

// runSourceOptions carries the caller's dirty-workspace policy into source
// resolution.
type runSourceOptions struct {
	IncludeDirty IncludeDirty
	Confirm      ConfirmIncludeDirtyFunc
}

type resolvedRunSource struct {
	Kind string
	// Slug names this source's repository in the sandbox, and is what a push
	// addresses. It is set only for a source code reference, whose name is the
	// client's to choose; the primary source leaves it to the server, which
	// calls it "primary".
	Slug           string
	URL            string
	LocalDirectory string
	// RepoRoot is where git commands for this source run: the repository root
	// of a local source, and the throwaway repository built over the directory
	// when the directory is in no repository at all.
	RepoRoot string
	// NoLocalRepository states that LocalDirectory holds no repository, so
	// nothing can be cloned from it however reachable it is.
	NoLocalRepository bool
	Checkout          resolvedRunSourceCheckout
	Workspace         resolvedRunSourceWorkspace
	Destination       resolvedRunSourceDestination
	// cleanup releases the throwaway repository, and is nil for a source that
	// did not need one.
	cleanup func()
}

// LocalSources are the local repositories a create resolved its sources from —
// the primary source and every `--include` reference — and that a push delivers
// those sources out of. Close releases them: a directory with no repository of
// its own got a throwaway one built over it, which is deleted once the source
// has reached the sandbox, and a real repository has nothing to release.
//
// They are carried from create to delivery rather than resolved twice, because
// a throwaway repository cannot be found again: it holds the only copy of the
// base commit and the workspace snapshot that the sandbox was configured
// against.
type LocalSources struct {
	sources []localSource
}

// localSource is one resolved source's repository, addressed by the same key
// the create request filed the source under: empty for the primary source, and
// the source code reference key — the sandbox directory it lands in — for a
// reference.
type localSource struct {
	key      string
	repoRoot string
	cleanup  func()
}

// add records the repository a resolved source was built from. The zero key is
// the primary source.
func (s *LocalSources) add(key string, resolved resolvedRunSource) {
	s.sources = append(s.sources, localSource{key: key, repoRoot: resolved.RepoRoot, cleanup: resolved.cleanup})
}

// Close releases every local source. It is safe to call on a nil value and to
// call more than once, so a caller can defer it and still close early.
func (s *LocalSources) Close() {
	if s == nil {
		return
	}
	for i := range s.sources {
		if s.sources[i].cleanup == nil {
			continue
		}
		s.sources[i].cleanup()
		s.sources[i].cleanup = nil
	}
}

// close releases the throwaway repository a resolved source built, for the
// paths that fail before it is handed to a LocalSources.
func (s resolvedRunSource) close() {
	if s.cleanup != nil {
		s.cleanup()
	}
}

type resolvedRunSourceCheckout struct {
	Commit  string
	RefName string
	RefType string
}

type resolvedRunSourceWorkspace struct {
	Mode        string
	SnapshotRef string
	BaseCommit  string
}

type resolvedRunSourceDestination struct {
	Directory        string
	WorkingDirectory string
}

func resolveRunSource(ctx context.Context, sourceArg string, opts runSourceOptions) (resolvedRunSource, error) {
	source, ref, explicitRef := splitRunSourceRef(sourceArg)
	if strings.TrimSpace(source) == "" {
		return resolvedRunSource{}, fmt.Errorf("source directory or Git repository is required")
	}
	if isRemoteGitSource(source) {
		if opts.IncludeDirty == IncludeDirtyAlways {
			return resolvedRunSource{}, fmt.Errorf("--include-dirty=true needs a local source: a remote repository has no working tree")
		}
		return resolveRemoteRunSource(ctx, source, ref, explicitRef)
	}
	return resolveLocalRunSource(ctx, source, ref, explicitRef, opts)
}

// ResolveOrigin resolves the origin of a CLI invocation acting on sourceArg:
// the client host, and the project directory the command was run against. Any
// @REF suffix is dropped, so every sandbox from a directory shares one origin
// regardless of the ref it checked out.
//
// A remote repository has no local project directory, so the origin falls back
// to the working directory — the sandbox is still one you started from here,
// and "discobox ls" here should list it.
func ResolveOrigin(ctx context.Context, sourceArg string) (apimodel.Origin, error) {
	source, _, _ := splitRunSourceRef(sourceArg)
	dir := strings.TrimSpace(source)
	if dir == "" || isRemoteGitSource(dir) {
		cwd, err := os.Getwd()
		if err != nil {
			return apimodel.Origin{}, fmt.Errorf("resolve working directory: %w", err)
		}
		dir = cwd
	}
	return origin.Resolve(ctx, dir)
}

// OriginKey is the listing filter matching ResolveOrigin's origin.
func OriginKey(ctx context.Context, sourceArg string) (string, error) {
	resolved, err := ResolveOrigin(ctx, sourceArg)
	if err != nil {
		return "", err
	}
	return origin.Key(resolved), nil
}

func (s resolvedRunSource) apiGitSource() (*apimodel.GitSource, error) {
	source := &apimodel.GitSource{Kind: apiclientgen.GitSourceKindGit}
	source.SetSlug(optionalString(s.Slug))
	if s.URL != "" {
		u, err := url.Parse(s.URL)
		if err != nil {
			return nil, err
		}
		source.SetURL(apiclientgen.NewOptURI(*u))
	}
	source.SetLocalDirectory(optionalString(s.LocalDirectory))
	if s.NoLocalRepository {
		source.SetNoLocalRepository(apiclientgen.NewOptBool(true))
	}
	checkout := apimodel.GitSourceCheckout{}
	checkout.SetCommit(optionalString(s.Checkout.Commit))
	checkout.SetRefName(optionalString(s.Checkout.RefName))
	checkout.SetRefType(optionalString(s.Checkout.RefType))
	source.SetCheckout(apiclientgen.NewOptGitSourceCheckout(checkout))
	workspace := apimodel.GitSourceWorkspace{}
	switch s.Workspace.Mode {
	case runWorkspaceModeDirty:
		workspace.SetMode(apiclientgen.NewOptGitSourceWorkspaceMode(apiclientgen.GitSourceWorkspaceModeDirty))
	default:
		workspace.SetMode(apiclientgen.NewOptGitSourceWorkspaceMode(apiclientgen.GitSourceWorkspaceModeClean))
	}
	workspace.SetSnapshotRef(optionalString(s.Workspace.SnapshotRef))
	workspace.SetBaseCommit(optionalString(s.Workspace.BaseCommit))
	source.SetWorkspace(apiclientgen.NewOptGitSourceWorkspace(workspace))
	destination := apimodel.GitSourceDestination{}
	destination.SetDirectory(optionalString(s.Destination.Directory))
	destination.SetWorkingDirectory(optionalString(s.Destination.WorkingDirectory))
	source.SetDestination(apiclientgen.NewOptGitSourceDestination(destination))
	return source, nil
}

func resolveLocalRunSource(ctx context.Context, source, ref string, explicitRef bool, opts runSourceOptions) (resolvedRunSource, error) {
	absSource, err := filepath.Abs(source)
	if err != nil {
		return resolvedRunSource{}, fmt.Errorf("resolve source directory: %w", err)
	}
	info, err := os.Stat(absSource)
	if err != nil {
		return resolvedRunSource{}, fmt.Errorf("stat source directory %s: %w", absSource, err)
	}
	if !info.IsDir() {
		return resolvedRunSource{}, fmt.Errorf("source %s is not a directory", absSource)
	}
	repoRoot, err := gitutil.Root(ctx, absSource)
	if errors.Is(err, gitutil.ErrNotARepository) {
		return resolveDirectoryRunSource(ctx, absSource, ref, explicitRef, opts)
	}
	if err != nil {
		return resolvedRunSource{}, err
	}
	destination := localRunDestination(repoRoot, absSource)
	resolved := resolvedRunSource{
		Kind:           runSourceKindGit,
		LocalDirectory: repoRoot,
		RepoRoot:       repoRoot,
		Workspace: resolvedRunSourceWorkspace{
			Mode: runWorkspaceModeClean,
		},
		Destination: destination,
	}
	if explicitRef {
		// A snapshot is only ever built on top of HEAD, so an explicit ref and
		// uncommitted work are mutually exclusive rather than silently ignored.
		if opts.IncludeDirty == IncludeDirtyAlways {
			return resolvedRunSource{}, fmt.Errorf("--include-dirty=true cannot be combined with an explicit ref (%s@%s): uncommitted changes only apply on top of the checked-out commit", source, ref)
		}
		commit, err := gitutil.ResolveCommit(ctx, repoRoot, ref)
		if err != nil {
			return resolvedRunSource{}, err
		}
		resolved.Checkout = localRunCheckout(ctx, repoRoot, ref, commit)
		return resolved, nil
	}
	baseCommit, err := gitutil.ResolveCommit(ctx, repoRoot, "HEAD")
	if err != nil {
		return resolvedRunSource{}, err
	}
	resolved.Checkout = localRunCheckout(ctx, repoRoot, "", baseCommit)
	if opts.IncludeDirty == IncludeDirtyNever {
		return resolved, nil
	}
	workspaceTree, cleanup, err := gitutil.CurrentWorkspaceTree(ctx, repoRoot)
	if err != nil {
		return resolvedRunSource{}, err
	}
	defer cleanup()
	if !workspaceTree.Dirty {
		return resolved, nil
	}
	include, err := includeDirtyWorkspace(ctx, repoRoot, workspaceTree.BaseCommit, opts)
	if err != nil {
		return resolvedRunSource{}, err
	}
	if !include {
		return resolved, nil
	}
	workspace, err := snapshotWorkspace(ctx, repoRoot, workspaceTree)
	if err != nil {
		return resolvedRunSource{}, err
	}
	resolved.Workspace = workspace
	return resolved, nil
}

// snapshotWorkspace records a dirty working tree as a commit on top of the
// commit it differs from, under a ref of our own so no branch is disturbed. The
// sandbox reads the difference between the two back out as uncommitted changes.
func snapshotWorkspace(ctx context.Context, repoRoot string, tree gitutil.WorkspaceTree) (resolvedRunSourceWorkspace, error) {
	snapshotID, err := id.New(id.PrefixSnapshot)
	if err != nil {
		return resolvedRunSourceWorkspace{}, err
	}
	snapshotCommit, err := gitutil.CommitTree(ctx, repoRoot, tree.Tree, tree.BaseCommit, runSnapshotCommitMessage)
	if err != nil {
		return resolvedRunSourceWorkspace{}, err
	}
	snapshotRef := runSnapshotRefPrefix + snapshotID
	if err := gitutil.UpdateRef(ctx, repoRoot, snapshotRef, snapshotCommit); err != nil {
		return resolvedRunSourceWorkspace{}, err
	}
	return resolvedRunSourceWorkspace{
		Mode:        runWorkspaceModeDirty,
		SnapshotRef: snapshotRef,
		BaseCommit:  tree.BaseCommit,
	}, nil
}

// resolveDirectoryRunSource prepares a source from a directory that is in no
// Git repository.
//
// The directory's content is the uncommitted work of a repository that does not
// exist yet, so this builds that repository outside the directory, commits an
// empty root commit as the base, and snapshots the whole directory on top of it
// as a dirty workspace. The sandbox comes up with the directory's files as
// uncommitted changes, which is what they are, and the user's directory is left
// untouched — no .git appears in it, and the repository is deleted once the
// source has been delivered.
//
// Nobody is asked about this. The dirty-workspace question offers the last
// commit as its alternative, and there is none here: excluding the content
// would start the sandbox on an empty directory.
func resolveDirectoryRunSource(ctx context.Context, dir, ref string, explicitRef bool, opts runSourceOptions) (resolvedRunSource, error) {
	if explicitRef {
		return resolvedRunSource{}, fmt.Errorf("%s is not a Git repository, so it has no ref %q to check out", dir, ref)
	}
	if opts.IncludeDirty == IncludeDirtyNever {
		return resolvedRunSource{}, fmt.Errorf("--include-dirty=false leaves nothing to run: %s is not a Git repository, so everything in it is uncommitted", dir)
	}
	repoRoot, cleanup, err := gitutil.InitOverWorkTree(ctx, dir)
	if err != nil {
		return resolvedRunSource{}, err
	}
	resolved, err := directoryRunSource(ctx, dir, repoRoot)
	if err != nil {
		cleanup()
		return resolvedRunSource{}, err
	}
	resolved.cleanup = cleanup
	return resolved, nil
}

// directoryRunSource fills in the source that repoRoot, a fresh repository over
// dir, describes.
func directoryRunSource(ctx context.Context, dir, repoRoot string) (resolvedRunSource, error) {
	emptyTree, err := gitutil.EmptyTree(ctx, repoRoot)
	if err != nil {
		return resolvedRunSource{}, err
	}
	baseCommit, err := gitutil.CommitTree(ctx, repoRoot, emptyTree, "", runEmptyBaseMessage)
	if err != nil {
		return resolvedRunSource{}, err
	}
	branch, ok := gitutil.CurrentBranch(ctx, repoRoot)
	if !ok {
		return resolvedRunSource{}, fmt.Errorf("new git repository for %s has no branch checked out", dir)
	}
	// The base commit has to be on the branch, not just in the object database:
	// it is what the sandbox checks out, and what the snapshot is measured
	// against on both ends.
	if err := gitutil.UpdateRef(ctx, repoRoot, "refs/heads/"+branch, baseCommit); err != nil {
		return resolvedRunSource{}, err
	}
	resolved := resolvedRunSource{
		Kind:              runSourceKindGit,
		LocalDirectory:    dir,
		RepoRoot:          repoRoot,
		NoLocalRepository: true,
		Checkout: resolvedRunSourceCheckout{
			Commit:  baseCommit,
			RefName: branch,
			RefType: runSourceRefTypeBranch,
		},
		Workspace:   resolvedRunSourceWorkspace{Mode: runWorkspaceModeClean},
		Destination: localRunDestination(dir, dir),
	}
	workspaceTree, cleanup, err := gitutil.CurrentWorkspaceTree(ctx, repoRoot)
	if err != nil {
		return resolvedRunSource{}, err
	}
	defer cleanup()
	if !workspaceTree.Dirty {
		// An empty directory. There is nothing to snapshot, and the sandbox
		// starts on the empty base commit — which is the point of running in
		// one.
		return resolved, nil
	}
	workspace, err := snapshotWorkspace(ctx, repoRoot, workspaceTree)
	if err != nil {
		return resolvedRunSource{}, err
	}
	resolved.Workspace = workspace
	return resolved, nil
}

// includeDirtyWorkspace decides whether the dirty workspace at repoRoot becomes
// a snapshot. Only "auto" asks, and only when there is someone to ask: with no
// confirmation func the uncommitted work is included, because dropping a user's
// edits is the more surprising of the two answers.
func includeDirtyWorkspace(ctx context.Context, repoRoot, baseCommit string, opts runSourceOptions) (bool, error) {
	if opts.IncludeDirty != IncludeDirtyAuto || opts.Confirm == nil {
		return true, nil
	}
	changes, err := gitutil.StatusChanges(ctx, repoRoot)
	if err != nil {
		return false, err
	}
	return opts.Confirm(ctx, DirtyWorkspace{RepoRoot: repoRoot, BaseCommit: baseCommit, Changes: changes})
}

func resolveRemoteRunSource(ctx context.Context, source, ref string, explicitRef bool) (resolvedRunSource, error) {
	if !explicitRef {
		ref = defaultRemoteBranch
	}
	commit, refName, refType, err := resolveRemoteGitRef(ctx, source, ref, explicitRef)
	if err != nil {
		return resolvedRunSource{}, err
	}
	return resolvedRunSource{
		Kind: runSourceKindGit,
		URL:  source,
		Checkout: resolvedRunSourceCheckout{
			Commit:  commit,
			RefName: refName,
			RefType: refType,
		},
		Workspace: resolvedRunSourceWorkspace{
			Mode: runWorkspaceModeClean,
		},
		Destination: defaultRunDestination(),
	}, nil
}

func localRunCheckout(ctx context.Context, repoRoot, ref, commit string) resolvedRunSourceCheckout {
	if ref == "" || ref == "HEAD" {
		branch, ok := gitutil.CurrentBranch(ctx, repoRoot)
		if ok && ref == "" {
			return resolvedRunSourceCheckout{Commit: commit, RefName: branch, RefType: runSourceRefTypeBranch}
		}
		return resolvedRunSourceCheckout{Commit: commit, RefType: runSourceRefTypeCommit}
	}
	if gitRefExists(ctx, repoRoot, "refs/heads/"+ref) {
		return resolvedRunSourceCheckout{Commit: commit, RefName: ref, RefType: runSourceRefTypeBranch}
	}
	if gitRefExists(ctx, repoRoot, "refs/tags/"+ref) {
		return resolvedRunSourceCheckout{Commit: commit, RefName: ref, RefType: runSourceRefTypeTag}
	}
	return resolvedRunSourceCheckout{Commit: commit, RefType: runSourceRefTypeCommit}
}

func resolveRemoteGitRef(ctx context.Context, source, ref string, explicitRef bool) (string, string, string, error) {
	refs := []string{ref}
	if explicitRef {
		refs = append(refs, "refs/heads/"+ref, "refs/tags/"+ref)
	}
	out, err := gitutil.Output(ctx, "", nil, nil, append([]string{"ls-remote", "--symref", source}, refs...)...)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve remote git ref %q: %w", ref, err)
	}
	commit, symbolicRef, err := parseRemoteRefOutput(out, ref)
	if err != nil {
		return "", "", "", err
	}
	if symbolicRef != "" {
		return commit, strings.TrimPrefix(symbolicRef, "refs/heads/"), runSourceRefTypeBranch, nil
	}
	if !explicitRef {
		return commit, "", runSourceRefTypeCommit, nil
	}
	switch {
	case remoteOutputHasRef(out, "refs/heads/"+ref):
		return commit, ref, runSourceRefTypeBranch, nil
	case remoteOutputHasRef(out, "refs/tags/"+ref):
		return commit, ref, runSourceRefTypeTag, nil
	default:
		return commit, "", runSourceRefTypeCommit, nil
	}
}

func parseRemoteRefOutput(out, ref string) (string, string, error) {
	var commit, symbolicRef string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "ref:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				symbolicRef = fields[1]
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 1 && len(fields[0]) == 40 {
			commit = fields[0]
			break
		}
	}
	if commit == "" {
		return "", "", fmt.Errorf("remote ref %q did not resolve to a commit", ref)
	}
	return commit, symbolicRef, nil
}

func remoteOutputHasRef(out, ref string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[1] == ref {
			return true
		}
	}
	return false
}

func gitRefExists(ctx context.Context, repoRoot, ref string) bool {
	_, err := gitutil.Output(ctx, repoRoot, nil, nil, "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

func isRemoteGitSource(value string) bool {
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		return err == nil && u.Scheme != "" && (u.Host != "" || u.Scheme == "file")
	}
	return strings.Contains(value, "@") && strings.Contains(value, ":") && !strings.HasPrefix(value, ".")
}

func splitRunSourceRef(value string) (string, string, bool) {
	at := strings.LastIndex(value, "@")
	if at <= 0 || at == len(value)-1 {
		return value, "", false
	}
	if !strings.Contains(value[:at], "@") && strings.Contains(value[at+1:], ":") {
		return value, "", false
	}
	return value[:at], value[at+1:], true
}

// resolvedReference is one extra source brought into the sandbox alongside the
// primary one, and the key the create request files it under.
type resolvedReference struct {
	// Key is the SourceCodeReferences key, which is the directory the sandbox
	// places the source in.
	Key      string
	Resolved resolvedRunSource
	// APISource is the request shape of Resolved, built once the reference is
	// known to be one the request keeps.
	APISource *apimodel.GitSource
}

// referencePlacement decides what an extra source is called and, when it has no
// host path of its own, where it goes.
//
// `--include` leaves both to the source: `-i ../foo` is the source foo, and a
// remote one lands under /workspace. A declared source (.discobox/sources.json)
// sets both, because its whole point is that the sandbox looks the same however
// the source was obtained: it takes the name the repository declared, and a
// remote fallback is placed at the sibling path the local checkout would have
// occupied, so `../foo` from the primary source resolves inside the sandbox
// whether or not the caller had foo checked out.
type referencePlacement struct {
	// Name is the source's name; empty takes it from the source itself.
	Name string
	// Root is where a source with no host path of its own is placed; empty
	// means /workspace.
	Root string
}

// resolveRunSourceReference resolves an extra source brought in alongside the
// primary one.
//
// It is the same resolution the primary source gets — a local repository, a
// directory in no repository, or a remote URL, each asked about its own
// uncommitted work — and differs only in where the result lands and what it is
// called. A local source keeps its own absolute host path inside the sandbox,
// exactly as the primary source does, so a path means the same thing on both
// sides of the sandbox boundary.
//
// used carries the names already taken by earlier references so two of them
// cannot both claim one; a collision with the primary source's own slug is the
// server's to resolve, and the client reads the resolved slug back off the
// created sandbox rather than assuming it.
func resolveRunSourceReference(ctx context.Context, arg string, placement referencePlacement, opts runSourceOptions, used map[string]struct{}) (resolvedReference, error) {
	resolved, err := resolveRunSource(ctx, arg, opts)
	if err != nil {
		return resolvedReference{}, err
	}
	directory, name := referenceDestination(resolved, placement)
	if directory == "" {
		resolved.close()
		return resolvedReference{}, fmt.Errorf("cannot tell where to put source %s in the discobox", arg)
	}
	slug := uniqueSlug(slugifySource(name), used)
	resolved.Slug = slug
	// Only the primary source decides where the harness starts, so a reference
	// carries a directory and nothing else.
	resolved.Destination = resolvedRunSourceDestination{Directory: directory}
	return resolvedReference{Key: directory, Resolved: resolved}, nil
}

// referenceDestination is the sandbox directory an extra source is placed in,
// and the name it takes from.
func referenceDestination(resolved resolvedRunSource, placement referencePlacement) (directory, name string) {
	if resolved.URL != "" {
		name = placement.Name
		if name == "" {
			name = remoteSourceName(resolved.URL)
		}
		root := placement.Root
		if root == "" {
			root = referenceRunSourceRoot
		}
		return path.Join(filepath.ToSlash(root), slugifySource(name)), name
	}
	// The local destination is the repository root, not the directory that was
	// named: running against a subdirectory brings in the repository that holds
	// it, and the source is named after what it actually is unless the caller
	// named it.
	directory = filepath.ToSlash(resolved.Destination.Directory)
	name = placement.Name
	if name == "" {
		name = path.Base(directory)
	}
	return directory, name
}

// remoteSourceName is the repository name a remote URL ends in.
func remoteSourceName(value string) string {
	trimmed := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(value), "/"), ".git")
	if u, err := url.Parse(trimmed); err == nil && u.Path != "" {
		return path.Base(u.Path)
	}
	// An scp-style remote (git@host:owner/repo) is not a URL; its path is
	// whatever follows the colon.
	if _, rest, ok := strings.Cut(trimmed, ":"); ok {
		return path.Base(rest)
	}
	return path.Base(trimmed)
}

// uniqueSlug settles a slug this client can guarantee: the name itself when it
// is free, and a numbered variant when an earlier source already took it. The
// number has to fit inside the API's slug limit, so a name long enough to fill
// it gives up its tail rather than the suffix.
func uniqueSlug(base string, used map[string]struct{}) string {
	if base == "" {
		base = "source"
	}
	slug := base
	for i := 2; ; i++ {
		if _, taken := used[slug]; !taken {
			used[slug] = struct{}{}
			return slug
		}
		suffix := fmt.Sprintf("-%d", i)
		slug = strings.TrimRight(truncateSlug(base, maxRunSourceSlugLen-len(suffix)), "-") + suffix
	}
}

func truncateSlug(value string, maxLen int) string {
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen]
}

// slugifySource reduces a directory or repository name to the slug shape the
// API accepts: lowercase alphanumerics and dashes, no leading or trailing dash.
func slugifySource(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case b.Len() > 0 && !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
		if b.Len() >= maxRunSourceSlugLen {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func defaultRunDestination() resolvedRunSourceDestination {
	return resolvedRunSourceDestination{
		Directory:        defaultRunSourceDir,
		WorkingDirectory: defaultRunWorkingDir,
	}
}

// localRunDestination keeps the repo root as the sandbox source directory and
// makes the requested source directory the working directory, so running
// against a subdirectory of a repo starts the harness in that subdirectory. The
// inside-repo guard covers cases where the source path does not sit under the
// resolved root lexically (e.g. symlinked paths).
func localRunDestination(repoRoot, sourceDir string) resolvedRunSourceDestination {
	workingDirectory := repoRoot
	if dir := filepath.Clean(sourceDir); pathInsideDirectory(repoRoot, dir) {
		workingDirectory = dir
	}
	if runtime.GOOS == "windows" {
		return windowsRunDestination(repoRoot, workingDirectory)
	}
	return resolvedRunSourceDestination{
		Directory:        repoRoot,
		WorkingDirectory: workingDirectory,
	}
}

// windowsRunDestination places the source at the default container location
// rather than mirroring the host path.
//
// Mirroring only works because a POSIX host path is already a valid path inside
// the sandbox. A Windows one is not: the sandbox runs Linux, and the daemon
// rejects "E:\src\project" outright as not absolute, so the sandbox fails
// before it receives its source. The subdirectory the user asked to run in is
// still honored, by its position within the repository rather than by its
// spelling on this machine.
func windowsRunDestination(repoRoot, workingDirectory string) resolvedRunSourceDestination {
	destination := resolvedRunSourceDestination{
		Directory:        defaultRunSourceDir,
		WorkingDirectory: defaultRunWorkingDir,
	}
	rel, err := filepath.Rel(repoRoot, workingDirectory)
	if err != nil || rel == "." {
		return destination
	}
	destination.WorkingDirectory = path.Join(defaultRunSourceDir, filepath.ToSlash(rel))
	return destination
}

func pathInsideDirectory(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
