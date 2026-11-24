package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/spf13/cobra"
)

func TestGitCredentialPolicyPrecedenceAndEntrypoints(t *testing.T) {
	t.Setenv("DISCOBOX", "")
	t.Setenv("DISCOBOX_SERVER", "http://127.0.0.1:8080")
	path := filepath.Join(t.TempDir(), "client.yaml")
	t.Setenv("DISCOBOX_CLIENT_CONFIG_FILE", path)
	for _, command := range []string{"run", "attach", "start", "console", "configure"} {
		for _, tc := range []struct {
			saved string
			flags []string
			want  string
			fail  bool
		}{
			{"ask", nil, "ask", false}, {"local", nil, "local", false}, {"lend", nil, "lend", false},
			{"lend", []string{"--git-credentials=ask"}, "ask", false}, {"local", []string{"--git-credentials=lend"}, "lend", false},
			{"ask", []string{"--git-credentials=no"}, "", true},
		} {
			t.Run(command+tc.saved+strings.Join(tc.flags, ""), func(t *testing.T) {
				if err := os.WriteFile(path, []byte("git:\n  credentials: "+tc.saved+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				root, _ := newRootCommand(false)
				cmd, _, err := root.Find([]string{command})
				if err != nil {
					t.Fatal(err)
				}
				called := false
				cmd.RunE = func(cmd *cobra.Command, _ []string) error {
					called = true
					p, _ := sandboxcreate.GitCredentialPolicy(cmd.Context())
					if string(p) != tc.want {
						t.Fatalf("policy=%s want %s", p, tc.want)
					}
					return nil
				}
				root.SetOut(&bytes.Buffer{})
				root.SetErr(&bytes.Buffer{})
				args := append([]string{command}, tc.flags...)
				if command == "start" {
					args = append(args, "sbx_test")
				}
				root.SetArgs(args)
				err = root.Execute()
				if (err != nil) != tc.fail || called == tc.fail {
					t.Fatalf("called=%v err=%v", called, err)
				}
			})
		}
	}
	for _, tc := range []struct {
		flags []string
		want  string
		fail  bool
	}{
		{[]string{"--lend-git-credentials"}, "lend", false},
		{[]string{"--lend-git-credentials=false"}, "ask", false},
		{[]string{"--lend-git-credentials=false", "--git-credentials=local"}, "", true},
		{[]string{"--lend-git-credentials", "--git-credentials=lend"}, "", true},
	} {
		root, _ := newRootCommand(false)
		cmd, _, _ := root.Find([]string{"run"})
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			p, _ := sandboxcreate.GitCredentialPolicy(cmd.Context())
			if string(p) != tc.want {
				t.Fatalf("legacy policy=%s", p)
			}
			return nil
		}
		root.SetArgs(append([]string{"run"}, tc.flags...))
		if err := root.Execute(); (err != nil) != tc.fail {
			t.Fatalf("legacy %v: %v", tc.flags, err)
		}
	}
}

func TestSavedGitConsentIsReadOnUseButExplicitChoiceStays(t *testing.T) {
	t.Setenv("DISCOBOX", "")
	path := filepath.Join(t.TempDir(), "client.yaml")
	t.Setenv("DISCOBOX_CLIENT_CONFIG_FILE", path)
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("git:\n  credentials: "+value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("lend")
	ctx, err := resolveGitCredentials(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	write("ask")
	ctx, err = resolveGitCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if policy, _ := sandboxcreate.GitCredentialPolicy(ctx); policy != sandboxcreate.GitCredentialsAsk {
		t.Fatalf("revoked saved lending still effective: %s", policy)
	}
	explicit := context.WithValue(ctx, gitCredentialOverrideKey{}, sandboxcreate.GitCredentialsLocal)
	write("lend")
	explicit, err = resolveGitCredentials(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if policy, _ := sandboxcreate.GitCredentialPolicy(explicit); policy != sandboxcreate.GitCredentialsLocal {
		t.Fatalf("explicit choice changed: %s", policy)
	}
}

func TestNoninteractiveWindowNeverInstallsCredentialCallbacks(t *testing.T) {
	ctx := sandboxgit.WithNoninteractive(t.Context())
	ui := tui.CredentialUI{Confirm: func(context.Context, string) (bool, error) { t.Fatal("asked"); return false, nil }, Run: func(context.Context, *exec.Cmd) error { t.Fatal("terminal handoff"); return nil }}
	opts := tuiCredentialDeliveryOptions(ctx, ui)
	if opts.ConfirmCredential != nil || opts.RunCredential != nil {
		t.Fatal("noninteractive window can ask")
	}
}

func TestBareHelpIgnoresInvalidGitConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	t.Setenv("DISCOBOX_CLIENT_CONFIG_FILE", path)
	if err := os.WriteFile(path, []byte("git:\n  credentials: typo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root, _ := newRootCommand(false)
	root.SetArgs([]string{})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("help read config: %v", err)
	}
}

// The window's create pins a private remote source before any discobox exists,
// so before the pool's Git credentials choice can be offered. git's lookup in
// its turn is handed the terminal through the window's own runner (the one
// lending's credential fill takes), never left to ask on a terminal the window
// is drawn on; and nothing is created for a source that could not be pinned.
func TestWindowCreateHandsTheTerminalToALookupThatMustAsk(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(remote.Close)
	serveSettings := preparePromptCreateSSHSync(t)
	api := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && serveSettings(w, r) {
			return
		}
		t.Errorf("unexpected API request %s %s for a source that was never pinned", r.Method, r.URL.Path)
	}))
	t.Cleanup(api.Close)
	configFile := filepath.Join(t.TempDir(), "client.yaml")
	if err := os.WriteFile(configFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCOBOX_CLIENT_CONFIG_FILE", configFile)
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	client, err := apiclientgen.NewClient(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	var stray strings.Builder
	ds := &apiDataSource{app: &App{serverURL: api.URL, errOut: &stray}, client: client, projectID: "project-1"}
	url := remote.URL + "/o/r.git"
	var handed []string
	credentials := tui.CredentialUI{
		Run: func(_ context.Context, cmd *exec.Cmd) error {
			handed = append(handed, strings.Join(cmd.Args, " "))
			_, _ = cmd.Stderr.Write([]byte("fatal: Authentication failed for '" + url + "/'\n"))
			return errors.New("exit status 128")
		},
		Confirm: func(context.Context, string) (bool, error) {
			t.Fatal("the pool's Git credentials question was asked before any discobox exists")
			return false, nil
		},
	}
	_, err = ds.Run(t.Context(), tui.RunRequest{Source: url, Prompt: []string{"x"}, Credentials: credentials}, nil, nil, nil)
	if err == nil {
		t.Fatal("run of a source nobody gave a credential for succeeded")
	}
	if len(handed) != 1 || !strings.Contains(handed[0], "ls-remote") || !strings.Contains(handed[0], url) {
		t.Fatalf("terminal handed to %q, want the ls-remote of %s alone (run: %v)", handed, url, err)
	}
	if !strings.Contains(err.Error(), "gh auth setup-git") {
		t.Fatalf("run error = %v, want how to give git a credential", err)
	}
	if stray.Len() > 0 {
		t.Fatalf("the create wrote to the terminal the window is drawn on: %q", stray.String())
	}
}
