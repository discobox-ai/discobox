package provision

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

func TestPackagesRequireSuccessfulIndexRefresh(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux apt-get")
	}
	if _, err := exec.LookPath("apt-get"); err != nil {
		t.Skip("requires apt-get")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "repository unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	for _, failedRefresh := range []bool{true, false} {
		name := "successful refresh"
		if failedRefresh {
			name = "failed refresh with satisfiable install"
		}
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			b.runner.LogDir = "/var/log/discobox/provision"
			b.runner.Tracker = NewTracker("")
			b.feature("f", map[string]string{provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: packages
    packages: {debian: [git]}
`})
			aptDir := t.TempDir()
			lists := filepath.Join(aptDir, "lists")
			if err := os.MkdirAll(filepath.Join(lists, "partial"), 0o755); err != nil {
				t.Fatal(err)
			}
			sources := filepath.Join(aptDir, "sources.list")
			source := ""
			if failedRefresh {
				source = "deb [trusted=yes] " + server.URL + " stable main\n"
			}
			if err := os.WriteFile(sources, []byte(source), 0o644); err != nil {
				t.Fatal(err)
			}
			installs := 0
			b.runner.Command = func(ctx context.Context, out io.Writer, dir string, env []string, tool string, args ...string) error {
				if tool != "apt-get" {
					t.Fatalf("unexpected tool %s", tool)
				}
				if len(args) > 0 && args[0] == "install" {
					installs++ // Model packages already installed; never change the host.
					_, err := io.WriteString(out, "git is already the newest version\n")
					return err
				}
				options := []string{
					"-o", "Dir::Etc::sourcelist=" + sources,
					"-o", "Dir::Etc::sourceparts=-",
					"-o", "Dir::State::lists=" + lists,
					"-o", "Dir::Cache=" + aptDir,
					"-o", "APT::Get::List-Cleanup=0",
					"-o", "Acquire::http::Proxy=DIRECT",
					"-o", "Acquire::Retries=0",
					"-o", "Acquire::http::Timeout=2",
					"-o", "Debug::NoLocking=1",
				}
				ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				return runCommand(ctx, out, dir, env, tool, append(options, args...)...)
			}
			outcomes, err := b.apply()
			_, recorded, recordErr := b.runner.Records.Step("f", "packages")
			if recordErr != nil {
				t.Fatal(recordErr)
			}
			status := b.runner.Tracker.Status()
			log := b.read("var/log/discobox/provision/f.log")
			if failedRefresh {
				if !strings.Contains(log, "Failed to fetch") {
					t.Fatalf("refresh did not encounter fetch failure:\n%s", log)
				}
				if err == nil {
					t.Errorf("provisioning succeeded after repository fetch failed; outcomes=%v", outcomes)
				}
				if installs != 0 {
					t.Errorf("ran %d installs after failed refresh, want 0", installs)
				}
				if recorded {
					t.Error("failed refresh recorded packages step as successful")
				}
				if status.Features["f"].State != provisionspec.StateFailed {
					t.Errorf("feature state = %s, want failed", status.Features["f"].State)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if installs != 1 || !recorded {
					t.Errorf("successful refresh: installs=%d recorded=%v", installs, recorded)
				}
			}
		})
	}
}
