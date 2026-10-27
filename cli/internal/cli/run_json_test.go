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

// runJSONServer answers a `discobox run` the way the discobox API answers a
// discobox making another: the create, a secret to resolve by name, and the
// SSH sync refused by the sandbox role in plain text.
func runJSONServer(t *testing.T, posted *map[string]any) *httptest.Server {
	t.Helper()
	return runJSONServerWithSkills(t, posted, `{"skillsSources":[]}`, nil)
}

// runJSONServerWithSkills is runJSONServer answering the project's skills
// sources with skillsJSON, counting each read in asked when it is not nil.
func runJSONServerWithSkills(t *testing.T, posted *map[string]any, skillsJSON string, asked *int) *httptest.Server {
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
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/skills-sources":
			if asked != nil {
				*asked++
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(skillsJSON))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1":
			http.Error(w, "not in the sandbox role", http.StatusForbidden)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func postedBindings(t *testing.T, posted map[string]any) []map[string]any {
	t.Helper()
	raw, _ := posted["bindings"].([]any)
	bindings := make([]map[string]any, 0, len(raw))
	for _, binding := range raw {
		bindings = append(bindings, binding.(map[string]any))
	}
	return bindings
}

// `new --bind` and `--delegate` give the new discobox what `admin box create`
// does: the secret as written, which the server reads by name or ID. A
// discobox making another is refused the SSH sync, and the create stands
// (ADR 26-09-24-630 §4).
func TestRunGivesItsBindings(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(newRunSourceTestRepo(t))
	var posted map[string]any
	server := runJSONServer(t, &posted)

	cmd := NewRootCommand()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "run", "-d", "--no-source",
		"--bind", "npm:NPM_TOKEN@registry.npmjs.org",
		"--bind", "github",
		"--delegate", "openai",
		"--discobox-api",
		"-p", "fix issue 42"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute new: %v", err)
	}

	bindings := postedBindings(t, posted)
	if len(bindings) != 3 {
		t.Fatalf("bindings = %#v, want three", posted["bindings"])
	}
	if bindings[0]["secret"] != "npm" || bindings[0]["envVar"] != "NPM_TOKEN" || !slices.Equal(routeHostsOf(bindings[0]), []string{"registry.npmjs.org"}) {
		t.Fatalf("binding = %#v, want npm in NPM_TOKEN to its registry", bindings[0])
	}
	if bindings[1]["secret"] != "github" || bindings[1]["envVar"] != "GITHUB" {
		t.Fatalf("binding = %#v, want github in GITHUB", bindings[1])
	}
	if bindings[2]["secret"] != "openai" || bindings[2]["delegate"] != true || bindings[2]["use"] != false {
		t.Fatalf("binding = %#v, want openai only to give on", bindings[2])
	}
	if posted["discoboxApi"] != true {
		t.Fatalf("create = %#v, want --discobox-api carried", posted)
	}
	if !strings.Contains(out.String(), runJSONSandboxID) {
		t.Fatalf("output = %q, want the created discobox", out.String())
	}
	if !strings.Contains(errOut.String(), "SSH config not synced") {
		t.Fatalf("stderr = %q, want the skipped sync noted", errOut.String())
	}
}

// `run --from-json` takes the request from stdin, with no shell between the caller
// and the prompt, and answers with the discobox as JSON.
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
		"discoboxApi": true,
		"bindings": [
			{"secret": "npm", "envVar": "NPM_TOKEN", "routes": ["registry.npmjs.org"]}
		]
	}`))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "run", "--from-json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute run --from-json: %v", err)
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
	bindings := postedBindings(t, posted)
	if len(bindings) != 1 || bindings[0]["secret"] != "npm" || bindings[0]["envVar"] != "NPM_TOKEN" ||
		!slices.Equal(routeHostsOf(bindings[0]), []string{"registry.npmjs.org"}) {
		t.Fatalf("bindings = %#v, want the secret as given", posted["bindings"])
	}
	if posted["discoboxApi"] != true {
		t.Fatalf("create = %#v, want discoboxApi carried", posted)
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
// and so is a field --from-json does not know or a binding --bind would have refused.
func TestRunJSONRefusesWhatItCannotMean(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		stdin string
		want  string
	}{
		"a run flag beside it": {[]string{"run", "--from-json", "-p", "x"}, `{}`, "put --prompt in it"},
		"a word beside it":     {[]string{"run", "--from-json", "fix"}, `{}`, `"prompt"`},
		"an unknown field":     {[]string{"run", "--from-json"}, `{"promt": "x"}`, `unknown field "promt"`},
		"two requests":         {[]string{"run", "--from-json"}, `{} {}`, "more than one request"},
		"a well-known ID":      {[]string{"run", "--from-json"}, `{"bindings": [{"id": "com.github.api"}]}`, `unknown field "id"`},
		"no secret":            {[]string{"run", "--from-json"}, `{"bindings": [{"envVar": "T"}]}`, `"secret"`},
		"a variable twice":     {[]string{"run", "--from-json"}, `{"bindings": [{"secret": "gh", "envVar": "T"}, {"secret": "npm", "envVar": "T"}]}`, "already bound"},
		"a delegation's var":   {[]string{"run", "--from-json"}, `{"bindings": [{"secret": "gh", "envVar": "T", "delegate": true}]}`, "names no envVar"},
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

// routeHostsOf is the hosts of the routes a posted binding names.
func routeHostsOf(binding map[string]any) []string {
	var out []string
	routes, _ := binding["routes"].([]any)
	for _, route := range routes {
		if route, ok := route.(map[string]any); ok {
			if host, ok := route["host"].(string); ok {
				out = append(out, host)
			}
		}
	}
	return out
}

// `new --skills` and --from-json's "skills" read each directory's skills into the
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
		"flags": {"run", "-d", "--no-source", "--skills", skillsDir, "-p", "go"},
		"json":  {"run", "--from-json"},
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

// client.yaml's new section is what `run` takes when its command line says
// nothing about skills, and a flag or a --from-json field given replaces it rather
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
		{name: "nothing given", args: []string{"run", "-d", "--no-source", "-p", "go"}, want: []string{"configured", "mine"}},
		{name: "--skills given", args: []string{"run", "-d", "--no-source", "--skills", "OTHER", "-p", "go"}, want: []string{"mine", "other"}},
		{name: "--user-skills=false", args: []string{"run", "-d", "--no-source", "--user-skills=false", "-p", "go"}, want: []string{"configured"}},
		{name: "json leaves both out", args: []string{"run", "--from-json"}, json: map[string]any{"noSource": true}, want: []string{"configured", "mine"}},
		{name: "json says none", args: []string{"run", "--from-json"}, json: map[string]any{"noSource": true, "skills": []string{}, "userSkills": false}},
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
			if err := os.WriteFile(configFile, []byte("run:\n  skills: [team]\n  userSkills: true\n"), 0o644); err != nil {
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

// A client.yaml that names a key nothing defines stops `run` with the key
// named, before anything is created.
func TestRunRefusesAClientConfigItCannotRead(t *testing.T) {
	setHome(t, t.TempDir())
	configFile := filepath.Join(t.TempDir(), "client.yaml")
	if err := os.WriteFile(configFile, []byte("run:\n  skils: [team]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISCOBOX_CLIENT_CONFIG_FILE", configFile)
	cmd := NewRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--server", "http://127.0.0.1:1", "--project", "project-1", "run", "-d", "--no-source", "-p", "go"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "skils") || !strings.Contains(err.Error(), configFile) {
		t.Fatalf("execute error = %v, want one naming skils and %s", err, configFile)
	}
}
