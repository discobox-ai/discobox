package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi" //nolint:gosec // test-only Git smart HTTP backend, on a current Go
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/coder/websocket"
	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/pkg/execstream/resume"
)

// Each invocation executes the real Cobra tree in a fresh process, including
// creator cleanup. No LocalSources or in-memory recovery state crosses exits.
func TestDirectoryRetryCLIProcess(t *testing.T) {
	argsJSON := os.Getenv("DISCOBOX_TEST_DIRECTORY_RETRY_ARGS")
	if argsJSON == "" {
		return
	}
	t.Setenv("XDG_CONFIG_HOME", os.Getenv("DISCOBOX_TEST_DIRECTORY_RETRY_CONFIG"))
	xdg.Reload()
	var args []string
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.Fatal(err)
	}
	if len(args) > 0 && (args[0] == "console-create" || args[0] == "console-reopen") {
		client, err := api.NewClient(args[1])
		if err != nil {
			t.Fatal(err)
		}
		ds := &apiDataSource{app: &App{serverURL: args[1]}, client: client, projectID: "project-1"}
		if args[0] == "console-create" {
			_, err = ds.Run(t.Context(), tui.RunRequest{Source: args[2], CopyDirectory: "true"}, nil, nil, func(sb tui.Sandbox) { fmt.Fprintln(os.Stderr, "accepted", sb.ID) })
		} else {
			err = ds.DeliverSource(t.Context(), pushTestSandboxID, nil, tui.CredentialUI{})
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	cmd := NewRootCommand()
	cmd.SetArgs(args)
	cmd.SetIn(os.Stdin)
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
func directoryRetryProcess(t *testing.T, args []string) (string, error) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDirectoryRetryCLIProcess$")
	cmd.Env = append(os.Environ(), "DISCOBOX_TEST_DIRECTORY_RETRY_ARGS="+string(encoded), "DISCOBOX_TEST_DIRECTORY_RETRY_CONFIG="+os.Getenv("XDG_CONFIG_HOME"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}
func TestDirectoryDeliveryRecoversUsingPrintedAttachCommand(t *testing.T) {
	directoryRetryScenario(t, false, false)
}
func TestConsoleDirectoryDeliveryRecoversAfterExit(t *testing.T) {
	directoryRetryScenario(t, true, false)
}
func TestBindingFailureKeepsPrintedRetryAndConsoleCreatedBox(t *testing.T) {
	t.Run("command", func(t *testing.T) { directoryRetryScenario(t, false, true) })
	t.Run("console", func(t *testing.T) { directoryRetryScenario(t, true, true) })
}
func directoryRetryScenario(t *testing.T, console, bindFailure bool) {
	serveDefaults := preparePromptCreateSSHSync(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv(hostid.EnvVar, thisHost)
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(configDir, "discobox", "client", "pending-sources")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("original snapshot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	origin := filepath.Join(root, "projects", "project-1", "sandboxes", pushTestSandboxID, "git-origins", "primary.git")
	if err := os.MkdirAll(origin, 0700); err != nil {
		t.Fatal(err)
	}
	git := runSourceTestGit(t, origin)
	git("init", "--bare")
	git("config", "http.receivepack", "true")
	backend := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(git("--exec-path")), "git-http-backend"), Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	var mu sync.Mutex
	var sandbox map[string]any
	failDelivery, completed, attached := true, false, false
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/projects/default") {
			r = r.Clone(r.Context())
			copyURL := *r.URL
			r.URL = &copyURL
			r.URL.Path = strings.Replace(r.URL.Path, "/projects/default", "/projects/project-1", 1)
		}
		if strings.Contains(r.URL.Path, "/git-origins/") {
			if failDelivery {
				http.Error(w, "cold provisioning", http.StatusBadGateway)
				return
			}
			backend.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/sandboxes":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, "bad body", 400)
				return
			}
			config := body["config"].(map[string]any)
			source := config["source"].(map[string]any)
			source["slug"] = "primary"
			source["delivery"] = "push"
			sandbox = map[string]any{"id": pushTestSandboxID, "projectId": "project-1", "createdByUserId": "user-1", "displayName": "directory-retry", "config": config, "origin": body["origin"], "runtime": map[string]any{"state": "awaiting_source", "displayState": "starting", "desiredState": "present", "generation": 1, "observedGeneration": 1}, "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"}
			if bindFailure {
				entries, err := os.ReadDir(store)
				if err != nil {
					t.Error(err)
				}
				for _, entry := range entries {
					if entry.IsDir() {
						if err := os.Mkdir(filepath.Join(store, entry.Name(), "manifest.new"), 0700); err != nil {
							t.Error(err)
						}
					}
				}
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(sandbox)
		case strings.HasSuffix(r.URL.Path, "/complete-source-push"):
			completed = true
			sandbox["runtime"] = map[string]any{"state": "ready", "displayState": "running", "runtimeState": "running", "desiredState": "present", "generation": 1, "observedGeneration": 1, "sourceDeliveredAt": "2026-01-01T00:00:00Z"}
			_ = json.NewEncoder(w).Encode(sandbox)
		case r.URL.Path == "/projects/project-1/sandboxes/"+pushTestSandboxID:
			_ = json.NewEncoder(w).Encode(sandbox)
		case strings.HasSuffix(r.URL.Path, "/execs/primary/start"):
			_, _ = w.Write([]byte(`{"id":"ex_primary","status":"exited","exitCode":0,"command":["bash"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}`))
		case strings.HasSuffix(r.URL.Path, "/execs/primary"):
			_, _ = w.Write([]byte(`{"id":"ex_primary","status":"exited","exitCode":0,"command":["bash"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}`))
		case strings.HasSuffix(r.URL.Path, "/execs/primary/attach"):
			if !completed {
				t.Error("attached before source completion")
			}
			attached = true
			w.Header().Del("Content-Type")
			socket, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			conn := websocket.NetConn(r.Context(), socket, websocket.MessageBinary)
			defer conn.Close()
			hello, err := frame.Read(conn)
			if err != nil {
				t.Error(err)
				return
			}
			receiver, accepted, err := resume.NewServer().Accept(hello.Payload)
			if err != nil {
				t.Error(err)
				return
			}
			defer receiver.Close()
			if err := frame.Write(conn, frame.SessionOK, accepted); err != nil {
				t.Error(err)
				return
			}
			_ = frame.Write(conn, frame.Exit, []byte(`{"status":"exited","exitCode":0}`))
		default:
			if !serveDefaults(w, r) {
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
		}
	}))
	defer server.Close()
	argsCreate := []string{"--server", server.URL, "--project", "default", "run", "-d", dir}
	if console {
		argsCreate = []string{"console-create", server.URL, dir}
	}
	out, err := directoryRetryProcess(t, argsCreate)
	wantFailure := "502"
	if bindFailure {
		wantFailure = "bind source recovery"
	}
	if err == nil || !strings.Contains(out, wantFailure) || !strings.Contains(out, "snapshots retained at") {
		t.Fatalf("initial delivery = %v\n%s", err, out)
	}
	if console && bindFailure && !strings.Contains(out, "accepted "+pushTestSandboxID) {
		t.Fatalf("console lost created box: %s", out)
	}
	if bindFailure {
		entries, err := os.ReadDir(store)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				if err := os.Remove(filepath.Join(store, entry.Name(), "manifest.new")); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	start := strings.Index(out, "retry with ")
	if start < 0 {
		t.Fatal(out)
	}
	command := strings.SplitN(out[start+len("retry with "):], ": ", 2)[0]
	args, err := refreshcmd.Split(command)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) == 0 || args[0] != "discobox" || !strings.Contains(command, "project-1") {
		t.Fatalf("retry command = %q", command)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	failDelivery = false
	mu.Unlock()
	argsRetry := args[1:]
	if console {
		argsRetry = []string{"console-reopen", server.URL}
	}
	out, err = directoryRetryProcess(t, argsRetry)
	if err != nil {
		t.Fatalf("printed retry failed: %v\n%s\ncommand: %s", err, out, command)
	}
	mu.Lock()
	defer mu.Unlock()
	if !completed || (!console && !attached) {
		t.Fatalf("completed=%v attached=%v", completed, attached)
	}
	source := sandbox["config"].(map[string]any)["source"].(map[string]any)
	ref := source["workspace"].(map[string]any)["snapshotRef"].(string)
	if got := git("show", ref+":notes.txt"); got != "original snapshot\n" {
		t.Fatalf("delivered bytes = %q", got)
	}
	entries, err := os.ReadDir(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("snapshot retained after confirmed delivery: %s", entry.Name())
		}
	}
}
