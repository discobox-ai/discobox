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
	"strconv"
	"strings"

	"github.com/obot-platform/discobox/pool-agent/execidentity"
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

// ServeBackend runs git http-backend against repoPath as the given uid/gid, so
// its process ownership matches the worktree's owner and git's dubious-ownership
// check (safe.directory) doesn't reject the request. A negative uid means "run
// as the calling process" (used when there is no specific owner to impersonate).
func ServeBackend(w http.ResponseWriter, r *http.Request, repoPath, suffix string, uid, gid int) {
	//nolint:gosec // The executable and arguments are fixed; request data is passed through CGI env/stdin.
	cmd := exec.CommandContext(r.Context(),
		"git",
		"-c", "http.receivepack=true",
		"-c", "receive.denyCurrentBranch=updateInstead",
		"http-backend",
	)
	cmd.Env = backendEnv(r, repoPath, suffix)
	cmd.Stdin = r.Body
	cmd.SysProcAttr = execidentity.SysProcAttr(uid, gid)
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
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	status, err := writeCGIResponse(w, stdout)
	if err != nil {
		_ = cmd.Wait()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := cmd.Wait(); err != nil && !errors.Is(r.Context().Err(), context.Canceled) {
		data, _ := io.ReadAll(io.LimitReader(stderr, 4096))
		if status == 0 {
			http.Error(w, strings.TrimSpace(string(data)), http.StatusInternalServerError)
		}
	}
}

func backendEnv(r *http.Request, repoPath, suffix string) []string {
	env := append(os.Environ(),
		"GIT_PROJECT_ROOT="+repoPath,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO="+suffix,
		"REQUEST_METHOD="+r.Method,
		"QUERY_STRING="+r.URL.RawQuery,
		"REMOTE_USER=pool-agent",
	)
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
