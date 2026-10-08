// Package githttp serves a source's origin over git's smart HTTP protocol, as
// the repository's owner, through gitbackend. A live origin — the developer's
// own repository — is served fetch-only, with only an allow-list of refs
// advertised. The sandbox's own worktree is not served here: sandbox-agent
// serves it, and the pool forwards to it (ADR 0126 §4).
package githttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/gitbackend"
	"github.com/discobox-ai/discobox/pool-agent/childproc"
	"github.com/discobox-ai/discobox/pool-agent/execidentity"
)

// Repository is what one request is served from: a directory git
// http-backend can open, the identity it runs as, and whether it is a live
// origin.
type Repository struct {
	Path string
	// UID and GID are the identity the backend runs as: the repository's
	// owner, so git's dubious-ownership check (safe.directory) does not reject
	// the request. A negative uid means "run as the calling process" (used when
	// there is no specific owner to impersonate).
	UID int
	GID int
	// Live marks a repository someone else is using — the developer's own Git
	// directory, served as a source's origin (ADR 0126 §4). It is served
	// fetch-only, and advertises HEAD, the branch HEAD names when it is asked,
	// and Refs, and nothing else.
	Live bool
	// Refs are the refs a live repository advertises besides HEAD and its
	// branch. Each is a full name under refs/.
	Refs []string
}

// ServeBackend runs git http-backend against repo, as its owner.
//
// A live repository is served under four restrictions that only hold together,
// because each one alone leaves a way past the allow-list:
//
//   - Only the smart upload-pack service. http-backend also answers the dumb
//     protocol — loose objects and packs fetched as plain files — which
//     reaches every object in the repository whatever is advertised, so
//     anything but the two upload-pack endpoints is refused here and
//     http.getanyfile is off besides.
//   - Not the developer's repository itself but a snapshot of it
//     (liveSnapshot): a repository holding the allowed refs and nothing
//     else, borrowing the developer's objects. What it advertises is fixed
//     before the backend starts, and the developer's own .git/config is never
//     read.
//   - Protocol v0, whatever the client asks for. A v2 upload-pack serves any
//     object it is asked for by id, advertised or not and whatever
//     uploadpack.allow*SHA1InWant says, so a hidden ref's commits would be one
//     guess of its id away; v0 refuses a want that no advertised ref reaches.
//   - Never receive-pack, whatever the caller's token allows: nothing pushes
//     into the developer's own repository, and work leaves a sandbox through
//     apply and the worktree route instead.
func ServeBackend(w http.ResponseWriter, r *http.Request, repo Repository, suffix string) {
	backend := gitbackend.Backend{
		Root: repo.Path,
		// A bare origin takes the client's pushes (ADR 0058 §3).
		Config:      []string{"-c", "http.receivepack=true"},
		RemoteUser:  "pool-agent",
		SysProcAttr: execidentity.SysProcAttr(repo.UID, repo.GID),
		// Through childproc, so the pool agent's reaper leaves this backend's
		// exit status to gitbackend's Wait rather than collecting it first
		// (ADR 0087).
		Start: func(cmd *exec.Cmd) (gitbackend.Process, error) { return childproc.Start(cmd) },
	}
	if repo.Live {
		if !uploadPackRequest(r, suffix) {
			http.Error(w, "a live origin is served fetch-only, over git's smart protocol", http.StatusForbidden)
			return
		}
		snapshot, err := liveSnapshot(r.Context(), repo)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = os.RemoveAll(snapshot) }()
		backend.Root = snapshot
		backend.Config = liveOriginArgs()
		backend.FixedProtocol = true
	}
	gitbackend.Serve(w, r, backend, suffix)
}

// uploadPackRequest reports whether r is one of the two requests a smart fetch
// makes, and nothing else http-backend would answer.
func uploadPackRequest(r *http.Request, suffix string) bool {
	switch suffix {
	case "/info/refs":
		return r.Method == http.MethodGet && slices.Equal(r.URL.Query()["service"], []string{"git-upload-pack"})
	case "/git-upload-pack":
		return r.Method == http.MethodPost
	default:
		return false
	}
}

// liveOriginArgs are the switches a live repository's snapshot is served
// under; see ServeBackend. The snapshot's own configuration is the pool's, but
// they are given on the command line all the same, so what they say does not
// depend on what liveSnapshot writes.
func liveOriginArgs() []string {
	return []string{
		"-c", "http.receivepack=false",
		"-c", "http.uploadpack=true",
		"-c", "http.getanyfile=false",
		"-c", "uploadpack.allowAnySHA1InWant=false",
		"-c", "uploadpack.allowReachableSHA1InWant=false",
		"-c", "uploadpack.allowTipSHA1InWant=false",
		"-c", "uploadpack.allowRefInWant=false",
	}
}

// liveSnapshot builds the repository one live request is served from and
// returns its path; the caller removes it. It is a bare repository holding
// HEAD and the allow-list — the repository's declared refs and the branch
// HEAD names now, each that exists, at the object it names now — and nothing
// else, with the developer's objects lent through objects/info/alternates.
//
// HEAD is read per request because no push ever updates a live origin: where
// the developer is now is what `git rebase origin/<branch>` in the sandbox is
// rebasing onto.
//
// Serving the developer's repository with hideRefs revealing the allowed refs
// would be shorter and is not equivalent. A hideRefs entry reveals by prefix,
// so revealing refs/heads/feature also reveals refs/heads/feature/x, and
// checking first that feature exists leaves the gap between the check and the
// backend reading the refs for itself: delete feature and create
// feature/private in it, and the private branch is served. Here the backend
// reads only refs this function wrote, so there is no other ref to reveal and
// no later moment at which one could appear.
func liveSnapshot(ctx context.Context, repo Repository) (string, error) {
	candidates := make([]string, 0, len(repo.Refs)+1)
	for _, ref := range repo.Refs {
		if !validAdvertisedRef(ref) {
			return "", fmt.Errorf("live origin ref %q is not a full ref name", ref)
		}
		candidates = append(candidates, ref)
	}
	head, err := headBranch(ctx, repo)
	if err != nil {
		return "", err
	}
	if head != "" && validAdvertisedRef(head) && !slices.Contains(candidates, head) {
		candidates = append(candidates, head)
	}
	refs, err := resolveRefs(ctx, repo, candidates)
	if err != nil {
		return "", err
	}
	headValue := "ref: " + head
	if head == "" {
		// Detached: HEAD is a commit, and the snapshot's HEAD is that commit.
		id, err := repositoryGit(ctx, repo, "rev-parse", "--verify", "--quiet", "HEAD")
		if err != nil {
			return "", fmt.Errorf("read live origin HEAD: %w", err)
		}
		headValue = id
	} else if !validAdvertisedRef(head) {
		// HEAD names something no allow-list could hold; serve it unborn.
		headValue = "ref: refs/heads/.unborn"
	}
	format, err := repositoryGit(ctx, repo, "rev-parse", "--show-object-format")
	if err != nil {
		return "", fmt.Errorf("read live origin object format: %w", err)
	}
	objects, err := filepath.Abs(filepath.Join(repo.Path, "objects"))
	if err != nil {
		return "", err
	}

	shallow, err := readShallow(repo.Path, format)
	if err != nil {
		return "", err
	}

	dir, err := os.MkdirTemp("", "discobox-live-origin-")
	if err != nil {
		return "", err
	}
	if err := writeSnapshot(dir, headValue, format, objects, refs, shallow, repo.UID, repo.GID); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// readShallow is the object ids in the repository's shallow file, if it has
// one. This is the one read of the developer's repository that is not git
// running as its owner, so it is the pool agent, as root, reading a file the
// developer controls — and what it reads is copied into a file the developer
// owns and that upload-pack echoes back in its errors. So it is opened through
// os.Root, which refuses a link leading out of the Git directory, it must be a
// regular file, and only lines that are object ids of the repository's format
// are taken: anything else fails the request without saying what it held.
func readShallow(gitDir, objectFormat string) ([]string, error) {
	root, err := os.OpenRoot(gitDir)
	if err != nil {
		return nil, fmt.Errorf("open live origin: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open("shallow")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open live origin shallow file: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat live origin shallow file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("live origin shallow file is not a regular file")
	}
	size := 41
	if objectFormat == "sha256" {
		size = 65
	}
	// A line per shallow commit, so a cap well past any real one bounds the
	// read without ever cutting a genuine file short.
	data, err := io.ReadAll(io.LimitReader(file, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read live origin shallow file: %w", err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		if len(line) != size-1 || strings.Trim(line, "0123456789abcdef") != "" {
			return nil, errors.New("live origin shallow file holds something other than object ids")
		}
		ids = append(ids, line)
	}
	return ids, nil
}

// snapshotRef is one allowed ref and the object it names.
type snapshotRef struct {
	name string
	id   string
}

// resolveRefs is each candidate that exists, with the object it names.
// for-each-ref matches its patterns by prefix, so what it lists is filtered
// back down to the names asked for.
func resolveRefs(ctx context.Context, repo Repository, candidates []string) ([]snapshotRef, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	out, err := repositoryGit(ctx, repo, append([]string{"for-each-ref", "--format=%(objectname) %(refname)", "--"}, candidates...)...)
	if err != nil {
		return nil, fmt.Errorf("list live origin refs: %w", err)
	}
	var refs []snapshotRef
	for _, line := range strings.Split(out, "\n") {
		id, name, ok := strings.Cut(line, " ")
		if ok && slices.Contains(candidates, name) {
			refs = append(refs, snapshotRef{name: name, id: id})
		}
	}
	return refs, nil
}

// writeSnapshot lays the snapshot repository out in dir, owned by uid:gid,
// which is who the backend serving it runs as.
func writeSnapshot(dir, head, objectFormat, objects string, refs []snapshotRef, shallow []string, uid, gid int) error {
	config := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"
	if objectFormat != "sha1" {
		config = "[core]\n\trepositoryformatversion = 1\n\tbare = true\n[extensions]\n\tobjectformat = " + objectFormat + "\n"
	}
	var packed strings.Builder
	for _, ref := range refs {
		packed.WriteString(ref.id)
		packed.WriteString(" ")
		packed.WriteString(ref.name)
		packed.WriteString("\n")
	}
	files := map[string]string{
		"HEAD":                    head + "\n",
		"config":                  config,
		"packed-refs":             packed.String(),
		"objects/info/alternates": objects + "\n",
	}
	// A shallow repository's history stops at the commits this names; without
	// it the backend walks into parents that are not there.
	if len(shallow) > 0 {
		files["shallow"] = strings.Join(shallow, "\n") + "\n"
	}
	// Owner-only throughout: the snapshot is read by the backend, which runs
	// as its owner, and by nothing else.
	created := []string{dir}
	for _, sub := range []string{"refs", "objects", "objects/info"} {
		path := filepath.Join(dir, sub)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		created = append(created, path)
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return err
		}
		created = append(created, path)
	}
	if uid < 0 {
		return nil
	}
	// Each path this function created, by name: nothing else is in dir, and
	// nothing is walked that could have been swapped for a link.
	for _, path := range created {
		if err := os.Lchown(path, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// validAdvertisedRef accepts a full ref name, refs/<kind>/<name>, of the
// shape git itself would accept: a namespace is never one, and nothing in it
// may reach for-each-ref as a pattern or a packed-refs line as anything but a
// name.
func validAdvertisedRef(ref string) bool {
	rest, ok := strings.CutPrefix(ref, "refs/")
	if !ok || !strings.Contains(strings.Trim(rest, "/"), "/") {
		return false
	}
	return !strings.ContainsAny(ref, " \t\r\n\x00~^:?*[\\") && !strings.Contains(ref, "..") && !strings.Contains(ref, "//") && !strings.HasSuffix(ref, "/")
}

// headBranch is the branch HEAD names, or empty when HEAD is detached.
func headBranch(ctx context.Context, repo Repository) (string, error) {
	out, err := repositoryGit(ctx, repo, "symbolic-ref", "--quiet", "HEAD")
	// --quiet makes "HEAD is not a symbolic ref" exit 1 and say nothing, which
	// is git's own answer for a detached HEAD; anything else is a failure.
	var gitErr *repositoryGitError
	if errors.As(err, &gitErr) && gitErr.exitCode == 1 && gitErr.stderr == "" {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read live origin HEAD: %w", err)
	}
	return out, nil
}

// repositoryGit runs git against repo's Git directory and returns what it
// printed. It runs as the repository's owner, in the backend's own
// environment, for the same reasons the backend does.
func repositoryGit(ctx context.Context, repo Repository, args ...string) (string, error) {
	//nolint:gosec // The executable is fixed and the arguments are fixed or validated ref names.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(gitbackend.BaseEnv(), "GIT_DIR="+repo.Path)
	cmd.SysProcAttr = execidentity.SysProcAttr(repo.UID, repo.GID)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := childproc.Run(cmd); err != nil {
		var exitErr *exec.ExitError
		code := -1
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		return "", &repositoryGitError{err: err, exitCode: code, stderr: strings.TrimSpace(stderr.String())}
	}
	return strings.TrimSpace(stdout.String()), nil
}

type repositoryGitError struct {
	err      error
	exitCode int
	stderr   string
}

func (e *repositoryGitError) Error() string { return fmt.Sprintf("%v: %s", e.err, e.stderr) }
func (e *repositoryGitError) Unwrap() error { return e.err }
