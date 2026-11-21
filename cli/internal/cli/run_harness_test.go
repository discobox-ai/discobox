package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestDetachedRunWithoutDefaultHarness(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		name := "without terminal"
		if interactive {
			name = "chooses project default"
		}
		t.Run(name, func(t *testing.T) {
			serveSSH := preparePromptCreateSSHSync(t)
			const projectPath = "/projects/project-1"
			const project = `{"id":"project-1","name":"P","ownerUserId":"user-1","default":true,"harnessChangesExplained":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
			var setDefault, created bool
			var projectReads int
			server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == projectPath:
					projectReads++
					_, _ = w.Write([]byte(project))
				case !interactive:
					t.Errorf("unexpected %s %s after reading the project", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				case r.Method == http.MethodGet && r.URL.Path == projectPath+"/harness-configs":
					_, _ = w.Write([]byte(`{"harnessConfigs":[{"id":"hc_codex","projectId":"project-1","name":"Codex","slug":"codex","builtIn":true,"runCommand":[],"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}]}`))
				case r.Method == http.MethodPut && r.URL.Path == projectPath+"/harness-configs/hc_codex/default":
					setDefault = true
					_, _ = w.Write([]byte(project))
				case r.Method == http.MethodPost && r.URL.Path == projectPath+"/sandboxes":
					if projectReads != 2 {
						t.Errorf("read project %d times before creation, want a fresh read after the picker", projectReads)
					}
					if !setDefault {
						t.Error("created a box before setting the default")
					}
					created = true
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"id":"` + runJSONSandboxID + `","projectId":"project-1","createdByUserId":"user-1","displayName":"run-test","config":{"name":"run-test","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}`))
				default:
					if !serveSSH(w, r) {
						t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusBadRequest)
					}
				}
			}))
			defer server.Close()

			cmd := NewRootCommand()
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetIn(strings.NewReader(""))
			cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "run", "-d", "discobox-ai/tour"})
			if !interactive {
				err := cmd.Execute()
				if err == nil || !strings.Contains(err.Error(), "-H <harness>") || !strings.Contains(err.Error(), "discobox admin harness set-default <harness>") {
					t.Fatalf("expected both remedies, got %v", err)
				}
				return
			}

			t.Setenv("TERM", "xterm-256color")
			master, tty, err := pty.Open()
			if err != nil {
				t.Skipf("no pty available: %v", err)
			}
			defer master.Close()
			defer tty.Close()
			if err := pty.Setsize(tty, &pty.Winsize{Rows: 24, Cols: 100}); err != nil {
				t.Fatal(err)
			}
			cmd.SetIn(tty)
			cmd.SetErr(tty)
			answered := make(chan error, 1)
			go func() {
				var visible bytes.Buffer
				buff := make([]byte, 4096)
				for {
					n, err := master.Read(buff)
					visible.Write(buff[:n])
					if bytes.Contains(visible.Bytes(), []byte("Choose a harness")) {
						_, err = master.Write([]byte("\r"))
						answered <- err
						return
					}
					if err != nil {
						answered <- err
						return
					}
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := cmd.ExecuteContext(ctx); err != nil {
				t.Fatal(err)
			}
			if !created || !strings.Contains(out.String(), runJSONSandboxID) {
				t.Fatalf("expected detached box output, got %q", out.String())
			}
			if err := <-answered; err != nil {
				t.Fatalf("could not answer picker: %v", err)
			}
		})
	}
}
