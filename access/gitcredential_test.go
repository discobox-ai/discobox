package access

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/cgi" //nolint:gosec // Runs git http-backend for the test, on a modern Go.
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// helperEnv, set, makes the test binary discobox-access itself, so a real git
// can run it as its credential helper.
const helperEnv = "DISCOBOX_ACCESS_TEST_AS_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		os.Exit(Run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

const githubSentinel = "dbx_sentinel_github"

func githubHeld() *fakeService {
	return &fakeService{
		credentials: []agentcreds.Credential{
			{Name: "claude", EnvVar: "CLAUDE_CODE_OAUTH_TOKEN", Purpose: agentcreds.PurposeUse, Hosts: []string{"api.anthropic.com"}},
			{Name: "github", EnvVar: "GH_TOKEN", Purpose: agentcreds.PurposeUse, Hosts: []string{"api.github.com", "github.com"}},
		},
		environment: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "dbx_sentinel_claude", "GH_TOKEN": githubSentinel}, //nolint:gosec // Placeholders, not credentials.
	}
}

func gitCredential(t *testing.T, operation, request string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = runGitCredential(t.Context(), []string{operation}, strings.NewReader(request), &out, &errOut)
	return out.String(), errOut.String(), code
}

// git is answered with the placeholder of the credential whose routes name
// the host it is fetching from, as Basic authorization's password.
func TestGitCredentialAnswersTheHostsCredential(t *testing.T) {
	serve(t, githubHeld())
	stdout, stderr, code := gitCredential(t, "get", "protocol=https\nhost=github.com\npath=acme/web.git\n\n")
	if code != exitOK || stderr != "" {
		t.Fatalf("get = %d, stderr %q", code, stderr)
	}
	if want := "username=x-access-token\npassword=" + githubSentinel + "\n"; stdout != want {
		t.Fatalf("get answered %q, want %q", stdout, want)
	}
}

// A git that takes a credential whole (2.46 and later) is given the Basic
// credential as ephemeral, so no later helper of the user's keeps it.
func TestGitCredentialIsEphemeralToAGitThatTakesOne(t *testing.T) {
	serve(t, githubHeld())
	stdout, _, _ := gitCredential(t, "get", "capability[]=authtype\ncapability[]=state\nprotocol=https\nhost=github.com\n")
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + githubSentinel))
	if want := "capability[]=authtype\nauthtype=Basic\ncredential=" + basic + "\nephemeral=1\n"; stdout != want {
		t.Fatalf("get answered %q, want %q", stdout, want)
	}
}

// A username git already has, from the remote's URL, is kept.
func TestGitCredentialKeepsTheUsernameGitHas(t *testing.T) {
	serve(t, githubHeld())
	stdout, _, _ := gitCredential(t, "get", "protocol=https\nhost=github.com:443\nusername=octocat\n")
	if want := "username=octocat\npassword=" + githubSentinel + "\n"; stdout != want {
		t.Fatalf("get answered %q, want %q", stdout, want)
	}
}

// Git hears nothing where no credential is clearly the host's: a host no
// route names, plain HTTP, a credential routed to any host, a delegation, a
// raw value, and a host two credentials name.
func TestGitCredentialAnswersNothingWithoutOneCredentialForTheHost(t *testing.T) {
	for name, tc := range map[string]struct {
		credentials []agentcreds.Credential
		request     string
		stderr      string
	}{
		"another host": {
			credentials: githubHeld().credentials,
			request:     "protocol=https\nhost=gitlab.com\n",
		},
		"plain http": {
			credentials: githubHeld().credentials,
			request:     "protocol=http\nhost=github.com\n",
		},
		"any host": {
			credentials: []agentcreds.Credential{{Name: "wide", EnvVar: "GH_TOKEN", Hosts: []string{"*"}}},
			request:     "protocol=https\nhost=github.com\n",
		},
		"a delegation": {
			credentials: []agentcreds.Credential{{Name: "github", Purpose: agentcreds.PurposeDelegate, Hosts: []string{"github.com"}}},
			request:     "protocol=https\nhost=github.com\n",
		},
		"a raw value": {
			credentials: []agentcreds.Credential{{Name: "github", EnvVar: "GH_TOKEN", Raw: agentcreds.RawEnv, Hosts: []string{"github.com"}}},
			request:     "protocol=https\nhost=github.com\n",
		},
		"two credentials": {
			credentials: []agentcreds.Credential{
				{Name: "mine", EnvVar: "GH_TOKEN", Hosts: []string{"github.com"}},
				{Name: "bot", EnvVar: "BOT_TOKEN", Hosts: []string{"github.com"}},
			},
			request: "protocol=https\nhost=github.com\n",
			stderr:  "github.com is a host of more than one credential (bot, mine)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc := githubHeld()
			svc.credentials = tc.credentials
			svc.environment["BOT_TOKEN"] = "dbx_sentinel_bot"
			serve(t, svc)
			stdout, stderr, code := gitCredential(t, "get", tc.request)
			if code != exitOK || stdout != "" {
				t.Fatalf("get = %d, answered %q; want nothing", code, stdout)
			}
			if !strings.Contains(stderr, tc.stderr) || (tc.stderr == "" && stderr != "") {
				t.Fatalf("stderr = %q, want %q", stderr, tc.stderr)
			}
		})
	}
}

// A listed host names a host exactly or, as `*.h`, the hosts beneath h, and
// nothing else; where two credentials' hosts both name it, neither is given,
// since which route wins is the egress's to say, not this helper's.
func TestGitCredentialReadsHostsAsTheProtocolDoes(t *testing.T) {
	credentials := []agentcreds.Credential{
		{Name: "corp", EnvVar: "CORP_TOKEN", Hosts: []string{"*.corp.example"}},
		{Name: "exact", EnvVar: "EXACT_TOKEN", Hosts: []string{"code.git.corp.example"}},
	}
	for host, want := range map[string]struct {
		name   string
		others []string
	}{
		"wiki.corp.example":     {name: "corp"},
		"corp.example":          {},
		"xcorp.example":         {},
		"code.git.corp.example": {others: []string{"corp", "exact"}},
	} {
		got, others := gitCredentialFor(credentials, host)
		gotName := ""
		if got != nil {
			gotName = got.Name
		}
		if gotName != want.name || !slices.Equal(others, want.others) {
			t.Errorf("%s: got %q and %v, want %q and %v", host, gotName, others, want.name, want.others)
		}
	}
}

// store and erase change nothing: the placeholder is the service's, and a
// refused push must not end the credential.
func TestGitCredentialStoresAndErasesNothing(t *testing.T) {
	svc := githubHeld()
	serve(t, svc)
	for _, operation := range []string{"store", "erase"} {
		stdout, stderr, code := gitCredential(t, operation, "protocol=https\nhost=github.com\nusername=x-access-token\npassword="+githubSentinel+"\n")
		if code != exitOK || stdout != "" || stderr != "" {
			t.Fatalf("%s = %d, %q, %q", operation, code, stdout, stderr)
		}
	}
}

// A service that cannot be reached leaves git to its other helpers rather
// than failing the command.
func TestGitCredentialWithoutTheServiceAnswersNothing(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	t.Setenv(agentcreds.URLEnv, server.URL)
	stdout, stderr, code := gitCredential(t, "get", "protocol=https\nhost=github.com\n")
	if code != exitOK || stdout != "" || !strings.Contains(stderr, "list the credentials you hold") {
		t.Fatalf("get = %d, %q, %q", code, stdout, stderr)
	}
}

// A real git, configured only with the helper, clones and pushes over HTTPS
// to a remote that takes nothing but the placeholder as Basic authorization's
// password — what the egress exchanges for the token. The remote is git's own
// smart HTTP server behind TLS; the test's host stands in for github.com.
func TestGitClonesAndPushesWithTheHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git http-backend runs as a CGI program here")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	execPath, err := exec.CommandContext(t.Context(), gitPath, "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("no git-http-backend")
	}

	root := t.TempDir()
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), gitPath, args...)
		cmd.Dir = dir
		cmd.Env = gitTestEnv(home, root)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	// The remote: a bare repository with one commit, served by git's own
	// smart HTTP backend to a request carrying the placeholder alone.
	repos := filepath.Join(root, "repos")
	seed := filepath.Join(root, "seed")
	git(root, "init", "-q", "-b", "main", seed)
	git(seed, "commit", "-q", "--allow-empty", "-m", "seed")
	git(root, "clone", "-q", "--bare", seed, filepath.Join(repos, "acme", "web.git"))
	git(filepath.Join(repos, "acme", "web.git"), "config", "http.receivepack", "true")
	var (
		mu         sync.Mutex
		authorized []string
	)
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || password != githubSentinel {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		authorized = append(authorized, user)
		mu.Unlock()
		(&cgi.Handler{
			Path: backend,
			Env:  []string{"GIT_PROJECT_ROOT=" + repos, "GIT_HTTP_EXPORT_ALL=1"},
		}).ServeHTTP(w, r)
	}))
	t.Cleanup(remote.Close)
	host := strings.TrimPrefix(remote.URL, "https://")
	hostname := host[:strings.LastIndexByte(host, ':')]

	svc := githubHeld()
	svc.credentials[1].Hosts = []string{hostname}
	serve(t, svc)

	// The box's system config, as core's git-credential step writes it, with
	// this test binary as discobox-access.
	system := filepath.Join(root, "gitconfig")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(system, []byte("[credential]\n\thelper = !"+shellQuote(exe)+" git-credential\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_SYSTEM", system)
	t.Setenv(helperEnv, "1")
	// The user's own helper, after it, which git sends every login that
	// worked: the placeholder must not be kept there.
	stored := filepath.Join(root, "git-credentials")
	if err := os.WriteFile(filepath.Join(root, "global-gitconfig"), []byte("[credential]\n\thelper = store --file "+shellQuote(stored)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	work := filepath.Join(root, "work")
	git(root, "-c", "http.sslVerify=false", "clone", "-q", remote.URL+"/acme/web.git", work)
	git(work, "commit", "-q", "--allow-empty", "-m", "from the box")
	git(work, "-c", "http.sslVerify=false", "push", "-q", "origin", "HEAD:refs/heads/main")
	if got := git(filepath.Join(repos, "acme", "web.git"), "log", "-1", "--format=%s", "main"); strings.TrimSpace(got) != "from the box" {
		t.Fatalf("remote main is at %q, want the pushed commit", got)
	}
	if kept, err := os.ReadFile(stored); err == nil && strings.Contains(string(kept), githubSentinel) {
		t.Fatalf("the user's store helper kept the placeholder: %q", kept)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(authorized) == 0 || authorized[0] != gitCredentialUsername {
		t.Fatalf("authorized as %v, want %s", authorized, gitCredentialUsername)
	}
}

// gitTestEnv is an environment that keeps the developer's own git config, and
// anything that would prompt, out of the test.
func gitTestEnv(home, root string) []string {
	env := []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(root, "global-gitconfig"),
		"GIT_CONFIG_SYSTEM=" + os.Getenv("GIT_CONFIG_SYSTEM"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	}
	for _, name := range []string{helperEnv, agentcreds.URLEnv} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
