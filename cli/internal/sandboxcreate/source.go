package sandboxcreate

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/origin"
	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/internal/gitutil"
)

const (
	runSourceKindGit         = "git"
	runWorkspaceModeClean    = "clean"
	runWorkspaceModeDirty    = "dirty"
	runSourceRefTypeBranch   = "branch"
	runSourceRefTypeTag      = "tag"
	runSourceRefTypeCommit   = "commit"
	runSnapshotRefPrefix     = "refs/discobox/run/"
	defaultRunSourceDir      = "/workspace/source"
	defaultRunWorkingDir     = "/workspace/source"
	defaultRemoteBranch      = "HEAD"
	runSnapshotCommitMessage = "disco run workspace snapshot\n"
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
	Kind           string
	URL            string
	LocalDirectory string
	RepoRoot       string
	Checkout       resolvedRunSourceCheckout
	Workspace      resolvedRunSourceWorkspace
	Destination    resolvedRunSourceDestination
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
// and "disco ls" here should list it.
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
	if s.URL != "" {
		u, err := url.Parse(s.URL)
		if err != nil {
			return nil, err
		}
		source.SetURL(apiclientgen.NewOptURI(*u))
	}
	source.SetLocalDirectory(optionalString(s.LocalDirectory))
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
	snapshotID, err := id.New(id.PrefixSnapshot)
	if err != nil {
		return resolvedRunSource{}, err
	}
	snapshotCommit, err := gitutil.CommitTree(ctx, repoRoot, workspaceTree.Tree, workspaceTree.BaseCommit, runSnapshotCommitMessage)
	if err != nil {
		return resolvedRunSource{}, err
	}
	snapshotRef := runSnapshotRefPrefix + snapshotID
	if err := gitutil.UpdateRef(ctx, repoRoot, snapshotRef, snapshotCommit); err != nil {
		return resolvedRunSource{}, err
	}
	resolved.Workspace = resolvedRunSourceWorkspace{
		Mode:        runWorkspaceModeDirty,
		SnapshotRef: snapshotRef,
		BaseCommit:  workspaceTree.BaseCommit,
	}
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
	return resolvedRunSourceDestination{
		Directory:        repoRoot,
		WorkingDirectory: workingDirectory,
	}
}

func pathInsideDirectory(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
