package sourceconverge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/gitprogress"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// repository is one source's checkout, and who git runs as in it.
type repository struct {
	dir   string
	env   map[string]string
	owner *execs.User
}

const (
	// scratchPrefix names the directory a clone lands in before its .git is
	// moved onto the target, one per attempt. It is inside the target rather
	// than beside it: the target can be a mount point, and a rename does not
	// cross one.
	scratchPrefix = ".discobox-materializing-"
	// scratchOwnedMarker, inside a scratch directory, says this agent made it.
	// Only a directory carrying it is ever removed, so a path in the target
	// that merely shares the prefix is the sandbox's and is left alone.
	scratchOwnedMarker = ".discobox-scratch"
	// materializingMarker marks a .git this agent moved onto the target and
	// has not finished materializing. A .git with neither it nor the
	// materialized marker is not this agent's to touch.
	materializingMarker = "discobox-materializing"
	// originRemote is git's own name for the remote a checkout came from.
	// For a source delivered into its pool's cache that is the source's real
	// remote, and the pool is reached under the name the source's manifest
	// gives (sandboxconfig.Source.PoolRemote, ADR 26-10-10-545 §6). For a
	// source delivered before that it is the pool itself.
	originRemote = "origin"
	// upstreamRemote is the remote a source's own remote is added as when
	// origin is the pool.
	upstreamRemote = "upstream"
)

// poolRemote is the name the checkout of a source reaches its pool under.
func poolRemote(spec sandboxconfig.Source) string {
	if spec.PoolRemote != "" {
		return spec.PoolRemote
	}
	return originRemote
}

// prepare clones source onto the target, checks it out, restores its
// workspace snapshot and sets up its remotes — everything but marking it
// materialized, which finish does once the converger has checked the document
// still asks for this. It returns false, having changed nothing, while the
// origin has nothing in it to clone: a push-delivered source before the
// client's push lands.
//
// A clone is never left half-made at the target, which is what would make
// every later attempt fail on a partial .git (#30): it lands in a scratch
// directory and its .git is renamed into place only once it is whole. What
// follows the rename is resumable — the .git carries materializingMarker, and
// a later attempt goes again from the checkout. Every step after the rename
// writes the tracked tree whole (checkout -f, restore) rather than cleaning
// it, so files that were in the target before the clone are never removed,
// on the first attempt or a retry.
//
// helper is empty for an origin that takes no token, so that a source the
// sandbox clones from its remote keeps whatever credential helper its user
// configures. progress is told how far the clone has got each time git says.
func (r *repository) prepare(ctx context.Context, source sandboxconfig.RuntimeSource, spec sandboxconfig.Source, helper string, progress func(gitprogress.Progress)) (bool, error) {
	gitDir := filepath.Join(r.dir, ".git")
	pool := poolRemote(spec)
	switch _, err := os.Stat(gitDir); {
	case errors.Is(err, os.ErrNotExist):
		cloned, err := r.clone(ctx, source.OriginURL, spec, helper, progress)
		if err != nil || !cloned {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		if _, err := os.Stat(filepath.Join(gitDir, materializingMarker)); err != nil {
			// Someone else's repository: one the sandbox began itself, or a
			// clone interrupted before this agent cloned sources (#30). A
			// pool's finished checkout was adopted before this
			// (unmarkedCheckout). Resetting it would destroy whatever it holds.
			return false, fmt.Errorf("%s already holds a repository this sandbox did not clone", r.dir)
		}
		// An attempt that moved the clone into place and stopped before it
		// finished: the checkout and restore below write the tracked tree
		// whole again, so it goes on from there — after a fetch, since what
		// stopped it may have been a pin the origin did not have yet.
		if err := r.ensurePoolRemote(ctx, pool, source.OriginURL, helper); err != nil {
			return false, err
		}
		if err := r.run(ctx, nil, "fetch", pool); err != nil {
			return false, err
		}
	}
	if err := r.ensurePoolRemote(ctx, pool, source.OriginURL, helper); err != nil {
		return false, err
	}
	if err := r.checkout(ctx, source.Commit, spec); err != nil {
		return false, err
	}
	if err := r.restoreWorkspace(ctx, pool, spec.Workspace); err != nil {
		return false, err
	}
	if spec.PoolRemote != "" {
		if err := r.configureOrigin(ctx, spec); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := r.configureUpstream(ctx, spec.UpstreamURL); err != nil {
		return false, err
	}
	return true, nil
}

// unmarkedCheckout reports whether the target holds a repository with neither
// marker — not materialized, and not an attempt of this agent's — that a pool
// made before it marked what it materialized, and which commit it has checked
// out. A pool's checkout is told by two things a repository someone else put
// there does not have together: a commit checked out, since a pool's clone
// always ended in one, and an origin that is an absolute path on a filesystem
// — the pool's own, or the bind it pointed origin at — never a URL.
func (r *repository) unmarkedCheckout(ctx context.Context) (string, bool) {
	gitDir := filepath.Join(r.dir, ".git")
	if info, err := os.Lstat(gitDir); err != nil || !info.IsDir() {
		return "", false
	}
	if _, err := os.Lstat(filepath.Join(gitDir, materializingMarker)); err == nil {
		return "", false
	}
	origin, err := r.configValues(ctx, "remote.origin.url")
	if err != nil || len(origin) != 1 || (!strings.HasPrefix(origin[0], "/") && !filepath.IsAbs(origin[0])) {
		return "", false
	}
	commit := r.head(ctx)
	return commit, commit != ""
}

// ensurePoolHeadRef gives the checkout a HEAD for its pool remote when it has
// none, from what the pool's origin names now. A clone made the moment a
// delivery landed, before the pool pointed the origin's HEAD at what arrived,
// has none, and the pool remote's HEAD is the upstream ref a source checked
// out at a bare commit or tag tracks. One that cannot be resolved yet is left
// for a later pass.
func (r *repository) ensurePoolHeadRef(ctx context.Context, pool string) {
	if err := r.run(ctx, nil, "rev-parse", "--verify", "--quiet", "refs/remotes/"+pool+"/HEAD"); err == nil {
		return
	}
	_ = r.run(ctx, nil, "remote", "set-head", pool, "--auto")
}

// finish marks a prepared checkout materialized and returns the commit the
// marker records, which is what the source's reported state names from then
// on, whatever the sandbox commits since.
func (r *repository) finish(ctx context.Context) (string, error) {
	gitDir := filepath.Join(r.dir, ".git")
	commit := r.head(ctx)
	if err := r.writeMarker(filepath.Join(gitDir, sandboxconfig.SourceMaterializedMarker), commit); err != nil {
		return "", err
	}
	return commit, os.Remove(filepath.Join(gitDir, materializingMarker))
}

// clone fetches the origin into a scratch directory inside the target and
// moves its .git onto the target, without a checkout: the checkout that
// follows writes the tree. It returns false, cloning nothing, when the origin
// has no refs — asked first, because a clone of an empty origin that names a
// branch fails rather than coming back empty.
//
// The clone is the one git command here that can take minutes — a remote-URL
// source is a full clone of the remote — so it runs with --progress and its
// meters are parsed and handed to progress as they change, for the pool to
// report while it waits (ADR 0060).
func (r *repository) clone(ctx context.Context, originURL string, spec sandboxconfig.Source, helper string, progress func(gitprogress.Progress)) (bool, error) {
	var credentials []string
	if helper != "" {
		// For these two commands only; ensurePoolRemote then writes the same into
		// the repository for every fetch after them.
		for _, setting := range credentialConfig(originURL, helper) {
			for _, value := range setting.values {
				credentials = append(credentials, "-c", setting.key+"="+value)
			}
		}
	}
	refs, err := r.output(ctx, r.dir, nil, append(slices.Clone(credentials), "ls-remote", "--", originURL)...)
	if err != nil {
		return false, err
	}
	if refs == "" {
		return false, nil
	}
	scratch, err := r.makeScratch()
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(scratch)
	clone := filepath.Join(scratch, "clone")
	args := append(credentials, "clone", "--no-checkout", "--progress", "--origin", poolRemote(spec))
	if spec.RefName != "" {
		args = append(args, "--branch", spec.RefName)
	}
	args = append(args, "--", originURL, clone)
	cmd, err := r.command(ctx, r.dir, args)
	if err != nil {
		return false, err
	}
	// The meters are git's English: a translated one would parse as a message
	// and simply not be reported.
	cmd.Env = append(cmd.Env, "LC_ALL=C")
	stderr := &gitprogress.Writer{Report: progress}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return false, gitError(args, err, stderr.String())
	}
	if err := r.writeMarker(filepath.Join(clone, ".git", materializingMarker), ""); err != nil {
		return false, err
	}
	if err := os.Rename(filepath.Join(clone, ".git"), filepath.Join(r.dir, ".git")); err != nil {
		return false, err
	}
	return true, nil
}

// makeScratch removes what earlier attempts left — only directories this
// agent marked as its own — and makes a fresh one, under a name no other
// attempt shares, that the checkout's owner can clone into.
func (r *repository) makeScratch() (string, error) {
	leftovers, err := filepath.Glob(filepath.Join(r.dir, scratchPrefix+"*"))
	if err != nil {
		return "", err
	}
	for _, leftover := range leftovers {
		info, err := os.Lstat(filepath.Join(leftover, scratchOwnedMarker))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := os.RemoveAll(leftover); err != nil {
			return "", err
		}
	}
	scratch, err := os.MkdirTemp(r.dir, scratchPrefix)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(scratch, scratchOwnedMarker), nil, 0o600); err != nil {
		_ = os.RemoveAll(scratch)
		return "", err
	}
	if r.owner != nil {
		if err := chownTo(scratch, r.owner); err != nil {
			_ = os.RemoveAll(scratch)
			return "", err
		}
	}
	return scratch, nil
}

// checkout writes the working tree at the pinned commit: on the source's
// branch, created there, when it names one, and detached otherwise; with no
// pin, at the ref the clone was asked for, or the clone's own HEAD. It is
// forced, which writes every tracked path whole — over a file the target
// already held at the same path, and over whatever an interrupted attempt
// left — and leaves every other file in the target alone.
func (r *repository) checkout(ctx context.Context, commit string, spec sandboxconfig.Source) error {
	commit = strings.TrimSpace(commit)
	switch {
	case commit != "" && spec.RefName != "" && spec.RefType == "branch":
		return r.run(ctx, nil, "checkout", "--force", "-B", spec.RefName, commit)
	case commit != "":
		return r.run(ctx, nil, "checkout", "--force", "--detach", commit)
	case spec.RefName != "":
		return r.run(ctx, nil, "checkout", "--force", spec.RefName)
	}
	return r.run(ctx, nil, "checkout", "--force", "HEAD")
}

// restoreWorkspace applies a dirty workspace's snapshot to the working tree,
// all unstaged, on top of the branch at its base commit. The snapshot ref is
// fetched explicitly, from the pool: a clone brings branches and tags, and it
// is neither.
func (r *repository) restoreWorkspace(ctx context.Context, pool string, workspace *sandboxconfig.SourceWorkspace) error {
	if workspace == nil {
		return nil
	}
	base, snapshot := strings.TrimSpace(workspace.BaseCommit), strings.TrimSpace(workspace.SnapshotRef)
	if base == "" || snapshot == "" {
		return errors.New("dirty workspace requires baseCommit and snapshotRef")
	}
	if err := r.run(ctx, nil, "check-ref-format", snapshot); err != nil {
		return fmt.Errorf("invalid workspace snapshot ref %q: %w", snapshot, err)
	}
	if err := r.run(ctx, nil, "fetch", pool, "+"+snapshot+":"+snapshot); err != nil {
		return fmt.Errorf("fetch workspace snapshot %q: %w", snapshot, err)
	}
	parent, err := r.output(ctx, r.dir, nil, "rev-parse", "--verify", snapshot+"^")
	if err != nil {
		return fmt.Errorf("resolve workspace snapshot parent: %w", err)
	}
	resolved, err := r.output(ctx, r.dir, nil, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve workspace base commit: %w", err)
	}
	if parent != resolved {
		return fmt.Errorf("workspace snapshot %q is not based on %s", snapshot, base)
	}
	if err := r.run(ctx, nil, "reset", "--hard", base); err != nil {
		return err
	}
	// The snapshot's tree onto the working tree alone: changed and new files
	// written, deleted ones removed, the index left at the base so all of it
	// reads as unstaged. Unlike applying a patch it does not care what is
	// already there, so a retry after an interruption lands the same way.
	if err := r.run(ctx, nil, "restore", "--source="+snapshot, "--worktree", "--", ":/"); err != nil {
		return fmt.Errorf("restore workspace snapshot: %w", err)
	}
	return nil
}

// configureUpstream adds the source's own remote beside an origin that is the
// pool. It is written once, at materialization: it is a starting point, and
// whoever works in the sandbox owns it afterwards.
func (r *repository) configureUpstream(ctx context.Context, upstreamURL string) error {
	upstreamURL = strings.TrimSpace(upstreamURL)
	if upstreamURL == "" {
		return nil
	}
	return r.configureRemote(ctx, upstreamRemote, upstreamURL)
}

// configureOrigin makes a checkout cloned from its pool read as a clone of
// the source's own remote (ADR 26-10-10-545 §6): origin is that remote, its
// remote-tracking refs are where the client last knew the remote's branches
// to be, and the checked-out branch tracks origin's when the remote has it.
//
// It is done once, at materialization, and never asserted afterwards: origin
// belongs to the sandbox from here, and moves when the sandbox fetches the
// real remote. The refs come from the pool, which holds them beside the
// source's branches, so nothing here reaches the remote or needs a
// credential for it. A source with no remote has the pool remote alone, and
// its branch tracks that, as the clone left it.
func (r *repository) configureOrigin(ctx context.Context, spec sandboxconfig.Source) error {
	remote := strings.TrimSpace(spec.UpstreamURL)
	if remote == "" {
		return nil
	}
	if err := r.run(ctx, nil, "fetch", spec.PoolRemote, "+refs/remotes/"+originRemote+"/*:refs/remotes/"+originRemote+"/*"); err != nil {
		return fmt.Errorf("fetch the remote's branches from the pool: %w", err)
	}
	// A delivery carries refs and no symbolic ones, so a HEAD among them is a
	// plain ref frozen where the client's was. None is the honest state: a
	// `git remote set-head origin --auto` asks the remote, as it would in any
	// clone that lacks one.
	if err := r.run(ctx, nil, "symbolic-ref", "--quiet", "refs/remotes/"+originRemote+"/HEAD"); err != nil {
		_ = r.run(ctx, nil, "update-ref", "-d", "refs/remotes/"+originRemote+"/HEAD")
	}
	if err := r.configureRemote(ctx, originRemote, remote); err != nil {
		return err
	}
	if spec.RefType != "branch" || spec.RefName == "" {
		return nil
	}
	tracked := "refs/remotes/" + originRemote + "/" + spec.RefName
	if err := r.run(ctx, nil, "rev-parse", "--verify", "--quiet", tracked); err != nil {
		// The remote has no such branch yet — how most work starts. The
		// branch keeps tracking the pool's.
		return nil //nolint:nilerr // an absent ref is an answer, not a failure
	}
	return r.run(ctx, nil, "branch", "--set-upstream-to="+originRemote+"/"+spec.RefName, spec.RefName)
}

// configureRemote points the remote name at url and gives it the default
// refspec.
func (r *repository) configureRemote(ctx context.Context, name, url string) error {
	if err := r.run(ctx, nil, "config", "--replace-all", "remote."+name+".url", url); err != nil {
		return err
	}
	return r.run(ctx, nil, "config", "--replace-all", "remote."+name+".fetch", "+refs/heads/*:refs/remotes/"+name+"/*")
}

// ensurePoolRemote points the remote the checkout reaches its pool under at
// originURL, gives it the default refspec when it has none, and has its
// credentials read from this agent. It is asserted on every pass, because the
// remote belongs to the sandbox and not to the clone that made it — and the
// pool may serve the origin somewhere new. A refspec someone set by hand is
// theirs and is left alone.
func (r *repository) ensurePoolRemote(ctx context.Context, pool, originURL, helper string) error {
	if err := r.ensureConfig(ctx, configSetting{"remote." + pool + ".url", []string{originURL}}); err != nil {
		return err
	}
	fetch, err := r.configValues(ctx, "remote."+pool+".fetch")
	if err != nil {
		return err
	}
	if len(fetch) == 0 {
		if err := r.run(ctx, nil, "config", "--add", "remote."+pool+".fetch", "+refs/heads/*:refs/remotes/"+pool+"/*"); err != nil {
			return err
		}
	}
	if helper == "" {
		return nil
	}
	for _, setting := range credentialConfig(originURL, helper) {
		if err := r.ensureConfig(ctx, setting); err != nil {
			return err
		}
	}
	return nil
}

// configSetting is one configuration key and every value it is to hold, in
// order.
type configSetting struct {
	key    string
	values []string
}

// credentialConfig is the configuration that hands an origin's credentials to
// the agent's helper alone. The empty helper first clears every helper
// configured before this one — a user's `credential.helper store` included,
// which would otherwise be handed the token to keep — and useHttpPath has git
// name the origin's path, which is what tells one source's origin from
// another's on the same host.
func credentialConfig(originURL, helper string) []configSetting {
	section := "credential." + originURL
	return []configSetting{
		{section + ".helper", []string{"", helper}},
		{section + ".useHttpPath", []string{"true"}},
	}
}

// ensureConfig makes a key hold exactly the setting's values, rewriting it
// only when it differs, so an unchanged repository is not touched.
func (r *repository) ensureConfig(ctx context.Context, setting configSetting) error {
	current, err := r.configValues(ctx, setting.key)
	if err != nil {
		return err
	}
	if slices.Equal(current, setting.values) {
		return nil
	}
	if len(current) > 0 {
		if err := r.run(ctx, nil, "config", "--unset-all", setting.key); err != nil {
			return err
		}
	}
	for _, value := range setting.values {
		if err := r.run(ctx, nil, "config", "--add", setting.key, value); err != nil {
			return err
		}
	}
	return nil
}

// configValues reads a key's values; an unset key is none, not an error.
func (r *repository) configValues(ctx context.Context, key string) ([]string, error) {
	out, err := r.rawOutput(ctx, r.dir, nil, "config", "--null", "--get-all", key)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	values := strings.Split(string(out), "\x00")
	return values[:len(values)-1], nil
}

// head is the commit checked out, empty when there is none.
func (r *repository) head(ctx context.Context) string {
	out, err := r.output(ctx, r.dir, nil, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// writeMarker creates a marker file holding content, owned by the checkout's
// owner as everything else in its .git is.
func (r *repository) writeMarker(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	if r.owner == nil {
		return nil
	}
	return chownTo(path, r.owner)
}

func (r *repository) run(ctx context.Context, stdin []byte, args ...string) error {
	_, err := r.rawOutput(ctx, r.dir, stdin, args...)
	return err
}

func (r *repository) output(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
	out, err := r.rawOutput(ctx, dir, stdin, args...)
	return strings.TrimSpace(string(out)), err
}

// rawOutput runs git in dir and returns its stdout.
func (r *repository) rawOutput(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd, err := r.command(ctx, dir, args)
	if err != nil {
		return nil, err
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), gitError(args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// command is git in dir as the checkout's owner, with the sandbox's
// environment and never a prompt: there is nobody to answer one.
func (r *repository) command(ctx context.Context, dir string, args []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed subcommands; refs, URLs and paths are argv, never a shell.
	cmd.Dir = dir
	attr, err := execs.AgentSysProcAttr(r.owner)
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr = attr
	env := execs.EnvWithRuntimeDefaults(r.env, r.owner)
	env["GIT_TERMINAL_PROMPT"] = "0"
	cmd.Env = make([]string, 0, len(env))
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	return cmd, nil
}

// gitError names a failed git invocation by its subcommand, with what it said.
func gitError(args []string, err error, stderr string) error {
	name := gitSubcommand(args)
	if msg := strings.TrimSpace(stderr); msg != "" {
		return fmt.Errorf("git %s: %w: %s", name, err, msg)
	}
	return fmt.Errorf("git %s: %w", name, err)
}

// gitSubcommand names a git invocation by its subcommand, past any -c.
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return ""
}
