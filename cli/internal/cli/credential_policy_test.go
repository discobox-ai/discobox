package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func writeCLIPolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const cliRepositoryPolicy = `{"version":1,"parameters":{"owner":{"type":"pathSegment"},"repo":{"type":"pathSegment"},"suffix":{"type":"pathSegment","optional":true,"default":""}},"routes":[{"hosts":["API.GITHUB.COM"],"methods":["post"],"paths":["/repos/${owner}/${repo}/issues${suffix}"]}]}`

func executePolicyCommand(server string, args ...string) error {
	cmd := NewRootCommand()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(append([]string{"--server", server, "--project", "project-1", "-o", "json"}, args...))
	return cmd.Execute()
}

func wantCLIPolicyRoutes(t *testing.T, body map[string]any) {
	t.Helper()
	want := []any{
		map[string]any{"hosts": []any{"api.github.com"}, "methods": []any{"POST"}, "paths": []any{"/repos/acme/widget/issues"}},
		map[string]any{"hosts": []any{"api.github.com"}, "methods": []any{"POST"}, "paths": []any{"/repos/other/gadget/issues-extra"}},
	}
	if !reflect.DeepEqual(body["routes"], want) {
		t.Fatalf("routes = %#v, want %#v", body["routes"], want)
	}
	for _, key := range []string{"policy", "policies", "parameters", "policyArgs"} {
		if _, ok := body[key]; ok {
			t.Fatalf("local policy field %q reached API: %#v", key, body)
		}
	}
}

// Repeated flags preserve policy-local argument scope, including a second use
// of the same file, and every binding entry point sends concrete route unions.
func TestCredentialPolicyBindingCommandsResolveIndependentArguments(t *testing.T) {
	path := writeCLIPolicy(t, cliRepositoryPolicy)
	flags := []string{"--policy", path, "--policy-arg", "owner=acme", "--policy-arg", "repo=widget",
		"--policy", path, "--policy-arg", "owner=other", "--policy-arg", "repo=gadget", "--policy-arg", "suffix=-extra"}
	for _, entry := range []struct {
		name   string
		args   []string
		method string
	}{
		{"secret bind", []string{"secret", "bind", "github", "--project-wide", "--as", "GH_TOKEN"}, http.MethodPost},
		{"admin create", []string{"admin", "binding", "create", "--secret", "github", "--target-kind", "project", "--as", "GH_TOKEN"}, http.MethodPost},
		{"admin update", []string{"admin", "binding", "update", "sbd_1"}, http.MethodPatch},
	} {
		t.Run(entry.name, func(t *testing.T) {
			var posted map[string]any
			var writes atomic.Int32
			server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != entry.method || !strings.HasPrefix(r.URL.Path, "/projects/project-1/secret-bindings") {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 500)
					return
				}
				writes.Add(1)
				if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
					t.Error(err)
				}
				_, _ = w.Write([]byte(bindTestUseBinding))
			}))
			defer server.Close()
			if err := executePolicyCommand(server.URL, append(entry.args, flags...)...); err != nil {
				t.Fatal(err)
			}
			if writes.Load() != 1 {
				t.Fatalf("writes = %d, want 1", writes.Load())
			}
			wantCLIPolicyRoutes(t, posted)
			if entry.method == http.MethodPatch && len(posted) != 1 {
				t.Fatalf("policy reapply changes unrelated fields: %#v", posted)
			}
		})
	}
}

func TestCredentialPolicyRunBindsMergedSnapshot(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(newRunSourceTestRepo(t))
	path := writeCLIPolicy(t, cliRepositoryPolicy)
	var posted map[string]any
	server := runJSONServer(t, &posted)
	bind := "github:GH_TOKEN,policy=" + path + ",owner=acme,repo=widget,policy=" + path + ",owner=other,repo=gadget,suffix=-extra"
	if err := executePolicyCommand(server.URL, "run", "-d", "--no-source", "--bind", bind, "-p", "file an issue"); err != nil {
		t.Fatal(err)
	}
	bindings := postedBindings(t, posted)
	if len(bindings) != 1 {
		t.Fatalf("bindings = %#v, want one", bindings)
	}
	if bindings[0]["secret"] != "github" || bindings[0]["envVar"] != "GH_TOKEN" {
		t.Fatalf("binding = %#v", bindings[0])
	}
	wantCLIPolicyRoutes(t, bindings[0])
}

// A later invalid policy cannot leave a partially applied binding or sandbox.
func TestCredentialPolicyInvalidInputsSubmitNoMutations(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(newRunSourceTestRepo(t))
	valid := writeCLIPolicy(t, cliRepositoryPolicy)
	invalid := writeCLIPolicy(t, `{"version":1,"routes":[]}`)
	missing := filepath.Join(t.TempDir(), "missing.json")
	first := []string{"--policy", valid, "--policy-arg", "owner=acme", "--policy-arg", "repo=widget"}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bind later file invalid", append(append([]string{"secret", "bind", "github", "--project-wide"}, first...), "--policy", invalid), "nonempty"},
		{"bind missing parameter", []string{"secret", "bind", "github", "--project-wide", "--policy", valid, "--policy-arg", "owner=acme"}, "repo"},
		{"bind unknown parameter", append(append([]string{"secret", "bind", "github", "--project-wide"}, first...), "--policy-arg", "typo=x"), "unknown parameter"},
		{"bind duplicate argument", append(append([]string{"secret", "bind", "github", "--project-wide"}, first...), "--policy-arg", "repo=other"), "repeated"},
		{"argument before file", []string{"secret", "bind", "github", "--project-wide", "--policy-arg", "repo=widget", "--policy", valid}, "preceding policy"},
		{"bind inline conflict", append(append([]string{"secret", "bind", "github", "--project-wide"}, first...), "--route", "api.github.com"), "mutually exclusive"},
		{"update clear conflict", append(append([]string{"admin", "binding", "update", "sbd_1"}, first...), "--clear-route"), "mutually exclusive"},
		{"create missing file", []string{"admin", "binding", "create", "--secret", "github", "--target-kind", "project", "--as", "GH_TOKEN", "--policy", missing}, "missing.json"},
		{"run later invalid", []string{"run", "-d", "--no-source", "--bind", "github,policy=" + valid + ",owner=acme,repo=widget,policy=" + invalid}, "nonempty"},
		{"run inline conflict", []string{"run", "-d", "--no-source", "--bind", "github@api.github.com,policy=" + valid + ",owner=acme,repo=widget"}, "mutually exclusive"},
		{"run wildcard argument", []string{"run", "-d", "--no-source", "--bind", "github,policy=" + valid + ",owner=acme,repo=*"}, "literal path segment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/projects/project-1/skills-sources" {
					_, _ = w.Write([]byte(`{"skillsSources":[]}`))
					return
				}
				http.Error(w, "unexpected request", http.StatusForbidden)
			}))
			defer server.Close()
			err := executePolicyCommand(server.URL, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if writes.Load() != 0 {
				t.Fatalf("invalid policy submitted %d mutations", writes.Load())
			}
		})
	}
}
