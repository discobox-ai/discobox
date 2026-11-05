//go:build !windows

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creack/pty"
	"github.com/discobox-ai/discobox/pkg/endpoint"
	"github.com/spf13/cobra"
)

func TestRuntimePermissionConsent(t *testing.T) {
	for _, answer := range []string{"yes\n", "y\n", "no\n", "\n", "y\x04\x04", "\x04"} {
		t.Run(strings.ReplaceAll(answer, "\n", "newline"), func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("XDG_RUNTIME_DIR", base)
			child := filepath.Join(base, "discobox")
			if err := os.Mkdir(child, 0755); err != nil {
				t.Fatal(err)
			}
			master, tty, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer tty.Close()
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetIn(tty)
			cmd.SetOut(tty)
			cmd.SetErr(tty)
			if _, err := master.Write([]byte(answer)); err != nil {
				t.Fatal(err)
			}
			app := &App{serverURL: endpoint.DefaultEndpoint()}
			err = app.prepareRuntimeDirectories(cmd)
			approved := answer == "yes\n" || answer == "y\n"
			if approved != (err == nil) {
				t.Fatalf("answer %q: %v", answer, err)
			}
			st, _ := os.Stat(child)
			want := os.FileMode(0755)
			if approved {
				want = 0700
			}
			if st.Mode().Perm() != want {
				t.Fatalf("permissions %o", st.Mode().Perm())
			}
		})
	}
}

func TestRuntimePermissionNoninteractiveCommands(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"version"}, {"admin", "server", "status"}, {"start", "sbx_missing"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("XDG_RUNTIME_DIR", base)
			t.Setenv("DISCOBOX_SERVER", endpoint.DefaultEndpoint())
			child := filepath.Join(base, "discobox")
			if err := os.Mkdir(child, 0755); err != nil {
				t.Fatal(err)
			}
			cmd, _ := newRootCommand(false)
			var output bytes.Buffer
			cmd.SetIn(strings.NewReader("yes\n"))
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(context.Background())
			if err == nil || !strings.Contains(err.Error(), "rerun interactively") {
				t.Fatalf("error: %v; output: %s", err, output.String())
			}
			if !strings.Contains(output.String(), "0755 → 0700") || !strings.Contains(output.String(), "does not undo previous exposure") {
				t.Fatalf("missing explanation: %s", output.String())
			}
			st, _ := os.Stat(child)
			if st.Mode().Perm() != 0755 {
				t.Fatal("noninteractive consent changed permissions")
			}
		})
	}
}

func TestRuntimePermissionOfflineCommands(t *testing.T) {
	for _, args := range [][]string{{"version", "--client"}, {}, {"completion", "bash"}, {"admin", "remote"}, {"admin", "remote", "primary"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("XDG_RUNTIME_DIR", base)
			t.Setenv("DISCOBOX_SERVER", endpoint.DefaultEndpoint())
			useTempServersFile(t)
			child := filepath.Join(base, "discobox")
			_ = os.Mkdir(child, 0755)
			cmd, _ := newRootCommand(false)
			var output bytes.Buffer
			cmd.SetIn(strings.NewReader(""))
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(args)
			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), "unsafe permissions") {
				t.Fatal("offline command prompted")
			}
			st, _ := os.Stat(child)
			if st.Mode().Perm() != 0755 {
				t.Fatal("offline command mutated runtime directory")
			}
		})
	}
}

func TestRuntimePermissionRegisterCurrent(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", base)
	useTempServersFile(t)
	child := filepath.Join(base, "discobox")
	_ = os.Mkdir(child, 0755)
	if err := withServerRegistry(func(reg *serverRegistry) (bool, error) {
		reg.Servers = append(reg.Servers, registeredServer{Name: "other", Address: "http://127.0.0.1:1"})
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader("yes\n"))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	app := &App{serverURL: endpoint.DefaultEndpoint()}
	if err := app.setPrimaryServer(cmd, "other", true); err == nil || !strings.Contains(err.Error(), "rerun interactively") {
		t.Fatalf("expected consent refusal, got %v", err)
	}
	st, _ := os.Stat(child)
	if st.Mode().Perm() != 0755 {
		t.Fatal("mutated runtime permissions")
	}
}
