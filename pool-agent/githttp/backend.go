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
	"net/http"
	"os"
	"os/exec"
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
//   - Every ref under refs/ is hidden, then the allowed ones are revealed. The
//     switches come from the command line, which git reads after the
//     repository's own configuration, so nothing in its .git/config can
//     reveal another ref or turn on a fetch by object id.
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
		refs, err := liveAdvertisedRefs(r.Context(), repo)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		args = liveOriginArgs(refs)
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
// with, or the pack it sends.
func IsReceivePack(r *http.Request) bool {
	return r.URL.Query().Get("service") == "git-receive-pack" ||
		(r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"))
}

// uploadPackRequest reports whether r is one of the two requests a smart fetch
// makes, and nothing else http-backend would answer.
func uploadPackRequest(r *http.Request, suffix string) bool {
	switch suffix {
	case "/info/refs":
		return r.Method == http.MethodGet && r.URL.Query().Get("service") == "git-upload-pack"
	case "/git-upload-pack":
		return r.Method == http.MethodPost
	default:
		return false
	}
}

// liveOriginArgs are the switches a live repository is served under; see
// ServeBackend.
func liveOriginArgs(refs []string) []string {
	args := []string{
		"-c", "http.receivepack=false",
		"-c", "http.uploadpack=true",
		"-c", "http.getanyfile=false",
		"-c", "uploadpack.allowAnySHA1InWant=false",
		"-c", "uploadpack.allowReachableSHA1InWant=false",
		"-c", "uploadpack.allowTipSHA1InWant=false",
		"-c", "uploadpack.allowRefInWant=false",
		"-c", "uploadpack.hideRefs=refs/",
	}
	for _, ref := range refs {
		args = append(args, "-c", "uploadpack.hideRefs=!"+ref)
	}
	return args
}

// liveAdvertisedRefs is the allow-list for one request: the repository's
// declared refs, and the branch HEAD names now. HEAD is read per request
// because no push ever updates a live origin: where the developer is now is
// what `git rebase origin/<branch>` in the sandbox is rebasing onto.
func liveAdvertisedRefs(ctx context.Context, repo Repository) ([]string, error) {
	refs := make([]string, 0, len(repo.Refs)+1)
	for _, ref := range repo.Refs {
		if !validAdvertisedRef(ref) {
			return nil, fmt.Errorf("live origin ref %q is not a full ref name", ref)
		}
		refs = append(refs, ref)
	}
	head, err := headBranch(ctx, repo)
	if err != nil {
		return nil, err
	}
	if head != "" && !slices.Contains(refs, head) {
		refs = append(refs, head)
	}
	return refs, nil
}

// validAdvertisedRef accepts a full ref name under refs/ with nothing that
// would change how a hideRefs entry reads: a leading "!" or "^" is syntax
// there, not part of the name.
func validAdvertisedRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || len(ref) == len("refs/") {
		return false
	}
	return !strings.ContainsAny(ref, " \t\r\n\x00~^:?*[\\") && !strings.Contains(ref, "..") && !strings.HasSuffix(ref, "/")
}

// headBranch is the branch HEAD names, or empty when HEAD is detached. It
// runs as the repository's owner, in the backend's own environment, for the
// same reasons the backend does.
func headBranch(ctx context.Context, repo Repository) (string, error) {
	//nolint:gosec // The executable and arguments are fixed.
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "--quiet", "HEAD")
	cmd.Env = append(baseEnv(), "GIT_DIR="+repo.Path)
	cmd.SysProcAttr = execidentity.SysProcAttr(repo.UID, repo.GID)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := childproc.Run(cmd)
	// --quiet makes "HEAD is not a symbolic ref" exit 1 and say nothing, which
	// is git's own answer for a detached HEAD; anything else is a failure.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && stderr.Len() == 0 {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read live origin HEAD: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

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
