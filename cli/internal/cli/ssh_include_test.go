package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func enableSSHIncludesForTest(t *testing.T) {
	t.Helper()
	mustWriteFile(t, filepath.Join(cliStateDir(), "ssh", "include-preference"), string(sshIncludeEnabled))
}

func TestSSHIncludePreference(t *testing.T) {
	for _, yes := range []bool{false, true} {
		name := "decline"
		if yes {
			name = "accept"
		}
		t.Run(name, func(t *testing.T) {
			useTempSSHMachine(t)
			server := writeFakeServer().start(t)
			_, app := newRootCommand(false)
			app.serverURL = server.URL
			client, err := app.apiClient()
			if err != nil {
				t.Fatal(err)
			}
			target, err := localSSHTarget()
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			prompt := func(context.Context) (sshIncludePreference, error) {
				calls++
				return sshIncludeAnswer(yes), nil
			}
			notes := func(string, ...any) {}
			for range 2 {
				if err := app.writeProjectSSHConfig(t.Context(), client, "project-1", "", prompt, notes); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 1 {
				t.Fatalf("asked %d times, want once", calls)
			}
			if yes {
				if got := readFile(t, target.userConfig.local); strings.Count(got, "Include ") != 1 {
					t.Fatalf("SSH config = %q, want one Include", got)
				}
			} else if _, err := os.Stat(filepath.Dir(target.userConfig.local)); !os.IsNotExist(err) {
				t.Fatalf("SSH directory exists after declining: %v", err)
			}
		})
	}
}

func TestConfigSSHEnableAndDisablePreserveUserConfig(t *testing.T) {
	useTempSSHMachine(t)
	target, err := localSSHTarget()
	if err != nil {
		t.Fatal(err)
	}
	useTempServersFile(t)
	server := writeFakeServer().start(t)
	managed := target.configPath(sshConfigNamespace(server.URL, resolvedTestProjectID))
	other := filepath.Join(t.TempDir(), "discobox", "cli", "ssh", "proj_other", "config")
	mustWriteFile(t, other, managedConfigHeader("proj_other"))
	original := "\r\nInclude " + sshConfigPath(other) + "\r\n\r\n\r\nHost mine\r\n\tUser me  "
	mustWriteFile(t, target.userConfig.local, original)

	for _, action := range []string{"enable", "enable", "disable", "disable"} {
		cmd := NewRootCommand()
		cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "ssh-config", action})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("ssh-config %s: %v", action, err)
		}
		want := original
		if action == "enable" {
			want = "Include " + sshConfigPath(managed.client) + "\n" + original
		}
		if got := readFile(t, target.userConfig.local); got != want {
			t.Fatalf("SSH config after %s = %q, want %q", action, got, want)
		}
	}
}
