package githttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestBackendEnvForwardsContentEncoding pins the CGI variable git http-backend
// needs to inflate a compressed request. git gzips an upload-pack request once
// negotiation runs past a round or two, so without this a fetch works for a
// small negotiation and fails for a large one — the backend reads gzip bytes as
// pkt-line, answers nothing, and the client reports that the remote end hung up.
func TestBackendEnvForwardsContentEncoding(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/repo.git/git-upload-pack", strings.NewReader("body"))
	r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	r.Header.Set("Content-Encoding", "gzip")

	env := backendEnv(r, "/srv/repo", "/git-upload-pack")
	if !slices.Contains(env, "HTTP_CONTENT_ENCODING=gzip") {
		t.Fatalf("HTTP_CONTENT_ENCODING missing from CGI environment: %v", env)
	}
}

func TestBackendEnvOmitsContentEncodingWhenAbsent(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/repo.git/git-upload-pack", strings.NewReader("body"))
	env := backendEnv(r, "/srv/repo", "/git-upload-pack")
	for _, entry := range env {
		if strings.HasPrefix(entry, "HTTP_CONTENT_ENCODING=") {
			t.Fatalf("unencoded request must not claim an encoding: %q", entry)
		}
	}
}

// TestBackendEnvIsNotInheritedFromTheAgent pins that the backend's environment
// is built rather than inherited. The agent runs as root and holds the pool's
// bootstrap token; the backend runs as the repository's owner over a worktree
// the sandbox can write, whose .git/config can name the program git runs there.
// So everything the agent was started with stays with the agent, PATH aside.
func TestBackendEnvIsNotInheritedFromTheAgent(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/root")
	t.Setenv("XDG_CONFIG_HOME", "/root/.config")
	t.Setenv("DISCOBOX_POOL_BOOTSTRAP_TOKEN", "secret")
	// Neither GIT_CONFIG_NOSYSTEM nor GIT_CONFIG_GLOBAL covers these: the first
	// injects configuration of its own, the second empties the advertisement.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_NAMESPACE", "hidden")

	r := httptest.NewRequestWithContext(t.Context(), "GET", "/repo.git/info/refs?service=git-upload-pack", nil)
	env := backendEnv(r, "/srv/repo", "/info/refs")

	want := []string{
		"PATH=/usr/bin",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_PROJECT_ROOT=/srv/repo",
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO=/info/refs",
		"REQUEST_METHOD=GET",
		"QUERY_STRING=service=git-upload-pack",
		"REMOTE_USER=pool-agent",
		"CONTENT_LENGTH=0",
	}
	if !slices.Equal(env, want) {
		t.Fatalf("CGI environment = %v, want %v", env, want)
	}
}

// TestBackendEnvForwardsGitProtocol pins the CGI variable that decides which
// wire protocol the backend speaks. Without it http-backend answers v0, which
// advertises every ref on every request; the version is the client's to choose,
// so it comes from the request header and from nowhere else.
func TestBackendEnvForwardsGitProtocol(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/repo.git/info/refs?service=git-upload-pack", nil)
	r.Header.Set("Git-Protocol", "version=2")

	env := backendEnv(r, "/srv/repo", "/info/refs")
	if !slices.Contains(env, "GIT_PROTOCOL=version=2") {
		t.Fatalf("GIT_PROTOCOL missing from CGI environment: %v", env)
	}
}

// A client that names no version is answered in the version http-backend
// defaults to, not in one the pool host's own environment happened to hold.
func TestBackendEnvOmitsGitProtocolWhenAbsent(t *testing.T) {
	t.Setenv("GIT_PROTOCOL", "version=2")

	r := httptest.NewRequestWithContext(t.Context(), "GET", "/repo.git/info/refs?service=git-upload-pack", nil)
	env := backendEnv(r, "/srv/repo", "/info/refs")
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_PROTOCOL=") {
			t.Fatalf("request naming no protocol version must not claim one: %q", entry)
		}
	}
}

// TestServeBackendAnswersInTheProtocolVersionTheClientAsked runs the real
// backend over a real repository, because the header-to-CGI mapping above is
// only worth anything if git acts on it: a client asking for v2 gets the v2
// capability list, and one asking for nothing still gets the v0 advertisement
// every older client expects.
func TestServeBackendAnswersInTheProtocolVersionTheClientAsked(t *testing.T) {
	repo := initTestRepository(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, suffix, ok := ParseRepositoryPath(strings.TrimPrefix(r.URL.Path, "/"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		ServeBackend(w, r, repo, suffix, -1, -1)
	}))
	defer server.Close()

	advertisement := func(protocol string) string {
		r, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/repo.git/info/refs?service=git-upload-pack", nil)
		if err != nil {
			t.Fatal(err)
		}
		if protocol != "" {
			r.Header.Set("Git-Protocol", protocol)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("info/refs status = %d, body = %s", resp.StatusCode, body)
		}
		return string(body)
	}

	if got := advertisement("version=2"); !strings.Contains(got, "version 2") {
		t.Fatalf("a client asking for protocol v2 was answered %q", got)
	}
	if got := advertisement(""); !strings.Contains(got, "# service=git-upload-pack") {
		t.Fatalf("a client asking for no version was answered %q", got)
	}
}

func initTestRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "README.md"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "one"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}
