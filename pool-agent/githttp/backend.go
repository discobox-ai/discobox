// Package githttp serves git's smart HTTP protocol for a sandbox's
// repositories: it parses the "<id>.git/..." route suffix and runs
// git http-backend as a CGI, as the repository's owner, in an environment it
// builds from scratch. A live origin — the developer's own repository — is
// served fetch-only, with only an allow-list of refs advertised.
package githttp

import (
	"bufio"
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
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/pool-agent/childproc"
	"github.com/discobox-ai/discobox/pool-agent/execidentity"
)

func ParseRepositoryPath(path string) (repositoryID, suffix string, ok bool) {
	repositoryID, suffix, ok = strings.Cut(path, ".git")
	if !ok || !ValidRepositoryID(repositoryID) {
		return "", "", false
	}
	if suffix != "" && !strings.HasPrefix(suffix, "/") {
		return "", "", false
	}
	if suffix == "" {
		suffix = "/"
	}
	return repositoryID, suffix, true
}

func ValidRepositoryID(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	for i, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !valid {
			return false
		}
		if (i == 0 || i == len(value)-1) && r == '-' {
			return false
		}
	}
	return true
}

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
	args := []string{
		"-c", "http.receivepack=true",
		"-c", "receive.denyCurrentBranch=updateInstead",
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
		repo.Path = snapshot
		args = liveOriginArgs()
	}
	//nolint:gosec // The executable is fixed and every argument is either fixed or a validated ref name; request data is passed through CGI env/stdin.
	cmd := exec.CommandContext(r.Context(), "git", append(args, "http-backend")...)
	cmd.Env = backendEnv(r, repo, suffix)
	cmd.Stdin = r.Body
	cmd.SysProcAttr = execidentity.SysProcAttr(repo.UID, repo.GID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Through childproc, so the pool agent's reaper leaves this backend's exit
	// status to the Wait below rather than collecting it first (ADR 0087).
	child, err := childproc.Start(cmd)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	status, err := writeCGIResponse(w, stdout)
	if err != nil {
		_ = child.Wait()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := child.Wait(); err != nil && !errors.Is(r.Context().Err(), context.Canceled) {
		data, _ := io.ReadAll(io.LimitReader(stderr, 4096))
		if status == 0 {
			http.Error(w, strings.TrimSpace(string(data)), http.StatusInternalServerError)
		}
	}
}

// IsReceivePack reports whether r is a push: the advertisement a push starts
// with, or the pack it sends. Every service parameter counts, not the first:
// http-backend reads the last one, so a request naming both must not pass
// for a fetch here.
func IsReceivePack(r *http.Request) bool {
	return slices.Contains(r.URL.Query()["service"], "git-receive-pack") ||
		(r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"))
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
	cmd.Env = append(baseEnv(), "GIT_DIR="+repo.Path)
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

// backendEnv is the environment git http-backend runs in. It is built from
// nothing rather than inherited from the pool agent, because of what sits on
// either side of this process: the agent runs as root, while the backend runs
// as the repository's owner (see ServeBackend) over a worktree the sandbox
// itself can write.
//
// The caller's identity comes first. Every path git resolves from it —
// $HOME/.gitconfig, the XDG attributes file — names a user this process is no
// longer, so git reads root's configuration where the sandbox user is allowed
// to and warns where it is not, muxing that warning into the client's sideband:
//
//	remote: warning: unable to access '/root/.config/git/attributes': Permission denied
//
// The repository comes second. Its own .git/config names programs git runs on
// the pool host — core.hooksPath, uploadpack.packObjectsHook — and repo-local
// configuration is read whatever the switches below say, so the sandbox picks
// what this environment is handed to. Nothing the agent was started with — the
// pool bootstrap token among it — belongs there. PATH is kept, because git
// resolves what it execs through it, and nothing else is.
//
// So the only GIT_* variables the backend sees are the ones set here. An
// inherited one is never harmless: GIT_NAMESPACE empties the ref
// advertisement, GIT_CONFIG_COUNT with its GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_*
// pairs injects the very configuration GIT_CONFIG_NOSYSTEM and
// GIT_CONFIG_GLOBAL are switching off, and GIT_ALTERNATE_OBJECT_DIRECTORIES
// lends the repository objects it does not have.
func backendEnv(r *http.Request, repo Repository, suffix string) []string {
	env := append(baseEnv(),
		"GIT_PROJECT_ROOT="+repo.Path,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO="+suffix,
		"REQUEST_METHOD="+r.Method,
		"QUERY_STRING="+r.URL.RawQuery,
		"REMOTE_USER=pool-agent",
	)
	// Which protocol version the client speaks is a request header, and mapping
	// it onto GIT_PROTOCOL is the HTTP server's job — http-backend answers v0 to
	// anyone who does not, advertising every ref on every request and negotiating
	// over more rounds than v2 needs. It belongs to the client and not to this
	// host — one more thing this environment must not pick up from the agent.
	// A live repository is the exception, answered in v0 whatever the client
	// asked (see ServeBackend).
	if protocol := r.Header.Get("Git-Protocol"); protocol != "" && !repo.Live {
		env = append(env, "GIT_PROTOCOL="+protocol)
	}
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		env = append(env, "CONTENT_TYPE="+contentType)
	}
	// git compresses an upload-pack request once negotiation grows past a round
	// or two, and http-backend only inflates the body when CGI tells it the
	// request is encoded. Without this the backend reads gzip bytes as pkt-line,
	// answers nothing, and the client reports "the remote end hung up
	// unexpectedly" — a fetch that fails only once the negotiation is large
	// enough, which is why small ones have always worked.
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" {
		env = append(env, "HTTP_CONTENT_ENCODING="+encoding)
	}
	if r.ContentLength >= 0 {
		env = append(env, "CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10))
	}
	return env
}

// baseEnv is the part of backendEnv every git this package runs gets: PATH,
// and the switches that keep the system's and the agent's own configuration
// out of it.
func baseEnv() []string {
	env := make([]string, 0, 16)
	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+path)
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
	)
}

func writeCGIResponse(w http.ResponseWriter, stdout io.Reader) (int, error) {
	reader := bufio.NewReader(stdout)
	status := http.StatusOK
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return 0, fmt.Errorf("invalid git http-backend header %q", line)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if strings.EqualFold(name, "Status") {
			fields := strings.Fields(value)
			if len(fields) > 0 {
				if parsed, err := strconv.Atoi(fields[0]); err == nil {
					status = parsed
				}
			}
			continue
		}
		w.Header().Add(name, value)
	}
	w.WriteHeader(status)
	_, err := io.Copy(w, reader)
	return status, err
}
