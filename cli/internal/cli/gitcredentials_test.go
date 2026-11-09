package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
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
	opts := tuiCredentialDeliveryOptions(ctx, console.NewNarration())
	if opts.ConfirmCredential != nil || opts.RunCredential != nil {
		t.Fatal("noninteractive window can ask")
	}
	// Nobody narrating is nobody to ask, as an empty CredentialUI was.
	opts = tuiCredentialDeliveryOptions(t.Context(), nil)
	if opts.ConfirmCredential != nil || opts.RunCredential != nil {
		t.Fatal("a delivery nobody narrates can ask")
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
