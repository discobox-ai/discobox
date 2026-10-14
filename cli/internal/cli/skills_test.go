package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/clientconfig"
)

// A create reads the project's skills sources and checks each again before
// Git runs on it, and --no-project-skills does not read them at all (ADR
// 26-10-14-894 §2). The entry here is one the server refuses — a file URL,
// as a test repository has to be — so it stands for one a server let through:
// the create is refused, naming it and the way out.
func TestRunCopiesInTheProjectsSkillsSources(t *testing.T) {
	tools := "file://" + filepath.ToSlash(newRunSourceTestRepo(t))
	listing := `{"skillsSources":[{"id":"skills_1","projectId":"project-1","url":"` + tools + `","directory":".","createdAt":"2026-10-14T00:00:00Z"}]}`
	for _, tc := range []struct {
		name      string
		args      []string
		json      map[string]any
		wantAsked int
		wantErr   string
	}{
		{name: "by default", args: []string{"new", "-d", "--no-source", "-p", "go"}, wantAsked: 1, wantErr: "the project's skills source " + tools},
		{name: "--no-project-skills", args: []string{"new", "-d", "--no-source", "--no-project-skills", "-p", "go"}},
		{name: "json noProjectSkills", args: []string{"new", "--json"}, json: map[string]any{"noSource": true, "noProjectSkills": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHome(t, t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv(clientconfig.FileVar, "")
			t.Chdir(t.TempDir())
			var posted map[string]any
			asked := 0
			server := runJSONServerWithSkills(t, &posted, listing, &asked)
			request, _ := json.Marshal(tc.json)

			cmd := NewRootCommand()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetIn(bytes.NewReader(request))
			cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1"}, tc.args...))
			err := cmd.Execute()
			if asked != tc.wantAsked {
				t.Fatalf("the project's skills sources were read %d times, want %d", asked, tc.wantAsked)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "--no-project-skills") {
					t.Fatalf("execute = %v, want a refusal naming %q and --no-project-skills", err, tc.wantErr)
				}
				if posted != nil {
					t.Fatalf("a create was posted: %#v", posted)
				}
				return
			}
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if sources, _ := posted["config"].(map[string]any)["skillsSources"].([]any); len(sources) != 0 {
				t.Fatalf("skillsSources = %#v, want none", sources)
			}
		})
	}
}

// client.yaml is asked first on rm, so a Git URL kept there, and a directory
// whatever its name, come out of the file and never reach the project.
func TestSkillsRemoveTakesClientEntriesFromTheFile(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	configFile := filepath.Join(t.TempDir(), "client.yaml")
	t.Setenv(clientconfig.FileVar, configFile)
	dir := filepath.Join(home, "skills_shared")
	if err := os.WriteFile(configFile, []byte("new:\n  skills:\n    - https://github.com/acme/tools//skills\n    - "+dir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "no", http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	t.Chdir(home)

	var errOut bytes.Buffer
	cmd := NewRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "skills", "rm", "https://github.com/acme/tools//skills", "skills_shared"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rm: %v\n%s", err, errOut.String())
	}
	cfg, err := clientconfig.Load()
	if err != nil || len(cfg.New.Skills) != 0 {
		t.Fatalf("client.yaml skills = %q (%v), want none", cfg.New.Skills, err)
	}
}

// `discobox skills add` files a Git URL with the project and a directory in
// client.yaml; `ls` lists both, and `rm` takes each out of where it is.
func TestSkillsCommandFilesEachValueByWhatItIs(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	configFile := filepath.Join(t.TempDir(), "client.yaml")
	t.Setenv(clientconfig.FileVar, configFile)
	dir := filepath.Join(home, "team-skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	var sources []map[string]any
	var posted map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/skills-sources":
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Fatal(err)
			}
			source := map[string]any{"id": "skills_abc", "projectId": "project-1", "url": posted["url"], "ref": posted["ref"], "directory": posted["directory"], "createdAt": "2026-10-14T00:00:00Z"}
			sources = append(sources, source)
			_ = json.NewEncoder(w).Encode(source)
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/skills-sources":
			_ = json.NewEncoder(w).Encode(map[string]any{"skillsSources": sources})
		case r.Method == http.MethodDelete && r.URL.Path == "/projects/project-1/skills-sources/skills_abc":
			sources = nil
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	run := func(args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		cmd := NewRootCommand()
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "skills"}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("skills %v: %v\n%s", args, err, errOut.String())
		}
		return out.String()
	}

	run("add", "https://github.com/acme/tools@v2//agents/skills", "~/team-skills")
	if posted["url"] != "https://github.com/acme/tools" || posted["ref"] != "v2" || posted["directory"] != "agents/skills" {
		t.Fatalf("posted = %#v", posted)
	}
	cfg, err := clientconfig.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.New.Skills) != 1 || cfg.New.Skills[0] != dir {
		t.Fatalf("client.yaml skills = %q, want %s", cfg.New.Skills, dir)
	}

	listed := run("ls")
	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(listed), "\n")[1:] {
		rows = append(rows, strings.Join(strings.Fields(line), " "))
	}
	if want := []string{"project https://github.com/acme/tools@v2//agents/skills skills_abc", "client.yaml " + dir}; strings.Join(rows, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ls = %q, want %q", rows, want)
	}

	run("rm", "https://github.com/acme/tools@v2//agents/skills", dir)
	if len(sources) != 0 {
		t.Fatalf("project sources = %v, want none", sources)
	}
	if cfg, err = clientconfig.Load(); err != nil || len(cfg.New.Skills) != 0 {
		t.Fatalf("client.yaml skills = %v (%v), want none", cfg.New.Skills, err)
	}
}
