package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const runJSONSandboxID = "sbx_9qk5n25t2hh2rv00"

// runJSONServer answers a `discobox new` the way the discobox API answers a
// discobox making another: the create, a secret to resolve by name, and the
// SSH sync refused by the sandbox role in plain text.
func runJSONServer(t *testing.T, posted *map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/sandboxes":
			defer r.Body.Close()
			if err := json.NewDecoder(r.Body).Decode(posted); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"` + runJSONSandboxID + `","projectId":"project-1","createdByUserId":"user-1","createdBySandboxId":"sbx_lead","displayName":"run-test","config":{"name":"run-test","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secrets":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"secrets":[{"id":"sec_npm","projectId":"project-1","name":"npm","type":"token","maxGrantTTLSeconds":0,"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1":
			http.Error(w, "not in the sandbox role", http.StatusForbidden)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// postedGrants are the grants a create carried, as the server read them.
func postedGrants(t *testing.T, posted map[string]any) []map[string]any {
	t.Helper()
	raw, _ := posted["grants"].([]any)
	grants := make([]map[string]any, 0, len(raw))
	for _, grant := range raw {
		grants = append(grants, grant.(map[string]any))
	}
	return grants
}

func grantUses(grant map[string]any) []string {
	var uses []string
	for _, use := range grant["uses"].([]any) {
		uses = append(uses, use.(map[string]any)["description"].(string))
	}
	return uses
}

// `new --grant` gives the new discobox what `admin box create --grant` does, a
// secret named by name reaching the server by its ID. A discobox making
// another is refused the SSH sync, and the create stands (ADR 26-09-24-630 §4).
func TestRunGivesItsGrants(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(newRunSourceTestRepo(t))
	var posted map[string]any
	server := runJSONServer(t, &posted)

	cmd := NewRootCommand()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "new", "-d", "--no-source",
		"--grant", "com.github.api=push a branch to org/repo",
		"--grant", "npm@registry.npmjs.org:NPM_TOKEN=publish @org/pkg",
		"-p", "fix issue 42"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute new: %v", err)
	}

	grants := postedGrants(t, posted)
	if len(grants) != 2 {
		t.Fatalf("grants = %#v, want two", posted["grants"])
	}
	if grants[0]["wellKnownId"] != "com.github.api" || grants[0]["secretId"] != nil ||
		strings.Join(grantUses(grants[0]), "|") != "push a branch to org/repo" {
		t.Fatalf("first grant = %#v, want com.github.api by its ID", grants[0])
	}
	if grants[1]["secretId"] != "sec_npm" || grants[1]["envVar"] != "NPM_TOKEN" || !slices.Equal(grantHostsOf(grants[1]), []string{"registry.npmjs.org"}) {
		t.Fatalf("second grant = %#v, want npm resolved to its ID", grants[1])
	}
	if !strings.Contains(out.String(), runJSONSandboxID) {
		t.Fatalf("output = %q, want the created discobox", out.String())
	}
	if !strings.Contains(errOut.String(), "SSH config not synced") {
		t.Fatalf("stderr = %q, want the skipped sync noted", errOut.String())
	}
}

// `new --json` takes the request from stdin, with no shell between the caller
// and the prompt or a use's sentence, and answers with the discobox as JSON.
func TestRunTakesItsRequestAsJSON(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(newRunSourceTestRepo(t))
	var posted map[string]any
	server := runJSONServer(t, &posted)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader(`{
		"prompt": "fix the 'failing' tests; don't \"skip\" any",
		"harness": "",
		"noSource": true,
		"env": ["MODE=test"],
		"grants": [
			{"id": "com.github.api", "uses": [{"description": "push a branch to org/repo"}, {"description": "open a PR"}]},
			{"secret": "npm", "envVar": "NPM_TOKEN", "uses": [{"description": "publish @org/pkg"}]}
		]
	}`))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "new", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute new --json: %v", err)
	}

	config := posted["config"].(map[string]any)
	prompt, _ := config["prompt"].([]any)
	if len(prompt) != 1 || prompt[0] != `fix the 'failing' tests; don't "skip" any` {
		t.Fatalf("prompt = %#v, want the request's one argument, quotes and all", config["prompt"])
	}
	if config["source"] != nil {
		t.Fatalf("source = %#v, want none for noSource", config["source"])
	}
	if env, _ := config["env"].(map[string]any); env["MODE"] != "test" {
		t.Fatalf("env = %#v, want MODE=test", config["env"])
	}
	grants := postedGrants(t, posted)
	if len(grants) != 2 || grants[0]["wellKnownId"] != "com.github.api" ||
		strings.Join(grantUses(grants[0]), "|") != "push a branch to org/repo|open a PR" ||
		grants[1]["secretId"] != "sec_npm" || grants[1]["envVar"] != "NPM_TOKEN" {
		t.Fatalf("grants = %#v, want both, the secret by its ID", posted["grants"])
	}
	var printed struct {
		ID                 string `json:"id"`
		CreatedBySandboxID string `json:"createdBySandboxId"`
	}
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil {
		t.Fatalf("stdout is not the discobox as JSON: %v\n%s", err, out.String())
	}
	if printed.ID != runJSONSandboxID || printed.CreatedBySandboxID != "sbx_lead" {
		t.Fatalf("printed = %+v, want the created discobox", printed)
	}
}

// A JSON request is the whole request: a flag or a word beside it is refused,
// and so is a field --json does not know or a grant --grant would have refused.
func TestRunJSONRefusesWhatItCannotMean(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		stdin string
		want  string
	}{
		"a run flag beside it": {[]string{"new", "--json", "-p", "x"}, `{}`, "put --prompt in it"},
		"a word beside it":     {[]string{"new", "--json", "fix"}, `{}`, `"prompt"`},
		"an unknown field":     {[]string{"new", "--json"}, `{"promt": "x"}`, `unknown field "promt"`},
		"two requests":         {[]string{"new", "--json"}, `{} {}`, "more than one request"},
		"a grant with no use":  {[]string{"new", "--json"}, `{"grants": [{"id": "com.github.api"}]}`, "at least one use"},
		"a grant naming both":  {[]string{"new", "--json"}, `{"grants": [{"id": "com.github.api", "secret": "gh", "envVar": "GH_TOKEN", "uses": [{"description": "x"}]}]}`, "not both"},
		"a secret with no var": {[]string{"new", "--json"}, `{"grants": [{"secret": "gh", "uses": [{"description": "x"}]}]}`, `"envVar"`},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := NewRootCommand()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetIn(strings.NewReader(tc.stdin))
			cmd.SetArgs(append([]string{"--server", "http://127.0.0.1:1", "--project", "project-1"}, tc.args...))
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to say %s", err, tc.want)
			}
		})
	}
}

// grantHostsOf is the hosts a posted grant names.
func grantHostsOf(grant map[string]any) []string {
	var out []string
	hosts, _ := grant["hosts"].([]any)
	for _, host := range hosts {
		if host, ok := host.(string); ok {
			out = append(out, host)
		}
	}
	return out
}

// `new --skills` and --json's "skills" read each directory's skills into the
// request as content, which is what the server installs (ADR 26-10-09-395 §4).
func TestRunCarriesItsSkills(t *testing.T) {
	skillsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(skillsDir, "foo", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "foo", "SKILL.md"), []byte("# foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "foo", "bin", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"flags": {"new", "-d", "--no-source", "--skills", skillsDir, "-p", "go"},
		"json":  {"new", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			setHome(t, t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Chdir(newRunSourceTestRepo(t))
			var posted map[string]any
			server := runJSONServer(t, &posted)
			request, _ := json.Marshal(map[string]any{"noSource": true, "skills": []string{skillsDir}})

			cmd := NewRootCommand()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetIn(bytes.NewReader(request))
			cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1"}, args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}

			skills, _ := posted["config"].(map[string]any)["skills"].(map[string]any)
			foo, _ := skills["foo"].(map[string]any)
			if foo["skill"] != "# foo" {
				t.Fatalf("skills = %#v, want foo's SKILL.md", skills)
			}
			files, _ := foo["files"].([]any)
			if len(files) != 1 {
				t.Fatalf("files = %#v, want bin/run.sh", foo["files"])
			}
			file := files[0].(map[string]any)
			if file["path"] != "bin/run.sh" || file["content"] != base64.StdEncoding.EncodeToString([]byte("#!/bin/sh\n")) {
				t.Fatalf("file = %#v, want bin/run.sh as base64", file)
			}
			// By its bit here, and by its #! line on Windows.
			if file["executable"] != true {
				t.Fatalf("file = %#v, want it executable", file)
			}
		})
	}
}

// client.yaml's new section is what `new` takes when its command line says
// nothing about skills, and a flag or a --json field given replaces it rather
// than adding to it (ADR 26-10-09-389).
func TestRunTakesItsSkillsFromClientConfig(t *testing.T) {
	writeSkill := func(t *testing.T, dir, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte("# "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		args []string
		json map[string]any
		want []string
	}{
		{name: "nothing given", args: []string{"new", "-d", "--no-source", "-p", "go"}, want: []string{"configured", "mine"}},
		{name: "--skills given", args: []string{"new", "-d", "--no-source", "--skills", "OTHER", "-p", "go"}, want: []string{"mine", "other"}},
		{name: "--user-skills=false", args: []string{"new", "-d", "--no-source", "--user-skills=false", "-p", "go"}, want: []string{"configured"}},
		{name: "json leaves both out", args: []string{"new", "--json"}, json: map[string]any{"noSource": true}, want: []string{"configured", "mine"}},
		{name: "json says none", args: []string{"new", "--json"}, json: map[string]any{"noSource": true, "skills": []string{}, "userSkills": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setHome(t, home)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			writeSkill(t, filepath.Join(home, ".claude", "skills"), "mine")
			// A relative directory in the file is relative to the file.
			configDir := t.TempDir()
			writeSkill(t, filepath.Join(configDir, "team"), "configured")
			configFile := filepath.Join(configDir, "client.yaml")
			if err := os.WriteFile(configFile, []byte("new:\n  skills: [team]\n  userSkills: true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DISCOBOX_CLIENT_CONFIG_FILE", configFile)
			other := t.TempDir()
			writeSkill(t, other, "other")
			args := slices.Clone(tc.args)
			if i := slices.Index(args, "OTHER"); i >= 0 {
				args[i] = other
			}
			t.Chdir(newRunSourceTestRepo(t))
			var posted map[string]any
			server := runJSONServer(t, &posted)
			request, _ := json.Marshal(tc.json)

			cmd := NewRootCommand()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetIn(bytes.NewReader(request))
			cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1"}, args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}

			skills, _ := posted["config"].(map[string]any)["skills"].(map[string]any)
			var got []string
			for name := range skills {
				got = append(got, name)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("skills = %v, want %v", got, tc.want)
			}
		})
	}
}

// A client.yaml that names a key nothing defines stops `new` with the key
// named, before anything is created.
func TestRunRefusesAClientConfigItCannotRead(t *testing.T) {
	setHome(t, t.TempDir())
	configFile := filepath.Join(t.TempDir(), "client.yaml")
	if err := os.WriteFile(configFile, []byte("new:\n  skils: [team]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCOBOX_CLIENT_CONFIG_FILE", configFile)
	cmd := NewRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--server", "http://127.0.0.1:1", "--project", "project-1", "new", "-d", "--no-source", "-p", "go"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "skils") || !strings.Contains(err.Error(), configFile) {
		t.Fatalf("execute error = %v, want one naming skils and %s", err, configFile)
	}
}
