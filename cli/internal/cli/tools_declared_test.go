package cli

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/tools"
)

// sourceDeclaresTools is a sandbox whose primary source declares a reviewer of
// its own, a program for this machine, and a replacement for the CLI's vscode.
// Only the first may stand (ADR 0125 §5).
const sourceDeclaresTools = `{"tools":[
	{"id":"review","name":"review","runs":"sandbox","layer":"source","fileName":"review.yaml","script":false,"program":["reviewdog"]},
	{"id":"open","name":"open","runs":"host","layer":"source","fileName":"open.yaml","script":false,"program":["open"],
	 "problem":"a tool that runs on your machine cannot be declared by the discobox's source"},
	{"id":"vscode","name":"vscode","runs":"sandbox","layer":"source","fileName":"vscode.yaml","script":false,"program":["code-server"]}
]}`

func TestToolsListShowsWhereEachToolCameFromAndWhatCannotRun(t *testing.T) {
	fake := vscodeFakeServer()
	fake.tools = sourceDeclaresTools
	home := t.TempDir()
	declareUserTool(t, home, "lint.yaml", "description: lint the box\nprogram: golangci-lint\n")

	setHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := fake.start(t)
	cmd := NewRootCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "tools", "--discobox-id", "sbx_devbox00000001", "ls"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("tools ls: %v\n%s", err, out.String())
	}

	listing := out.String()
	for _, want := range []string{"review", "lint", "zed"} {
		if !strings.Contains(listing, want) {
			t.Errorf("the listing has no %s:\n%s", want, listing)
		}
	}
	if !strings.Contains(listing, "cannot run: a tool that runs on your machine") {
		t.Errorf("the source's host tool is not listed as refused:\n%s", listing)
	}
	if !strings.Contains(listing, "the discobox's source cannot replace it") {
		t.Errorf("the source's vscode is not listed as refused:\n%s", listing)
	}
}

// The CLI's vscode stays the one that runs, whatever the discobox declares
// under the same name.
func TestADiscoboxCannotReplaceAHostTool(t *testing.T) {
	record := fakeVSCode(t)
	fake := vscodeFakeServer()
	fake.tools = sourceDeclaresTools
	if _, _, _, err := runToolsVSCodeCmd(t, fake, "--discobox-id", "sbx_devbox00000001"); err != nil {
		t.Fatalf("execute tools vscode: %v", err)
	}
	if args := editorArgs(t, record); !contains(args, "--folder-uri") {
		t.Fatalf("editor args = %v, want the CLI's vscode", args)
	}
}

// A tool only the discobox declares is still `discobox tools <id>`, and one it
// may not declare says why instead of running.
func TestAToolTheDiscoboxDeclaresForThisMachineIsRefused(t *testing.T) {
	fake := vscodeFakeServer()
	fake.tools = sourceDeclaresTools
	_, _, _, err := runToolsCmd(t, fake, t.TempDir(), "open", "--discobox-id", "sbx_devbox00000001")
	if err == nil || !strings.Contains(err.Error(), "runs on your machine") {
		t.Fatalf("err = %v, want the refusal", err)
	}
	_, _, _, err = runToolsCmd(t, fake, t.TempDir(), "nope", "--discobox-id", "sbx_devbox00000001")
	if err == nil || !strings.Contains(err.Error(), "tools ls") {
		t.Fatalf("err = %v, want a pointer to the listing", err)
	}
}

// A server or sandbox agent that predates the tools route has no tools of its
// own to offer, and that must not take the CLI's with it. http.NotFound is what
// such a server answers with.
func TestAServerWithoutTheToolsRouteStillRunsTheCLIsTools(t *testing.T) {
	record := fakeVSCode(t)
	fake := vscodeFakeServer()
	inner := fake.start(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tools") {
			http.NotFound(w, r)
			return
		}
		proxy, err := http.NewRequestWithContext(r.Context(), r.Method, inner.URL+r.URL.RequestURI(), r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		proxy.Header = r.Header
		res, err := http.DefaultClient.Do(proxy)
		if err != nil {
			t.Error(err)
			return
		}
		defer res.Body.Close()
		for k, v := range res.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(res.StatusCode)
		_, _ = io.Copy(w, res.Body)
	}))
	t.Cleanup(server.Close)

	home := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cmd := NewRootCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "tools", "vscode", "--discobox-id", "sbx_devbox00000001"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("tools vscode: %v\n%s", err, out.String())
	}
	if args := editorArgs(t, record); !contains(args, "--folder-uri") {
		t.Fatalf("editor args = %v", args)
	}
}

// A host script has no args of its own to put placeholders in, so it is handed
// the discobox in its environment.
func TestAHostScriptIsHandedTheDiscoboxInItsEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script is a shell script")
	}
	home := t.TempDir()
	record := filepath.Join(t.TempDir(), "env")
	declareUserTool(t, home, "gitk.sh", "#!/bin/sh\n#---\n# runs: host\n#---\n"+
		"printf '%s\\n' \"$DISCOBOX_SSH_HOST\" \"$DISCOBOX_GIT_URL\" \"$DISCOBOX_WORKDIR\" \"$1\" > "+record+"\n")
	if err := os.Chmod(filepath.Join(home, ".config", "discobox", "tools", "gitk.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, stderr, err := runToolsCmd(t, vscodeFakeServer(), home, "gitk", "--discobox-id", "sbx_devbox00000001", "--", "--all"); err != nil {
		t.Fatalf("tools gitk: %v\n%s", err, stderr)
	}
	want := "devbox\nssh://devbox/home/agent/repo\n/home/agent/repo\n--all\n"
	if got := readFile(t, record); got != want {
		t.Fatalf("the script saw %q, want %q", got, want)
	}
}

func TestSandboxToolCommand(t *testing.T) {
	yaml := tools.Definition{ID: "fresh", Program: []string{"fresh"}, Args: []string{"."}, Layer: tools.LayerImage}
	if got := strings.Join(sandboxToolCommand(yaml, []string{"x"}), " "); got != "fresh . x" {
		t.Errorf("yaml tool = %q", got)
	}
	script := tools.Definition{ID: "lint", Script: true, Path: "/repo/.discobox/tools/lint.sh", Args: []string{"-v"}, Layer: tools.LayerSource}
	if got := strings.Join(sandboxToolCommand(script, nil), " "); got != "/repo/.discobox/tools/lint.sh -v" {
		t.Errorf("source script = %q", got)
	}
	defs, err := tools.DiscoverFS(fstestScript("lint.sh", "#!/bin/sh\n#---\n#---\necho \"$@\"\n"), "", tools.LayerUser)
	if err != nil || len(defs) != 1 || defs[0].Problem != "" {
		t.Fatalf("defs = %+v, %v", defs, err)
	}
	got := sandboxToolCommand(defs[0], []string{"a"})
	if got[0] != "sh" || got[1] != "-c" || got[2] != deliverToolScript || got[4] != "lint.sh" || !strings.Contains(got[5], "echo") || got[6] != "a" {
		t.Errorf("user script = %q", got)
	}
}

// The delivery script is run for real: a script from this machine is written
// somewhere of its own, runs with the caller's arguments and exit status, and
// is gone afterwards — nothing left in a directory another run could reach.
func TestDeliverToolScriptWritesRunsAndRemovesTheScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the delivery script runs in the discobox, under sh")
	}
	tmp := t.TempDir()
	for _, tc := range []struct {
		body, want string
		code       int
	}{
		{"#!/bin/sh\necho \"hello $1\"\n", "hello world\n", 0},
		{"#!/bin/sh\necho \"bye $1\"\nexit 3\n", "bye world\n", 3},
	} {
		//nolint:gosec // G204: the script under test, with the test's own fixed arguments.
		cmd := exec.CommandContext(t.Context(), "sh", "-c", deliverToolScript, "sh", "hello.sh", tc.body, "world")
		cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
		out, err := cmd.Output()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if string(out) != tc.want || code != tc.code {
			t.Fatalf("output = %q exit %d, want %q exit %d", out, code, tc.want, tc.code)
		}
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("the delivered scripts were left behind: %v", entries)
	}
}

func fstestScript(name, body string) fstest.MapFS {
	return fstest.MapFS{name: {Data: []byte(body), Mode: 0o755}}
}

// A person replaces the image's diff by declaring its stable id under a name of
// their own: the git summary opens theirs, and `discobox tools <their name>`
// runs it too.
func TestAUserToolTakesOverTheDiffByItsID(t *testing.T) {
	home := t.TempDir()
	declareUserTool(t, home, "difft.yaml", "id: ai.discobox.diff\nname: difft\nprogram: difft\n")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	user, err := userTools()
	if err != nil {
		t.Fatal(err)
	}
	image := []tools.Definition{{ID: tools.DiffID, Name: "diff", Runs: tools.RunsSandbox, Layer: tools.LayerImage, FileName: "10-diff.yaml", Program: []string{"discobox-review"}}}
	catalog := mergeTools(builtinTools(), image, nil, user)

	for _, name := range []string{tools.DiffID, "difft"} {
		def, err := findTool(catalog, name)
		if err != nil || def.Program[0] != "difft" {
			t.Fatalf("findTool(%q) = %+v, %v; want the user's difft", name, def, err)
		}
	}
	if _, err := findTool(catalog, "diff"); err == nil {
		t.Fatal("the image's diff is still reachable by its old name")
	}
}

// A pool that refuses the sandbox in plain text is answering the route, and the
// refusal is what the command reports — not an empty catalog that lists this
// CLI's tools for a discobox that cannot run them.
func TestAPlainTextRefusalIsNotAMissingRoute(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
	}{
		{http.StatusNotFound, "sandbox not found"},
		{http.StatusConflict, "sandbox has no container on this pool: it is being rebuilt, or it needs repair"},
	} {
		fake := vscodeFakeServer()
		fake.toolsError, fake.toolsCode = tc.body, tc.code
		server := fake.start(t)

		home := t.TempDir()
		setHome(t, home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		cmd := NewRootCommand()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "tools", "--discobox-id", "sbx_devbox00000001", "ls"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), tc.body) {
			t.Fatalf("%d %q: err = %v, output:\n%s", tc.code, tc.body, err, out.String())
		}
	}
}

// A diff session a launcher opened before the diff had a stable id is still
// labeled "diff" in its sandbox, and is still the diff.
func TestALegacyDiffSessionIsStillTheDiff(t *testing.T) {
	if got := execToolID("diff"); got != tools.DiffID {
		t.Fatalf("legacy label = %q, want %q", got, tools.DiffID)
	}
	for _, label := range []string{"fresh", tools.DiffID, ""} {
		if got := execToolID(label); got != label {
			t.Fatalf("label %q read as %q", label, got)
		}
	}
}

// A discobox's tool declarations are checked again on this side, whatever its
// agent said: anything with root in the box can answer the route.
func TestADiscoboxsToolsAreCheckedAgainHere(t *testing.T) {
	for name, in := range map[string]apimodel.SandboxTool{
		"path id":        {ID: "../x", Name: "x", Runs: "sandbox", Layer: "source", Program: []string{"x"}},
		"no program":     {ID: "x", Name: "x", Runs: "sandbox", Layer: "source"},
		"empty program":  {ID: "x", Name: "x", Runs: "sandbox", Layer: "source", Program: []string{""}},
		"backslash file": {ID: "x", Name: "x", Runs: "sandbox", Layer: "source", Program: []string{"x"}, Files: []apimodel.SandboxToolFile{{Name: `..\..\Startup\x.cmd`, Home: ".x"}}},
		"slash file":     {ID: "x", Name: "x", Runs: "sandbox", Layer: "source", Program: []string{"x"}, Files: []apimodel.SandboxToolFile{{Name: "../x", Home: ".x"}}},
		"host tool":      {ID: "x", Name: "x", Runs: "host", Layer: "source", Program: []string{"x"}},
		"user layer":     {ID: "x", Name: "x", Runs: "sandbox", Layer: "user", Program: []string{"x"}},
		"two-rune key":   {ID: "x", Name: "x", Runs: "sandbox", Layer: "image", Program: []string{"x"}, Key: apiclientgen.NewOptString("ab")},
	} {
		if def := definitionFromAPI(in); def.Problem == "" {
			t.Errorf("%s: accepted %+v", name, def)
		}
	}
	ok := apimodel.SandboxTool{ID: "ai.discobox.diff", Name: "diff", Runs: "sandbox", Layer: "image", Program: []string{"discobox-review"}, Key: apiclientgen.NewOptString("d")}
	if def := definitionFromAPI(ok); def.Problem != "" {
		t.Fatalf("a good declaration was refused: %s", def.Problem)
	}
	if _, err := toolFilePath("x", `..\..\evil`); err == nil {
		t.Fatal("a backslashed file name was joined to a path")
	}
}
