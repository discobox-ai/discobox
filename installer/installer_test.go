package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestStampWritesTheReleaseAndEveryDigest(t *testing.T) {
	scripts, err := Stamp("v1.2.3", []Asset{
		{Name: "discobox-windows-amd64.exe", SHA256: digest},
		{Name: "discobox-linux-amd64", SHA256: strings.Repeat("f", 64)},
	})
	if err != nil {
		t.Fatal(err)
	}

	shell := string(scripts[ShellName])
	for _, want := range []string{
		"\nrelease='v1.2.3'\n",
		"\nchecksums='\n" + strings.Repeat("f", 64) + "  discobox-linux-amd64\n" + digest + "  discobox-windows-amd64.exe\n'\n",
	} {
		if !strings.Contains(shell, want) {
			t.Errorf("%s lacks %q", ShellName, want)
		}
	}
	powerShell := string(scripts[PowerShellName])
	for _, want := range []string{
		"\n    $release = 'v1.2.3'\n",
		"\n    $checksums = @{\n        'discobox-linux-amd64' = '" + strings.Repeat("f", 64) + "'\n        'discobox-windows-amd64.exe' = '" + digest + "'\n    }\n",
	} {
		if !strings.Contains(powerShell, want) {
			t.Errorf("%s lacks %q", PowerShellName, want)
		}
	}

	// Nothing else moves: the stamp is the only difference from the source.
	if got, want := len(strings.Split(shell, "\n")), len(strings.Split(string(shellScript), "\n"))+3; got != want {
		t.Errorf("%s has %d lines, want %d", ShellName, got, want)
	}
}

func TestStampRefusesAnythingThatCouldEscapeItsQuotes(t *testing.T) {
	good := Asset{Name: "discobox-linux-amd64", SHA256: digest}
	for name, tc := range map[string]struct {
		release string
		assets  []Asset
	}{
		"release without v":    {"1.2.3", []Asset{good}},
		"release with a quote": {"v1.2.3';rm -rf ~;'", []Asset{good}},
		"no assets":            {"v1.2.3", nil},
		"asset with a quote":   {"v1.2.3", []Asset{{Name: "a'b", SHA256: digest}}},
		"asset with a slash":   {"v1.2.3", []Asset{{Name: "../discobox", SHA256: digest}}},
		"uppercase digest":     {"v1.2.3", []Asset{{Name: "discobox", SHA256: strings.ToUpper(digest)}}},
		"short digest":         {"v1.2.3", []Asset{{Name: "discobox", SHA256: digest[:63]}}},
		"asset listed twice":   {"v1.2.3", []Asset{good, good}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Stamp(tc.release, tc.assets); err == nil {
				t.Fatal("Stamp accepted it")
			}
		})
	}
}

func TestStampNeedsEachMarkerExactlyOnce(t *testing.T) {
	if _, err := stampLines("x", []byte("a\nrelease=\nrelease=\n"), map[string]string{"release=": "release='v1'"}); err == nil {
		t.Error("a doubled marker was stamped")
	}
	if _, err := stampLines("x", []byte("a\n"), map[string]string{"release=": "release='v1'"}); err == nil {
		t.Error("a missing marker was not noticed")
	}
}

// release is one release the fake GitHub serves.
type release struct {
	tag        string
	prerelease bool
	// noInstaller is a release cut before install.sh existed.
	noInstaller bool
}

// server fakes both places a release's assets are published and the Releases
// API, laid out as the installers expect them.
type server struct {
	*httptest.Server
	files map[string]map[string][]byte

	// mirror is how the mirror answers: "" serves the file, "missing" 404s,
	// "corrupt" serves other bytes. github is the same for the release URL.
	mirror, github string
	// rateLimited makes the API answer as GitHub does when it is.
	rateLimited bool
	// sortedAPI serves the release list compact and with its keys sorted,
	// rather than in GitHub's own order, so prerelease comes before tag_name.
	sortedAPI bool
	// apiCalls counts the requests the API has answered.
	apiCalls atomic.Int32
}

func newServer(t *testing.T, releases []release) *server {
	t.Helper()
	s := &server{files: map[string]map[string][]byte{}}
	var list []githubRelease
	for _, r := range releases {
		binary := fakeBinary(t, r.tag)
		sum := sha256.Sum256(binary)
		asset := hostAsset()
		s.files[r.tag] = map[string][]byte{asset: binary}
		if !r.noInstaller {
			scripts, err := Stamp(r.tag, []Asset{{Name: asset, SHA256: hex.EncodeToString(sum[:])}})
			if err != nil {
				t.Fatal(err)
			}
			for name, script := range scripts {
				s.files[r.tag][name] = script
			}
		}
		list = append(list, githubRelease{
			URL:        "https://api.github.com/repos/discobox-ai/discobox/releases/1",
			Author:     map[string]any{"login": "someone", "type": "User"},
			TagName:    r.tag,
			Prerelease: r.prerelease,
			// The body mentions both keys, as release notes can; the parser
			// must not take them for this release's own.
			Body:   `Fixed "tag_name": "v9.9.9", "prerelease": false, in the notes.`,
			Assets: []map[string]any{{"name": asset, "uploader": map[string]any{"login": "someone"}}},
		})
	}
	// GitHub's own shape: its field order, indented.
	githubJSON, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// The same list with every key sorted, which a map marshals to.
	var generic []map[string]any
	if err := json.Unmarshal(githubJSON, &generic); err != nil {
		t.Fatal(err)
	}
	sortedJSON, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	serve := func(mode *string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, ok := s.files[r.PathValue("tag")][r.PathValue("asset")]
			switch {
			case !ok || *mode == "missing":
				http.NotFound(w, r)
			case *mode == "corrupt":
				_, _ = w.Write(append([]byte("not "), body...))
			default:
				_, _ = w.Write(body)
			}
		}
	}
	mux.HandleFunc("GET /mirror/{tag}/{asset}", serve(&s.mirror))
	mux.HandleFunc("GET /github/{tag}/{asset}", serve(&s.github))
	mux.HandleFunc("GET /api/releases", func(w http.ResponseWriter, _ *http.Request) {
		s.apiCalls.Add(1)
		if s.rateLimited {
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if s.sortedAPI {
			_, _ = w.Write(sortedJSON)
			return
		}
		_, _ = w.Write(githubJSON)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// githubRelease is a release as the Releases API lists it, in GitHub's field
// order: tag_name before draft and prerelease.
type githubRelease struct {
	URL        string           `json:"url"`
	Author     map[string]any   `json:"author"`
	TagName    string           `json:"tag_name"`
	Draft      bool             `json:"draft"`
	Prerelease bool             `json:"prerelease"`
	Body       string           `json:"body"`
	Assets     []map[string]any `json:"assets"`
}

// env points an installer at this server rather than at the real release.
func (s *server) env() []string {
	return []string{
		"DISCOBOX_INSTALL_SOURCES=" + s.URL + "/mirror " + s.URL + "/github",
		"DISCOBOX_INSTALL_API=" + s.URL + "/api",
	}
}

// script writes one of a release's installers, or the unstamped source when
// tag is empty, to a file.
func (s *server) script(t *testing.T, tag, name string) string {
	t.Helper()
	body := map[string][]byte{ShellName: shellScript, PowerShellName: powerShellScript}[name]
	if tag != "" {
		body = s.files[tag][name]
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// hostAsset is the release asset for the machine running the test.
func hostAsset() string {
	name := "discobox-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

var (
	fakeBinariesMu sync.Mutex
	fakeBinaries   = map[string][]byte{}
	fakeBinaryDir  string
)

// fakeBinary is a discobox that reports tag as its version, built once per tag.
func fakeBinary(t *testing.T, tag string) []byte {
	t.Helper()
	fakeBinariesMu.Lock()
	defer fakeBinariesMu.Unlock()
	if binary, ok := fakeBinaries[tag]; ok {
		return binary
	}
	if fakeBinaryDir == "" {
		dir, err := os.MkdirTemp("", "fakediscobox")
		if err != nil {
			t.Fatal(err)
		}
		fakeBinaryDir = dir
	}
	out := filepath.Join(fakeBinaryDir, tag+".exe")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", out, "-ldflags", "-X main.version="+tag, "./testdata/fakediscobox")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the fake discobox: %v\n%s", err, output)
	}
	binary, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	fakeBinaries[tag] = binary
	return binary
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeBinaryDir != "" {
		_ = os.RemoveAll(fakeBinaryDir)
	}
	os.Exit(code)
}

// The releases most tests install from, newest first, in the shape ADR 0105
// leaves them: an explicit alpha on top, then a dot release nobody has blessed
// yet, then the newest blessed one, then one from before install.sh existed.
var ladder = []release{
	{tag: "v1.3.0-alpha.1", prerelease: true},
	{tag: "v1.2.0", prerelease: true},
	{tag: "v1.1.0"},
	{tag: "v1.0.0", noInstaller: true},
}

// installResult is what one run of an installer did.
type installResult struct {
	output  string
	err     error
	dir     string
	version string
}

// clean is the environment with nothing an installer reads, so a developer's
// own DISCOBOX_CHANNEL cannot decide a test.
func clean() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DISCOBOX_") {
			env = append(env, kv)
		}
	}
	return env
}

// installedVersion is what the discobox an installer put in dir says it is.
func installedVersion(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "discobox")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	out, err := exec.CommandContext(t.Context(), path, "--version").Output() //nolint:gosec // The binary this test just installed, in its own temporary directory.
	if err != nil {
		t.Fatalf("the installed discobox does not run: %v", err)
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "discobox version ")
}

func requireShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is not the Windows installer")
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		t.Skip("install.sh refuses an Intel Mac")
	}
	for _, tool := range []string{"sh", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s to run install.sh with", tool)
		}
	}
}

func runShell(t *testing.T, s *server, script string, env []string, args ...string) installResult {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	//nolint:gosec // The installer under test, with arguments this test wrote.
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{script, "--dir", dir}, args...)...)
	cmd.Env = append(append(clean(), s.env()...), env...)
	out, err := cmd.CombinedOutput()
	return installResult{output: string(out), err: err, dir: dir, version: installedVersion(t, dir)}
}

func TestShellInstallsItsOwnReleaseWithoutAskingTheAPI(t *testing.T) {
	requireShell(t)
	s := newServer(t, ladder)
	got := runShell(t, s, s.script(t, "v1.1.0", ShellName), nil)
	if got.err != nil || got.version != "v1.1.0" {
		t.Fatalf("installed %q, err %v:\n%s", got.version, got.err, got.output)
	}
	if calls := s.apiCalls.Load(); calls != 0 {
		t.Errorf("the API was asked %d times; a stamped installer with no options needs no lookup", calls)
	}
}

func TestShellChannelsAndVersions(t *testing.T) {
	requireShell(t)
	s := newServer(t, ladder)
	stamped := s.script(t, "v1.1.0", ShellName)
	source := s.script(t, "", ShellName)
	for name, tc := range map[string]struct {
		script string
		env    []string
		args   []string
		want   string
	}{
		"source installs stable":              {source, nil, nil, "v1.1.0"},
		"stable":                              {stamped, nil, []string{"--channel", "stable"}, "v1.1.0"},
		"latest is the newest dot release":    {stamped, nil, []string{"--channel", "latest"}, "v1.2.0"},
		"edge includes an alpha":              {stamped, nil, []string{"--channel=edge"}, "v1.3.0-alpha.1"},
		"a version":                           {stamped, nil, []string{"--version", "v1.2.0"}, "v1.2.0"},
		"a version without its v":             {source, nil, []string{"--version", "1.3.0-alpha.1"}, "v1.3.0-alpha.1"},
		"channel from the environment":        {stamped, []string{"DISCOBOX_CHANNEL=latest"}, nil, "v1.2.0"},
		"version beats channel in env":        {stamped, []string{"DISCOBOX_CHANNEL=edge", "DISCOBOX_VERSION=v1.2.0"}, nil, "v1.2.0"},
		"a flag beats the environment":        {stamped, []string{"DISCOBOX_VERSION=v1.2.0"}, []string{"--channel", "stable"}, "v1.1.0"},
		"a version flag beats a channel flag": {stamped, nil, []string{"--channel", "edge", "--version", "v1.2.0"}, "v1.2.0"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := runShell(t, s, tc.script, tc.env, tc.args...)
			if got.err != nil || got.version != tc.want {
				t.Fatalf("installed %q, want %s; err %v:\n%s", got.version, tc.want, got.err, got.output)
			}
		})
	}
}

func TestShellReadsTheReleaseListWhateverItsKeyOrder(t *testing.T) {
	requireShell(t)
	s := newServer(t, ladder)
	s.sortedAPI = true
	for channel, want := range map[string]string{"stable": "v1.1.0", "latest": "v1.2.0", "edge": "v1.3.0-alpha.1"} {
		got := runShell(t, s, s.script(t, "", ShellName), nil, "--channel", channel)
		if got.err != nil || got.version != want {
			t.Errorf("%s: installed %q, want %s; err %v:\n%s", channel, got.version, want, got.err, got.output)
		}
	}
}

func TestShellEdgeIsStableWhenStableIsNewest(t *testing.T) {
	requireShell(t)
	// Every release blessed but an old rc: the mirror's prerelease alias now
	// points backwards, and edge must not follow it.
	s := newServer(t, []release{{tag: "v2.0.0"}, {tag: "v1.9.0-rc.1", prerelease: true}})
	got := runShell(t, s, s.script(t, "v2.0.0", ShellName), nil, "--channel", "edge")
	if got.err != nil || got.version != "v2.0.0" {
		t.Fatalf("installed %q, want v2.0.0; err %v:\n%s", got.version, got.err, got.output)
	}
}

func TestShellFallsBackFromTheMirror(t *testing.T) {
	requireShell(t)
	for _, mode := range []string{"missing", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			s := newServer(t, ladder)
			s.mirror = mode
			got := runShell(t, s, s.script(t, "v1.1.0", ShellName), nil, "--channel", "latest")
			if got.err != nil || got.version != "v1.2.0" {
				t.Fatalf("installed %q, err %v:\n%s", got.version, got.err, got.output)
			}
		})
	}
}

func TestShellRefuses(t *testing.T) {
	requireShell(t)
	for name, tc := range map[string]struct {
		mirror, github string
		rateLimited    bool
		args           []string
		want           string
	}{
		"bytes that match no digest":  {mirror: "corrupt", github: "corrupt", want: "SHA-256"},
		"a release with no installer": {args: []string{"--version", "v1.0.0"}, want: "v1.0.0 has no installer"},
		"a rate-limited lookup":       {rateLimited: true, args: []string{"--channel", "edge"}, want: "rate limiting"},
		"an unknown channel":          {args: []string{"--channel", "beta"}, want: "no channel called beta"},
		"an unknown option":           {args: []string{"--force"}, want: "unknown option --force"},
		"a version that is not one":   {args: []string{"--version", "main"}, want: "not a release version"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newServer(t, ladder)
			s.mirror, s.github, s.rateLimited = tc.mirror, tc.github, tc.rateLimited
			got := runShell(t, s, s.script(t, "v1.1.0", ShellName), nil, tc.args...)
			if got.err == nil {
				t.Fatalf("installed %q:\n%s", got.version, got.output)
			}
			if !strings.Contains(got.output, tc.want) {
				t.Errorf("output does not say %q:\n%s", tc.want, got.output)
			}
			if got.version != "" {
				t.Errorf("installed %s anyway", got.version)
			}
		})
	}
}

// powerShells are the PowerShells on this machine: pwsh wherever it is found,
// and Windows PowerShell 5.1 too on Windows, which is what `irm | iex` most
// often runs under there.
func powerShells(t *testing.T) []string {
	t.Helper()
	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		t.Skip("install.ps1 refuses an Intel Mac")
	}
	var found []string
	names := []string{"pwsh"}
	if runtime.GOOS == "windows" {
		names = append(names, "powershell")
	}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			found = append(found, path)
		}
	}
	if len(found) == 0 {
		t.Skip("no PowerShell to run install.ps1 with")
	}
	return found
}

// psQuote is a PowerShell single-quoted string.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// runPowerShell runs install.ps1 the two ways people do: as a script block
// given parameters, or piped to Invoke-Expression and configured through the
// environment, which is the only way `irm | iex` takes options.
func runPowerShell(t *testing.T, shell string, s *server, script string, iex bool, env []string, params string) installResult {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	var command string
	if iex {
		env = append(env, "DISCOBOX_INSTALL_DIR="+dir, "DISCOBOX_NO_MODIFY_PATH=1")
		command = fmt.Sprintf("Get-Content -Raw -LiteralPath %s | Invoke-Expression", psQuote(script))
	} else {
		command = fmt.Sprintf("& ([scriptblock]::Create((Get-Content -Raw -LiteralPath %s))) -InstallDir %s -NoModifyPath %s",
			psQuote(script), psQuote(dir), params)
	}
	//nolint:gosec // A PowerShell this test found on PATH, running the installer under test.
	cmd := exec.CommandContext(t.Context(), shell, "-NoProfile", "-NonInteractive", "-Command", command)
	cmd.Env = append(append(clean(), s.env()...), env...)
	out, err := cmd.CombinedOutput()
	return installResult{output: string(out), err: err, dir: dir, version: installedVersion(t, dir)}
}

func TestPowerShell(t *testing.T) {
	for _, shell := range powerShells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			s := newServer(t, ladder)
			stamped := s.script(t, "v1.1.0", PowerShellName)
			for name, tc := range map[string]struct {
				iex    bool
				env    []string
				params string
				want   string
			}{
				"its own release":                   {false, nil, "", "v1.1.0"},
				"its own release through iex":       {true, nil, "", "v1.1.0"},
				"edge":                              {false, nil, "-Channel edge", "v1.3.0-alpha.1"},
				"latest through iex":                {true, []string{"DISCOBOX_CHANNEL=latest"}, "", "v1.2.0"},
				"a version":                         {false, nil, "-Version 1.2.0", "v1.2.0"},
				"a parameter beats the environment": {false, []string{"DISCOBOX_VERSION=v1.2.0"}, "-Channel stable", "v1.1.0"},
			} {
				t.Run(name, func(t *testing.T) {
					got := runPowerShell(t, shell, s, stamped, tc.iex, tc.env, tc.params)
					if got.err != nil || got.version != tc.want {
						t.Fatalf("installed %q, want %s; err %v:\n%s", got.version, tc.want, got.err, got.output)
					}
				})
			}
		})
	}
}

func TestPowerShellFallsBackAndRefuses(t *testing.T) {
	for _, shell := range powerShells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			s := newServer(t, ladder)
			s.mirror = "corrupt"
			got := runPowerShell(t, shell, s, s.script(t, "v1.1.0", PowerShellName), false, nil, "")
			if got.err != nil || got.version != "v1.1.0" {
				t.Fatalf("a corrupt mirror did not fall back: installed %q, err %v:\n%s", got.version, got.err, got.output)
			}

			s.github = "corrupt"
			got = runPowerShell(t, shell, s, s.script(t, "v1.1.0", PowerShellName), false, nil, "")
			if got.err == nil || !strings.Contains(got.output, "SHA-256") {
				t.Fatalf("installed %q from bytes that match no digest:\n%s", got.version, got.output)
			}

			s.mirror, s.github, s.rateLimited = "", "", true
			got = runPowerShell(t, shell, s, s.script(t, "v1.1.0", PowerShellName), false, nil, "-Channel latest")
			if got.err == nil || !strings.Contains(got.output, "rate limiting") {
				t.Fatalf("a rate-limited lookup did not say so: err %v:\n%s", got.err, got.output)
			}

			s.rateLimited = false
			got = runPowerShell(t, shell, s, s.script(t, "v1.1.0", PowerShellName), false, nil, "-Version v1.0.0")
			if got.err == nil || !strings.Contains(got.output, "v1.0.0 has no installer") {
				t.Fatalf("a release with no installer did not say so: err %v:\n%s", got.err, got.output)
			}
		})
	}
}

// styleEnv is a terminal that can show everything: the mark, its colors, and
// the symbols. CLICOLOR_FORCE stands in for the terminal a test does not have.
var styleEnv = []string{"CLICOLOR_FORCE=1", "COLORTERM=truecolor", "TERM=xterm-256color", "LANG=C.UTF-8"}

const (
	// The lit side of the mark, which is also the color the TUI frames its
	// window in (cli/internal/tui/theme.go).
	markPurple = "\x1b[38;2;244;92;255m"
	// The shadow side, which appears in the mark and nowhere else, so it is
	// what says the mark itself was drawn.
	shadowPurple = "\x1b[38;2;139;47;214m"
)

func TestShellDrawsTheMarkOnlyWhereItShows(t *testing.T) {
	requireShell(t)
	s := newServer(t, ladder)
	script := s.script(t, "v1.1.0", ShellName)

	// The sentence is the same either way: nothing here reads only in color.
	plain := runShell(t, s, script, nil)
	if strings.Contains(plain.output, "\x1b[") {
		t.Errorf("a pipe got escape sequences:\n%q", plain.output)
	}
	if !strings.Contains(plain.output, "installed discobox v1.1.0 to") {
		t.Errorf("plain output lost its sentence:\n%s", plain.output)
	}

	styled := runShell(t, s, script, styleEnv)
	for _, want := range []string{shadowPurple, markPurple, "\u2713", "\u2192"} {
		if !strings.Contains(styled.output, want) {
			t.Errorf("a terminal that shows everything did not get %q:\n%q", want, styled.output)
		}
	}
	if !strings.Contains(styled.output, "installed discobox v1.1.0 to") {
		t.Errorf("styled output lost its sentence:\n%s", styled.output)
	}

	// NO_COLOR wins over being asked for color, per no-color.org.
	none := runShell(t, s, script, append(append([]string{}, styleEnv...), "NO_COLOR=1"))
	if strings.Contains(none.output, "\x1b[") {
		t.Errorf("NO_COLOR got escape sequences:\n%q", none.output)
	}

	// Sixteen colors is not enough for the mark, which is shading rather than
	// line art — the rule the TUI's newLogo applies. The messages keep theirs.
	sixteen := runShell(t, s, script, []string{"CLICOLOR_FORCE=1", "TERM=xterm", "LANG=C.UTF-8"})
	if strings.Contains(sixteen.output, shadowPurple) || strings.Contains(sixteen.output, markPurple) {
		t.Errorf("a 16-color terminal was sent the mark:\n%q", sixteen.output)
	}
	if !strings.Contains(sixteen.output, "\x1b[") {
		t.Errorf("a 16-color terminal got no color at all:\n%q", sixteen.output)
	}

	// A terminal that is not being told to expect UTF-8 gets no block
	// characters and no arrows.
	ascii := runShell(t, s, script, []string{"CLICOLOR_FORCE=1", "COLORTERM=truecolor", "TERM=xterm-256color", "LANG=C", "LC_ALL=C"})
	if strings.Contains(ascii.output, shadowPurple) || strings.Contains(ascii.output, "\u2713") {
		t.Errorf("a non-UTF-8 terminal got the mark or its symbols:\n%q", ascii.output)
	}
}

func TestPowerShellDrawsTheMarkOnlyWhereItShows(t *testing.T) {
	for _, shell := range powerShells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			s := newServer(t, ladder)
			script := s.script(t, "v1.1.0", PowerShellName)

			plain := runPowerShell(t, shell, s, script, false, nil, "")
			if strings.Contains(plain.output, "\x1b[") {
				t.Errorf("a captured stream got escape sequences:\n%q", plain.output)
			}
			if !strings.Contains(plain.output, "installed discobox v1.1.0 to") {
				t.Errorf("plain output lost its sentence:\n%s", plain.output)
			}

			styled := runPowerShell(t, shell, s, script, false, styleEnv, "")
			for _, want := range []string{shadowPurple, markPurple, "\u2713"} {
				if !strings.Contains(styled.output, want) {
					t.Errorf("a terminal that shows everything did not get %q:\n%q", want, styled.output)
				}
			}

			none := runPowerShell(t, shell, s, script, false, append(append([]string{}, styleEnv...), "NO_COLOR=1"), "")
			if strings.Contains(none.output, "\x1b[") {
				t.Errorf("NO_COLOR got escape sequences:\n%q", none.output)
			}
		})
	}
}
