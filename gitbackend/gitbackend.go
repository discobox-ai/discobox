// Package gitbackend serves git's smart HTTP protocol by running
// git http-backend as a CGI: it parses the "<id>.git/..." route suffix both
// repository routes share, tells a push from a fetch, and runs the backend in
// an environment it builds from scratch.
//
// Two processes serve a repository this way, and they serve different ones on
// purpose (ADR 0126 §4): the pool agent serves each source's origin, and the
// sandbox agent serves the sandbox's own worktree. What each serves, and as
// whom, is the caller's; how a request becomes a backend run is this
// package's, so the two cannot drift on it.
package gitbackend

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
	"syscall"
	"time"
)

// ParseRepositoryPath splits a route's wildcard, "<id>.git/<suffix>", into the
// repository id and the path http-backend is asked for.
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

// ValidRepositoryID accepts a source slug: lowercase letters, digits and
// inner hyphens, at most 63 of them.
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

// IsReceivePack reports whether r is a push: the advertisement a push starts
// with, or the pack it sends. Every service parameter counts, not the first:
// http-backend reads the last one, so a request naming both must not pass
// for a fetch here.
func IsReceivePack(r *http.Request) bool {
	return slices.Contains(r.URL.Query()["service"], "git-receive-pack") ||
		(r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"))
}

// Process is a started backend, waited on once its output is copied.
type Process interface {
	Wait() error
}

// Backend is one git http-backend run.
type Backend struct {
	// Root is the repository served: a Git directory, or a worktree with one.
	Root string
	// Config is the `-c` switches git is started with, ahead of http-backend.
	Config []string
	// RemoteUser is who the backend is told authenticated the request. It is
	// the ident a push's reflog entries carry when the repository's own
	// configuration names nobody.
	RemoteUser string
	// FixedProtocol answers in protocol v0 whatever the client asked for,
	// rather than mapping its Git-Protocol header onto the backend.
	FixedProtocol bool
	// SysProcAttr is the identity the backend runs as: the repository's
	// owner, so git's dubious-ownership check (safe.directory) does not
	// reject the request. Nil runs it as the calling process.
	SysProcAttr *syscall.SysProcAttr
	// Start starts the backend. Nil is exec.Cmd.Start; a process that reaps
	// its orphans starts it through whatever keeps the reaper off it.
	Start func(*exec.Cmd) (Process, error)
}

// Serve runs b for one request, suffix being the path http-backend is asked
// for, and copies its CGI response back.
func Serve(w http.ResponseWriter, r *http.Request, b Backend, suffix string) {
	//nolint:gosec // The executable is fixed and the switches are the caller's; request data is passed through CGI env/stdin.
	cmd := exec.CommandContext(r.Context(), "git", append(slices.Clone(b.Config), "http-backend")...)
	cmd.Env = backendEnv(r, b, suffix)
	cmd.Stdin = r.Body
	cmd.SysProcAttr = b.SysProcAttr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Kept rather than piped: a pipe nobody reads while the backend runs
	// blocks it once git has said enough, and one read only after Wait has
	// already been closed under the reader.
	stderr := &tailBuffer{limit: 4096}
	cmd.Stderr = stderr
	// A hook the repository runs can leave something behind holding the
	// backend's output open; once the backend itself has exited, the request
	// does not wait on it for long.
	cmd.WaitDelay = 10 * time.Second
	start := b.Start
	if start == nil {
		start = func(cmd *exec.Cmd) (Process, error) { return cmd, cmd.Start() }
	}
	process, err := start(cmd)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	status, err := writeCGIResponse(w, stdout)
	if err != nil {
		_ = process.Wait()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := process.Wait(); err != nil && !errors.Is(r.Context().Err(), context.Canceled) {
		if status == 0 {
			http.Error(w, strings.TrimSpace(stderr.String()), http.StatusInternalServerError)
		}
	}
}

// backendEnv is the environment git http-backend runs in. It is built from
// nothing rather than inherited from the agent serving it, because of what
// sits on either side of this process: the agent runs as root, while the
// backend runs as the repository's owner over a repository someone else can
// write.
//
// The caller's identity comes first. Every path git resolves from it —
// $HOME/.gitconfig, the XDG attributes file — names a user this process is no
// longer, so git reads root's configuration where the owner is allowed to and
// warns where it is not, muxing that warning into the client's sideband:
//
//	remote: warning: unable to access '/root/.config/git/attributes': Permission denied
//
// The repository comes second. Its own .git/config names programs git runs —
// core.hooksPath, uploadpack.packObjectsHook — and repo-local configuration is
// read whatever the switches below say, so the repository's writer picks what
// this environment is handed to. Nothing the agent was started with — a
// bootstrap token among it — belongs there. PATH is kept, because git
// resolves what it execs through it, and nothing else is.
//
// So the only GIT_* variables the backend sees are the ones set here. An
// inherited one is never harmless: GIT_NAMESPACE empties the ref
// advertisement, GIT_CONFIG_COUNT with its GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_*
// pairs injects the very configuration GIT_CONFIG_NOSYSTEM and
// GIT_CONFIG_GLOBAL are switching off, and GIT_ALTERNATE_OBJECT_DIRECTORIES
// lends the repository objects it does not have.
func backendEnv(r *http.Request, b Backend, suffix string) []string {
	env := append(BaseEnv(),
		"GIT_PROJECT_ROOT="+b.Root,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO="+suffix,
		"REQUEST_METHOD="+r.Method,
		"QUERY_STRING="+r.URL.RawQuery,
		"REMOTE_USER="+b.RemoteUser,
	)
	// Which protocol version the client speaks is a request header, and mapping
	// it onto GIT_PROTOCOL is the HTTP server's job — http-backend answers v0 to
	// anyone who does not, advertising every ref on every request and negotiating
	// over more rounds than v2 needs. It belongs to the client and not to this
	// host — one more thing this environment must not pick up from the agent.
	if protocol := r.Header.Get("Git-Protocol"); protocol != "" && !b.FixedProtocol {
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

// BaseEnv is the part of the backend's environment every git run beside it
// gets too: PATH, and the switches that keep the system's and the agent's own
// configuration out of it.
func BaseEnv() []string {
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

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if over := len(b.data) - b.limit; over > 0 {
		b.data = b.data[over:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.data) }

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
