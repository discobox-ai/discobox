package cli

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/adrg/xdg"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/tools"
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
	setConfigHome(t, filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := fake.start(t)
	cmd := NewRootCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "tools", "ls", "sbx_devbox00000001"})
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
	if _, _, _, err := runToolsVSCodeCmd(t, fake, "sbx_devbox00000001"); err != nil {
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
	_, _, _, err := runToolsCmd(t, fake, t.TempDir(), "open", "sbx_devbox00000001")
	if err == nil || !strings.Contains(err.Error(), "runs on your machine") {
		t.Fatalf("err = %v, want the refusal", err)
	}
	_, _, _, err = runToolsCmd(t, fake, t.TempDir(), "nope", "sbx_devbox00000001")
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
	setConfigHome(t, filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cmd := NewRootCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "tools", "vscode", "sbx_devbox00000001"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("tools vscode: %v\n%s", err, out.String())
	}
	if args := editorArgs(t, record); !contains(args, "--folder-uri") {
		t.Fatalf("editor args = %v", args)
	}
}

// A host script has no args of its own to put placeholders in, so it is handed
// the discobox in its environment: where it is, and what a script needs to
// reach it again with the CLI (ADR 26-11-03-982 §1).
func TestAHostScriptIsHandedTheDiscoboxInItsEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script is a shell script")
	}
	home := t.TempDir()
	fake := vscodeFakeServer()
	record := declareEnvRecordingTool(t, home)
	if _, _, stderr, err := runToolsCmd(t, fake, home, "gitk", "sbx_devbox00000001", "--", "--all"); err != nil {
		t.Fatalf("tools gitk: %v\n%s", err, stderr)
	}
	got := strings.Split(readFile(t, record), "\n")
	host := sshConfigHost(sshConfigNamespace(fake.url, resolvedTestProjectID), "sbx_devbox00000001")
	want := []string{host, "ssh://" + host + "/home/agent/repo", "/home/agent/repo", "--all"}
	if strings.Join(got[:4], "\n") != strings.Join(want, "\n") {
		t.Fatalf("the script saw %q, want %q", got[:4], want)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// --project said project-1; the script is handed what the server
	// resolved it to, which a CLI on any machine reads the same way.
	if got[4] != self || got[6] != resolvedTestProjectID {
		t.Errorf("DISCOBOX_CLI = %q, DISCOBOX_PROJECT = %q; want %q, %q", got[4], got[6], self, resolvedTestProjectID)
	}
	if !strings.HasPrefix(got[5], "http://127.0.0.1:") {
		t.Errorf("DISCOBOX_SERVER = %q, want the test server's address", got[5])
	}
}

// A server named by its registered name is handed on as its address: a CLI
// run by the script, on a machine whose registry may differ, dials the same
// server.
func TestAHostScriptIsHandedTheServersAddressNotItsName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script is a shell script")
	}
	home := t.TempDir()
	record := declareEnvRecordingTool(t, home)
	setHome(t, home)
	setConfigHome(t, filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// The registry is found through xdg's cached directories: re-read them
	// for this test's, and again after, so the server registered here is not
	// every later test's.
	xdg.Reload()
	t.Cleanup(xdg.Reload)
	server := vscodeFakeServer().start(t)
	registerForTest(t, registeredServer{Name: "lab", Address: server.URL})

	cmd := NewRootCommand()
	var errOut strings.Builder
	cmd.SetOut(io.Discard)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", "lab", "--project", "project-1", "tools", "gitk", "sbx_devbox00000001"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("tools gitk: %v\n%s", err, errOut.String())
	}
	if got := strings.Split(readFile(t, record), "\n")[5]; got != server.URL {
		t.Errorf("DISCOBOX_SERVER = %q, want the address %q that lab names", got, server.URL)
	}
}

// A host tool is handed no token: the CLI it runs is authorized by its
// transport, and one this process inherited was given for the primary and
// would otherwise reach whatever server DISCOBOX_SERVER names
// (App.childCLIEnv). Neither the inherited token nor a --token given to this
// run reaches the script.
func TestAHostScriptIsHandedNoToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script is a shell script")
	}
	t.Setenv("DISCOBOX_TOKEN", "inherited-token")
	home := t.TempDir()
	record := filepath.Join(t.TempDir(), "token")
	declareUserTool(t, home, "tok.sh", "#!/bin/sh\n#---\n# runs: host\n#---\nprintf %s \"$DISCOBOX_TOKEN\" > "+record+"\n")
	if err := os.Chmod(filepath.Join(home, ".config", "discobox", "tools", "tok.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	runToolsWithToken(t, home, "flag-token", "tok")
	if got := readFile(t, record); got != "" {
		t.Errorf("DISCOBOX_TOKEN = %q, want none", got)
	}
}

// A .yaml host tool is usually an editor, and every terminal in it inherits
// what it was started with: it is not handed the discobox's server and
// project, which would aim every `discobox` run in it at this one discobox's
// (ADR 0125 §4 hands the environment to a script).
func TestAYAMLHostToolKeepsTheCLIsOwnEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the program is a shell script")
	}
	home := t.TempDir()
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "env")
	program := filepath.Join(bin, "fake-editor")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s|%s|%s' \"${DISCOBOX_SERVER-unset}\" \"${DISCOBOX_PROJECT-unset}\" \"$1\" > "+record+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	declareUserTool(t, home, "ed.yaml", "runs: host\nprogram: "+program+"\nargs: [\"{discobox.project}\"]\n")
	// Whatever this process inherited is the editor's to keep; this test's
	// starts with neither.
	for _, name := range []string{"DISCOBOX_SERVER", "DISCOBOX_PROJECT"} {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
	if _, _, stderr, err := runToolsCmd(t, vscodeFakeServer(), home, "ed", "sbx_devbox00000001"); err != nil {
		t.Fatalf("tools ed: %v\n%s", err, stderr)
	}
	if got, want := readFile(t, record), "unset|unset|"+resolvedTestProjectID; got != want {
		t.Errorf("the editor saw %q, want %q: no DISCOBOX_SERVER or DISCOBOX_PROJECT, and the resolved project as a placeholder", got, want)
	}
}

// A .yaml tool that runs the CLI against the discobox's server, by naming
// {discobox.server} or {discobox.cli}, is started as a script is, with no
// token: never one this process inherited for the primary
// (Definition.RunsTheCLI).
func TestAYAMLHostToolThatRunsTheCLIIsHandedNoToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the program is a shell script")
	}
	t.Setenv("DISCOBOX_TOKEN", "inherited-token")
	home := t.TempDir()
	record := filepath.Join(t.TempDir(), "token")
	program := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s|%s' \"$DISCOBOX_TOKEN\" \"$1\" > "+record+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	declareUserTool(t, home, "remote-ls.yaml", "runs: host\nprogram: "+program+"\nargs: [\"--server={discobox.server}\"]\n")
	runToolsWithToken(t, home, "flag-token", "remote-ls")
	token, server, _ := strings.Cut(readFile(t, record), "|")
	if token != "" {
		t.Errorf("DISCOBOX_TOKEN = %q, want none", token)
	}
	if !strings.HasPrefix(server, "--server=http://127.0.0.1:") {
		t.Errorf("args = %q, want the server's address", server)
	}
}

// `discobox tools --help` is what a person or an agent writes a tool from, so
// it names every variable a host script is handed, and every placeholder it
// names is one a .yaml tool's args are filled in with (ADR 26-11-03-982 §3).
func TestToolsHelpNamesWhatAHostToolIsHanded(t *testing.T) {
	cmd, _, err := NewRootCommand().Find([]string{"tools"})
	if err != nil {
		t.Fatal(err)
	}
	help := cmd.Long
	for _, entry := range (tools.Remote{}).Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.Contains(help, name) {
			t.Errorf("tools --help does not say a host script is handed %s", name)
		}
	}
	remote := tools.Remote{SandboxID: "sbx_1", Host: "h", Workdir: "/w", ProjectID: "p", Server: "s", CLI: "c"}
	placeholders := regexp.MustCompile(`\{[a-z]+(\.[a-z]+)*\}`).FindAllString(help, -1)
	if len(placeholders) == 0 {
		t.Fatal("tools --help names no placeholders")
	}
	for _, placeholder := range placeholders {
		if got, err := remote.Expand([]string{placeholder}); err != nil || got[0] == placeholder {
			t.Errorf("tools --help names %s, which a host tool's args are not filled in with", placeholder)
		}
	}
}

// runToolsWithToken runs `discobox --token <token> tools <tool>` against the
// devbox, which runToolsCmd cannot: its arguments follow `tools`.
func runToolsWithToken(t *testing.T, home, token, tool string) {
	t.Helper()
	setHome(t, home)
	setConfigHome(t, filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := vscodeFakeServer().start(t)
	cmd := NewRootCommand()
	var errOut strings.Builder
	cmd.SetOut(io.Discard)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "--token", token, "tools", tool, "sbx_devbox00000001"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("tools %s: %v\n%s", tool, err, errOut.String())
	}
}

// declareEnvRecordingTool declares a user host script, gitk, that writes what
// it was handed to the returned file, one value per line.
func declareEnvRecordingTool(t *testing.T, home string) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "env")
	declareUserTool(t, home, "gitk.sh", "#!/bin/sh\n#---\n# runs: host\n#---\n"+
		"printf '%s\\n' \"$DISCOBOX_SSH_HOST\" \"$DISCOBOX_GIT_URL\" \"$DISCOBOX_WORKDIR\" \"$1\" "+
		"\"$DISCOBOX_CLI\" \"$DISCOBOX_SERVER\" \"$DISCOBOX_PROJECT\" > "+record+"\n")
	if err := os.Chmod(filepath.Join(home, ".config", "discobox", "tools", "gitk.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestSandboxToolCommand(t *testing.T) {
	fake, app := newFakeFilesAPI(t)
	yaml := tools.Definition{ID: "fresh", Program: []string{"fresh"}, Args: []string{"."}, Layer: tools.LayerImage}
	if got, err := app.sandboxToolCommand(t.Context(), "project-1", "sbx_1", yaml, []string{"x"}); err != nil || strings.Join(got, " ") != "fresh . x" {
		t.Errorf("yaml tool = %q, %v", got, err)
	}
	script := tools.Definition{ID: "lint", Script: true, Path: "/repo/.discobox/tools/lint.sh", Args: []string{"-v"}, Layer: tools.LayerSource}
	if got, err := app.sandboxToolCommand(t.Context(), "project-1", "sbx_1", script, nil); err != nil || strings.Join(got, " ") != "/repo/.discobox/tools/lint.sh -v" {
		t.Errorf("source script = %q, %v", got, err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a tool already in the discobox wrote %v", fake.calls)
	}
	defs, err := tools.DiscoverFS(fstestScript("lint.sh", "#!/bin/sh\n#---\n#---\necho \"$@\"\n"), "", tools.LayerUser)
	if err != nil || len(defs) != 1 || defs[0].Problem != "" {
		t.Fatalf("defs = %+v, %v", defs, err)
	}
	got, err := app.sandboxToolCommand(t.Context(), "project-1", "sbx_1", defs[0], []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 || got[0] != "sh" || got[1] != "-c" || got[2] != runToolScript || got[5] != "lint.sh" || got[6] != "a" {
		t.Fatalf("user script = %q", got)
	}
	dir := got[4]
	if !strings.HasPrefix(dir, "/tmp/discobox-tool.") || fake.dirs[dir] != 0o700 {
		t.Fatalf("dir %q, made %v; want a directory of its own only the user enters", dir, fake.dirs)
	}
	written := fake.files[dir+"/lint.sh"]
	if !strings.Contains(written.content, "echo") || written.mode != "448" {
		t.Fatalf("script written as %+v, want it executable by the user alone", written)
	}
}

// writeToolScript lays out what deliverToolScript writes in the discobox: the
// script in a directory of its own.
func writeToolScript(t *testing.T, root, name, body string) string {
	t.Helper()
	dir, err := os.MkdirTemp(root, "discobox-tool.")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The run script is run for real: a script from this machine runs with the
// caller's arguments and exit status, and its directory is gone afterwards —
// nothing left where another run could reach it.
func TestRunToolScriptRunsAndRemovesTheScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the run script runs in the discobox, under sh")
	}
	tmp := t.TempDir()
	for _, tc := range []struct {
		body, want string
		code       int
	}{
		{"#!/bin/sh\necho \"hello $1\"\n", "hello world\n", 0},
		{"#!/bin/sh\necho \"bye $1\"\nexit 3\n", "bye world\n", 3},
	} {
		dir := writeToolScript(t, tmp, "hello.sh", tc.body)
		//nolint:gosec // G204: the script under test, with the test's own fixed arguments.
		cmd := exec.CommandContext(t.Context(), "sh", "-c", runToolScript, "sh", dir, "hello.sh", "world")
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
	setConfigHome(t, filepath.Join(home, ".config"))
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
		setConfigHome(t, filepath.Join(home, ".config"))
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		cmd := NewRootCommand()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "tools", "ls", "sbx_devbox00000001"})
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

// git's arguments go after the --: a git word where DISCOBOX goes is refused,
// saying so, rather than taken for git's.
func TestToolsGitTakesItsArgumentsAfterTheSeparator(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandboxes":[]}`))
	}))
	t.Cleanup(server.Close)
	for args, want := range map[string]string{
		"status":               "a command goes after --: discobox tools git -- status",
		"sbx_h1ssjzhp60emtc2n": "git's arguments go after --",
	} {
		cmd := NewRootCommand()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "tools", "git", args})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("tools git %s error = %v, want %q", args, err, want)
		}
	}
}
