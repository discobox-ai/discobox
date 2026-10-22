package sandboxcreate

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// A repository declaring foo means the ../foo the caller already has: the
// checkout is used, at its own path, exactly as -i ../foo would.
func TestBuildPromptSandboxBodyPrefersACheckoutOfADeclaredSource(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	reference := newRunSourceTestRepoIn(t, workspace, "foo")
	declareSources(t, primary, map[string]string{"foo": "https://github.com/acme/foo"})

	var reported []DeclaredSource
	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{Platform: linuxSandbox,
		Source:               primary,
		IncludeDirty:         IncludeDirtyNever,
		ReportDeclaredSource: func(source DeclaredSource) { reported = append(reported, source) },
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	references, _ := body.Config.SourceCodeReferences.Get()
	source, ok := references[wantSandboxPath(t, reference)]
	if !ok {
		t.Fatalf("references are keyed %v, want the local checkout %s", references, reference)
	}
	if source.Slug.Or("") != "foo" || source.LocalDirectory.Or("") != reference {
		t.Fatalf("source = %v, want the local checkout under the declared name", source)
	}
	if _, ok := source.URL.Get(); ok {
		t.Fatalf("source = %v, want it taken from disk rather than cloned", source)
	}
	if len(reported) != 1 || !reported[0].Local || reported[0].Checkout != reference {
		t.Fatalf("reported = %+v, want the local checkout announced", reported)
	}
}

// With no checkout beside the source, the declared URL is cloned — and it lands
// at the path the checkout would have occupied, so ../foo means the same thing
// inside the sandbox either way. That is the whole promise of declaring it.
func TestBuildPromptSandboxBodyClonesADeclaredSourceToTheSamePath(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	// A repository on a network as far as the declaration is concerned,
	// which this machine's Git is told to find on disk, so the test needs no
	// network. A file:// URL itself is refused: a declaration is repository
	// content, and may not name a path on whoever's machine runs the create.
	const remote = "https://git.invalid/acme/foo"
	served := "file://" + filepath.ToSlash(newRunSourceTestRepoIn(t, t.TempDir(), "foo"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+served+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", remote)
	declareSources(t, primary, map[string]string{"foo": remote})

	var reported []DeclaredSource
	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{Platform: linuxSandbox,
		Source:               primary,
		IncludeDirty:         IncludeDirtyNever,
		ReportDeclaredSource: func(source DeclaredSource) { reported = append(reported, source) },
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	want := filepath.Join(workspace, "foo")
	references, _ := body.Config.SourceCodeReferences.Get()
	source, ok := references[wantSandboxPath(t, want)]
	if !ok {
		t.Fatalf("references are keyed %v, want the sibling path %s a checkout would have used", references, want)
	}
	url, ok := source.URL.Get()
	if !ok || url.String() != remote {
		t.Fatalf("source = %v, want it cloned from the declared URL %s", source, remote)
	}
	if checkout, ok := source.Checkout.Get(); !ok || checkout.Commit.Or("") == "" {
		t.Fatalf("checkout = %v, want the remote's commit resolved at create", checkout)
	}
	if len(reported) != 1 || reported[0].Local || reported[0].Checkout != want {
		t.Fatalf("reported = %+v, want the missing checkout named", reported)
	}
}

// A fork checked out next door is used, because it is what the caller has — but
// the disagreement is reported, since a directory that merely shares the name
// looks identical from here.
func TestBuildPromptSandboxBodyReportsACheckoutThatDisagreesWithTheDeclaredURL(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	reference := newRunSourceTestRepoIn(t, workspace, "foo")
	runSourceTestGit(t, reference)("remote", "add", "origin", "https://github.com/darren/foo.git")
	declareSources(t, primary, map[string]string{"foo": "https://github.com/acme/foo"})

	var reported []DeclaredSource
	_, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{Platform: linuxSandbox,
		Source:               primary,
		IncludeDirty:         IncludeDirtyNever,
		ReportDeclaredSource: func(source DeclaredSource) { reported = append(reported, source) },
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	if len(reported) != 1 || !reported[0].Local {
		t.Fatalf("reported = %+v, want the checkout used anyway", reported)
	}
	if reported[0].Origin != "https://github.com/darren/foo.git" {
		t.Fatalf("reported origin = %q, want the checkout's own remote", reported[0].Origin)
	}
}

// The same repository over ssh, over https, with or without .git is the same
// repository, and must not be reported as a disagreement.
func TestDeclaredSourceOriginMatchesAcrossURLForms(t *testing.T) {
	same := [][2]string{
		{"https://github.com/acme/foo.git", "https://github.com/acme/foo"},
		{"git@github.com:acme/foo.git", "https://github.com/acme/foo"},
		{"ssh://git@github.com/acme/foo", "https://github.com/acme/foo/"},
		{"https://GitHub.com/Acme/Foo", "https://github.com/acme/foo"},
	}
	for _, pair := range same {
		if !sameGitRemote(pair[0], pair[1]) {
			t.Errorf("%q and %q were read as different repositories", pair[0], pair[1])
		}
	}
	if sameGitRemote("https://github.com/acme/foo", "https://github.com/darren/foo") {
		t.Error("two different repositories were read as the same one")
	}
}

// An explicit -i is the caller's own instruction and outranks the repository's
// declaration of the same source, rather than colliding with it.
func TestBuildPromptSandboxBodyLetsIncludeOverrideADeclaredSource(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	reference := newRunSourceTestRepoIn(t, workspace, "foo")
	declareSources(t, primary, map[string]string{"foo": "https://github.com/acme/foo"})

	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{Platform: linuxSandbox,
		Source:       primary,
		Include:      []string{reference},
		IncludeDirty: IncludeDirtyNever,
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	references, _ := body.Config.SourceCodeReferences.Get()
	if len(references) != 1 {
		t.Fatalf("references = %v, want the one source, not it and its declaration", references)
	}
	if slug := references[wantSandboxPath(t, reference)].Slug.Or(""); slug != "foo" {
		t.Fatalf("slug = %q, want foo taken once", slug)
	}
}

// Declared sources are what the repository asks for, so opting out has to be
// possible for a caller who wants only what they named.
func TestBuildPromptSandboxBodySkipsDeclaredSourcesWhenAsked(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	newRunSourceTestRepoIn(t, workspace, "foo")
	declareSources(t, primary, map[string]string{"foo": "https://github.com/acme/foo"})

	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{Platform: linuxSandbox,
		Source:              primary,
		IncludeDirty:        IncludeDirtyNever,
		SkipDeclaredSources: true,
	})
	if err != nil {
		t.Fatalf("build prompt sandbox body: %v", err)
	}
	defer local.Close()

	if references, ok := body.Config.SourceCodeReferences.Get(); ok && len(references) > 0 {
		t.Fatalf("references = %v, want none once declared sources are skipped", references)
	}
}

// The file states what the sandbox must contain. A file that cannot be read
// means that statement is unknown, and creating a sandbox missing the sources
// it names is worse than not creating one.
// A declared source may be GitHub shorthand, which is always the repository:
// a declaration never names a directory, so one that happens to be there is
// not consulted. //PATH is refused, as on any source but the primary.
func TestCheckDeclaredSourcesTakesGitHubShorthand(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join("acme", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := checkDeclaredSources(map[string]string{"lib": "acme/lib@v1", "tools": "github.com/acme/tools"}, "settings.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["lib"] != "https://github.com/acme/lib.git@v1" || got["tools"] != "https://github.com/acme/tools.git" {
		t.Fatalf("declared = %v, want the shorthand as GitHub URLs", got)
	}
	if _, err := checkDeclaredSources(map[string]string{"lib": "acme/lib//sub"}, "settings.json"); err == nil || !strings.Contains(err.Error(), "//sub") {
		t.Fatalf("a declared //PATH = %v, want a refusal naming it", err)
	}
}

func TestBuildPromptSandboxBodyRefusesAMalformedDeclaration(t *testing.T) {
	workspace := testWorkspace(t)
	primary := newRunSourceTestRepoIn(t, workspace, "app")
	if err := os.MkdirAll(filepath.Join(primary, ".discobox"), 0o755); err != nil {
		t.Fatal(err)
	}
	for file, data := range map[string]string{"settings.json": `{"sources": "x"}`, "sources.json": "{not json"} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join(primary, ".discobox", file)
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)

			_, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{Platform: linuxSandbox,
				Source:       primary,
				IncludeDirty: IncludeDirtyNever,
			})
			defer local.Close()
			if err == nil {
				t.Fatalf("a malformed %s: got nil error, want a refusal", file)
			}
			if !strings.Contains(err.Error(), file) {
				t.Fatalf("error = %v, want it to name the file", err)
			}
		})
	}
}

// Where a declared source is looked for locally is not the file's to say: the
// checkout beside the primary source is found by name. A path here would be
// resolved against whatever directory the caller ran from, so it is refused
// rather than quietly bringing in some other repository.
func TestBuildPromptSandboxBodyRefusesAPathInsteadOfAURL(t *testing.T) {
	for _, value := range []string{"..", "../foo", "/srv/foo", "./foo"} {
		t.Run(value, func(t *testing.T) {
			workspace := testWorkspace(t)
			primary := newRunSourceTestRepoIn(t, workspace, "app")
			newRunSourceTestRepoIn(t, workspace, "foo")
			declareSources(t, primary, map[string]string{"foo": value})

			_, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{Platform: linuxSandbox,
				Source:       primary,
				IncludeDirty: IncludeDirtyNever,
			})
			defer local.Close()
			if err == nil {
				t.Fatalf("declared source %q: got nil error, want a refusal", value)
			}
			if !strings.Contains(err.Error(), "not a Git URL") {
				t.Fatalf("error = %v, want it to say the value is not a Git URL", err)
			}
		})
	}
}

func declareSources(t *testing.T, root string, declared map[string]string) {
	t.Helper()
	writeSettingsFile(t, root, "settings.json", map[string]any{"sources": declared})
}

// writeSettingsFile writes one of a repository's settings files, as JSON.
func writeSettingsFile(t *testing.T, root, name string, content any) {
	t.Helper()
	dir := filepath.Join(root, ".discobox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// settingsLayouts are the repositories ADR 26-10-16-423 §11 tells apart: one
// with settings.json alone, one with only the files it replaces, and one with
// both. Each declares lib, and the old files declare it at another ref, so a
// merge would show.
var settingsLayouts = []struct {
	name  string
	files map[string]any
	// lib is what lib resolves to; empty for a refusal.
	lib string
	// warned says the create warns about the old files, and unknownWarned
	// about keys it does not read.
	warned        bool
	unknownWarned bool
}{
	{
		name:  "settings.json alone",
		files: map[string]any{"settings.json": map[string]any{"$schema": "https://example.invalid/s.json", "sources": map[string]string{"lib": "https://github.com/acme/lib@v2"}, "features": map[string]any{"brew": false}, "runCommand": []string{"make"}}},
		lib:   "https://github.com/acme/lib@v2",
	},
	{
		name:  "settings.json with a key this client does not read",
		files: map[string]any{"settings.json": map[string]any{"sources": map[string]string{"lib": "https://github.com/acme/lib@v2"}, "someday": true}},
		// A key is repository content: one spelling a control character
		// reaches the warning quoted, never raw.
		lib:           "https://github.com/acme/lib@v2",
		unknownWarned: true,
	},
	{
		name:   "the old files alone",
		files:  map[string]any{"sources.json": map[string]string{"lib": "https://github.com/acme/lib@v1"}, "project.json": map[string]any{"runCommand": []string{"make"}}},
		lib:    "https://github.com/acme/lib@v1",
		warned: true,
	},
	{
		name:  "both",
		files: map[string]any{"settings.json": map[string]any{"sources": map[string]string{"lib": "https://github.com/acme/lib@v2"}}, "project.json": map[string]any{}},
	},
}

// A local primary source's settings, in each layout.
func TestDeclaredSourcesOfEachSettingsLayout(t *testing.T) {
	for _, layout := range settingsLayouts {
		t.Run(layout.name, func(t *testing.T) {
			primary := newRunSourceTestRepoIn(t, testWorkspace(t), "app")
			for name, content := range layout.files {
				writeSettingsFile(t, primary, name, content)
			}
			var warnings []string
			settings, err := declaredPromptSettings(context.Background(), PromptOptions{Warn: func(w string) { warnings = append(warnings, w) }},
				resolvedRunSource{LocalDirectory: primary})
			declared := settings.Sources
			checkSettingsLayout(t, layout.lib, layout.warned, layout.unknownWarned, declared, warnings, err)
		})
	}
}

// A remote primary source's settings, read at its pinned commit, in each
// layout.
func TestRemoteDeclaredSourcesOfEachSettingsLayout(t *testing.T) {
	for _, layout := range settingsLayouts {
		t.Run(layout.name, func(t *testing.T) {
			remote := newRunSourceTestRepoIn(t, t.TempDir(), "widget")
			git := runSourceTestGit(t, remote)
			for name, content := range layout.files {
				writeSettingsFile(t, remote, name, content)
			}
			git("add", ".discobox")
			git("commit", "-m", "settings")
			git("config", "uploadpack.allowFilter", "true")
			primary := resolvedRunSource{
				Kind:     runSourceKindGit,
				URL:      "file://" + filepath.ToSlash(remote),
				Checkout: resolvedRunSourceCheckout{Commit: strings.TrimSpace(git("rev-parse", "HEAD")), RefName: "main", RefType: runSourceRefTypeBranch},
			}
			var warnings []string
			settings, err := declaredPromptSettings(context.Background(), PromptOptions{Warn: func(w string) { warnings = append(warnings, w) }}, primary)
			declared := settings.Sources
			checkSettingsLayout(t, layout.lib, layout.warned, layout.unknownWarned, declared, warnings, err)
			if len(warnings) > 0 && !strings.HasPrefix(warnings[0], primary.URL+"@") {
				t.Fatalf("warning %q does not name the remote", warnings[0])
			}
		})
	}
}

// A sources.json moved into place without its "sources" wrapper is refused,
// $schema or not, rather than read as a repository declaring nothing; a file
// of keys a newer discobox added is read past with a warning, never refused.
func TestASettingsFileInTheShapeOfASourcesFile(t *testing.T) {
	for name, tc := range map[string]struct {
		content any
		refused bool
	}{
		"unwrapped":              {map[string]string{"lib": "https://github.com/acme/lib"}, true},
		"unwrapped with $schema": {map[string]string{"$schema": "https://example.invalid/s.json", "lib": "https://github.com/acme/lib"}, true},
		"a newer discobox's key": {map[string]any{"$schema": "https://example.invalid/s.json", "skills": []string{"x"}}, false},
		"a newer string key":     {map[string]any{"lib": "https://github.com/acme/lib", "features": map[string]any{}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			primary := newRunSourceTestRepoIn(t, testWorkspace(t), "app")
			writeSettingsFile(t, primary, "settings.json", tc.content)
			var warnings []string
			settings, err := declaredPromptSettings(context.Background(), PromptOptions{Warn: func(w string) { warnings = append(warnings, w) }},
				resolvedRunSource{LocalDirectory: primary})
			declared := settings.Sources
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), `holds "lib" at its top level`) {
					t.Fatalf("declared = %v, %v; want the file refused, naming the key", declared, err)
				}
				return
			}
			if err != nil || len(declared) != 0 || len(warnings) != 1 {
				t.Fatalf("declared = %v, %v, warnings %q; want nothing declared and one warning", declared, err, warnings)
			}
		})
	}
}

// A key is repository content: one spelling a control character is quoted
// wherever it is named, never handed to a terminal raw.
func TestQuoteKeysQuotesRepositoryContent(t *testing.T) {
	keys := map[string]json.RawMessage{"\x1b]52;c;eA==\a": nil, "a": nil}
	if quoted := quoteKeys(keys); quoted != `"\x1b]52;c;eA==\a", "a"` {
		t.Fatalf("quoteKeys = %s, want each key Go-quoted, sorted", quoted)
	}
}

func checkSettingsLayout(t *testing.T, lib string, warned, unknownWarned bool, declared map[string]string, warnings []string, err error) {
	t.Helper()
	if lib == "" {
		if err == nil || !strings.Contains(err.Error(), "settings.json replaces .discobox/project.json") {
			t.Fatalf("declared = %v, %v; want the repository refused for holding both", declared, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("declared sources: %v", err)
	}
	if len(declared) != 1 || declared["lib"] != lib {
		t.Fatalf("declared = %v, want lib at %s alone", declared, lib)
	}
	if unknownWarned {
		if len(warnings) != 1 || !strings.Contains(warnings[0], `sets "someday", which this discobox does not read`) {
			t.Fatalf("warnings = %q, want one naming the unknown key", warnings)
		}
		return
	}
	if !warned {
		if len(warnings) != 0 {
			t.Fatalf("warnings = %q, want none", warnings)
		}
		return
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], ".discobox/sources.json and .discobox/project.json are read because the repository has no .discobox/settings.json") {
		t.Fatalf("warnings = %q, want one naming both old files", warnings)
	}
}

// newRunSourceTestRepoIn is a repository at a named path inside a workspace, so
// a test can put two of them side by side the way a caller's checkouts are.
// testWorkspace is a directory a test's sources live in: a temporary directory
// whose name matches what git reports for it, registered for the length of the
// test as a root a sandbox may hold.
//
// The name has to match because these tests compare a path they built against
// one that came back through source resolution — Windows hands out an 8.3 short
// name (RUNNER~1) and macOS hands out /var where git says /private/var.
//
// It is registered because every source here is built under t.TempDir(), which
// is /tmp — the one root this package deliberately refuses to mirror into a
// sandbox, because the sandbox's own systemd owns that directory (ADR 26-09-09-044 on
// source host paths). Registering it keeps these tests about what each of them
// is about; the clamp itself is asserted from an unregistered path, in
// source_test.go.
func testWorkspace(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The parent, because a source is often the workspace directory itself and
	// a root is mirrored for its children rather than for itself.
	mirrorRootForTest(t, filepath.Dir(dir))
	return dir
}

// mirrorRootForTest makes root a directory a source may keep its own path
// under, until the test ends.
func mirrorRootForTest(t *testing.T, root string) {
	t.Helper()
	previous := mirrorableSourceRoots
	mirrorableSourceRoots = append(append([]string{}, previous...), filepath.ToSlash(root))
	t.Cleanup(func() { mirrorableSourceRoots = previous })
}

func newRunSourceTestRepoIn(t *testing.T, workspace, name string) string {
	t.Helper()
	repo := filepath.Join(workspace, name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := runSourceTestGit(t, repo)
	git("init", "-b", "main")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("commit", "-m", "base")
	return repo
}

// A remote primary source declares sources too: the file is read from its
// remote at the pinned commit, without cloning it, and every source it names
// is its URL, since there is no directory on this machine to look beside (ADR
// 26-10-10-545 §4).
func TestARemotePrimarySourceDeclaresItsSources(t *testing.T) {
	remote := newRunSourceTestRepoIn(t, t.TempDir(), "widget")
	git := runSourceTestGit(t, remote)
	before := strings.TrimSpace(git("rev-parse", "HEAD"))
	if err := os.MkdirAll(filepath.Join(remote, ".discobox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, ".discobox", "settings.json"), []byte(`{"sources": {"lib": "https://github.com/acme/lib@main"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".discobox/settings.json")
	git("commit", "-m", "declare lib")
	declaring := strings.TrimSpace(git("rev-parse", "HEAD"))
	url := "file://" + filepath.ToSlash(remote)

	for name, allowFilter := range map[string]string{"a remote that takes a filtered fetch": "true", "a remote that refuses one": "false"} {
		t.Run(name, func(t *testing.T) {
			git("config", "uploadpack.allowFilter", allowFilter)
			settings, err := readRemoteSettings(context.Background(), url, declaring, "main")
			if err != nil {
				t.Fatalf("read settings: %v", err)
			}
			declared, err := declaredSources(settings, url)
			if err != nil {
				t.Fatalf("read declared sources: %v", err)
			}
			if len(declared) != 1 || declared["lib"] != "https://github.com/acme/lib@main" {
				t.Fatalf("declared = %v, want lib alone", declared)
			}
		})
	}

	// The commit pinned, not the remote's tip: before the file existed there
	// was nothing declared.
	settings, err := readRemoteSettings(context.Background(), url, before, "")
	if err != nil || settings.Path != "" {
		t.Fatalf("settings at a commit without the file = %+v, %v; want none", settings, err)
	}

	// Through the create: a remote primary's declared source is brought in by
	// its URL, placed beside where the primary lands.
	primary := resolvedRunSource{
		Kind:        runSourceKindGit,
		URL:         url,
		Checkout:    resolvedRunSourceCheckout{Commit: declaring, RefName: "main", RefType: runSourceRefTypeBranch},
		Destination: defaultRunDestination(linuxSandbox),
	}
	declared, err := declaredPromptSettings(context.Background(), PromptOptions{}, primary)
	got := declared.Sources
	if err != nil || got["lib"] == "" {
		t.Fatalf("declared sources of a remote primary = %v, %v", got, err)
	}
	if skipped, err := declaredPromptSettings(context.Background(), PromptOptions{SkipDeclaredSources: true}, primary); err != nil || skipped.Sources != nil {
		t.Fatalf("declared sources when opted out = %v, %v", skipped.Sources, err)
	}
	arg, report := resolveDeclaredSourceArg(context.Background(), "", "lib", got["lib"])
	if arg != got["lib"] || report.Local || report.Checkout != "" {
		t.Fatalf("a remote primary's declared source resolved to %q (%+v), want its URL and no local checkout", arg, report)
	}

	// A declaration is repository content, fetched by this client and
	// delivered into the discobox: it may not name this machine's disk.
	if _, err := checkDeclaredSources(map[string]string{"lib": "file:///home/me/private"}, "settings.json"); err == nil || !strings.Contains(err.Error(), "a path on this machine") {
		t.Fatalf("a declared file:// URL was accepted: %v", err)
	}

	// A malformed file is an error there as it is on disk.
	if err := os.WriteFile(filepath.Join(remote, ".discobox", "settings.json"), []byte(`{"sources": {"lib": "../lib"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-am", "declare a path")
	primary.Checkout.Commit = strings.TrimSpace(git("rev-parse", "HEAD"))
	if _, err := declaredPromptSettings(context.Background(), PromptOptions{}, primary); err == nil || !strings.Contains(err.Error(), "not a Git URL") {
		t.Fatalf("a declared path was accepted: %v", err)
	}
}

// A repository's features ride the create request whether or not its declared
// sources are brought in: turning a feature off is not a source.
func TestDeclaredFeaturesAreReadEvenWithoutDeclaredSources(t *testing.T) {
	primary := newRunSourceTestRepoIn(t, testWorkspace(t), "app")
	writeSettingsFile(t, primary, "settings.json", map[string]any{"features": map[string]any{"brew": false, "nix": map[string]any{"enabled": true}}})
	for _, skip := range []bool{false, true} {
		settings, err := declaredPromptSettings(context.Background(), PromptOptions{SkipDeclaredSources: skip}, resolvedRunSource{LocalDirectory: primary})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]sandboxconfig.Feature{"brew": {Enabled: false}, "nix": {Enabled: true}}
		if !maps.Equal(settings.Features, want) {
			t.Errorf("skip %v: features = %v, want %v", skip, settings.Features, want)
		}
	}
}
