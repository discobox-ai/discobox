package sourceref

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseReadsEachPart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want Ref
	}{
		{".", Ref{Kind: Directory, Location: "."}},
		{"/src/app", Ref{Kind: Directory, Location: "/src/app"}},
		{"~/src/app@main", Ref{Kind: Directory, Location: "~/src/app", Ref: "main"}},
		{"../app@v1//cmd/tool", Ref{Kind: Directory, Location: "../app", Ref: "v1", Path: "cmd/tool"}},
		{`C:\src\app@main`, Ref{Kind: Directory, Location: `C:\src\app`, Ref: "main"}},
		{"//server/share", Ref{Kind: Directory, Location: "//server/share"}},
		{"foo/bar", Ref{Kind: Shorthand, Location: "foo/bar"}},
		{"github.com/foo/bar.git@main//skills", Ref{Kind: Shorthand, Location: "github.com/foo/bar.git", Ref: "main", Path: "skills"}},
		{"https://github.com/foo/bar", Ref{Kind: Git, Location: "https://github.com/foo/bar"}},
		{"https://github.com/foo/bar.git@main", Ref{Kind: Git, Location: "https://github.com/foo/bar.git", Ref: "main"}},
		{"https://github.com/acme/tools@main//agents/skills", Ref{Kind: Git, Location: "https://github.com/acme/tools", Ref: "main", Path: "agents/skills"}},
		{"https://github.com/acme/tools//.", Ref{Kind: Git, Location: "https://github.com/acme/tools"}},
		// A user is never a ref.
		{"ssh://git@github.com/foo/bar.git", Ref{Kind: Git, Location: "ssh://git@github.com/foo/bar.git"}},
		{"https://user@example.com/foo/bar", Ref{Kind: Git, Location: "https://user@example.com/foo/bar"}},
		{"git@github.com:foo/bar.git", Ref{Kind: Git, Location: "git@github.com:foo/bar.git"}},
		{"git@github.com:acme/tools@v1//odd@name:dir", Ref{Kind: Git, Location: "git@github.com:acme/tools", Ref: "v1", Path: "odd@name:dir"}},
		{"ssh://git@example.com:2222/acme/tools.git//skills", Ref{Kind: Git, Location: "ssh://git@example.com:2222/acme/tools.git", Path: "skills"}},
		{"file:///srv/git/app.git@main", Ref{Kind: Git, Location: "file:///srv/git/app.git", Ref: "main"}},
		// Not shorthand.
		{"gitlab.com/foo/bar", Ref{Kind: Directory, Location: "gitlab.com/foo/bar"}},
		{"foo/bar/baz", Ref{Kind: Directory, Location: "foo/bar/baz"}},
		{"-foo/bar", Ref{Kind: Directory, Location: "-foo/bar"}},
		{"foo/..", Ref{Kind: Directory, Location: "foo/.."}},
		{"host:path", Ref{Kind: Directory, Location: "host:path"}},
	} {
		got, err := Parse(tc.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseRefusesAPathOutsideTheSource(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"",
		"  ",
		"https://github.com/acme/tools//../etc",
		"https://github.com/acme/tools//",
		"https://github.com/acme/tools///abs",
		"https://github.com/acme/tools//a/./b",
		`foo/bar//a\b`,
		"foo/bar//a\x01b",
	} {
		if r, err := Parse(value); err == nil {
			t.Errorf("Parse(%q) = %+v, want a refusal", value, r)
		}
	}
}

func TestStringRoundTrips(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"/src/app",
		"/src/app@main//sub/dir",
		"https://github.com/foo/bar.git@main",
		"git@github.com:acme/tools@v1//odd@name:dir",
	} {
		r, err := Parse(value)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.String(); got != value {
			t.Errorf("Parse(%q).String() = %q", value, got)
		}
	}
}

func TestResolveLetsADirectoryWin(t *testing.T) {
	base := t.TempDir()
	for _, dir := range []string{"local/repo", "github.com/local/checkout"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"foo/bar", "https://github.com/foo/bar.git"},
		{"foo/bar.git", "https://github.com/foo/bar.git"},
		{"foo/bar@main//skills", "https://github.com/foo/bar.git@main//skills"},
		{"github.com/foo/bar@v1.2.3", "https://github.com/foo/bar.git@v1.2.3"},
		{"my-org/my_repo.js", "https://github.com/my-org/my_repo.js.git"},
		{"local/repo", filepath.Join(base, "local/repo")},
		{"local/repo@main", filepath.Join(base, "local/repo") + "@main"},
		{"github.com/local/checkout", filepath.Join(base, "github.com/local/checkout")},
		{"gitlab.com/foo/bar", filepath.Join(base, "gitlab.com/foo/bar")},
		{".", base},
		{"https://github.com/foo/bar", "https://github.com/foo/bar"},
	} {
		r, err := Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		r, err = r.Resolve(base)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tc.in, err)
		}
		if got := r.String(); got != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	r, err := Parse("~/src/app@main")
	if err != nil {
		t.Fatal(err)
	}
	if r, err = r.Resolve(""); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "src", "app"); r.Location != want || r.Kind != Directory {
		t.Fatalf("Resolve = %+v, want the directory %s", r, want)
	}
	if got, want := r.Display(home), "~"+string(filepath.Separator)+filepath.Join("src", "app")+"@main"; got != want {
		t.Fatalf("Display = %q, want %q", got, want)
	}
}

// Every spelling of one repository is one source, and the ref and path are
// part of which source it is.
func TestKeyIsOneForEverySpelling(t *testing.T) {
	t.Parallel()
	same := func(values ...string) {
		t.Helper()
		var first string
		for i, value := range values {
			r, err := Parse(value)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				first = r.Key()
			} else if r.Key() != first {
				t.Errorf("Key(%q) = %q, want %q (the key of %q)", value, r.Key(), first, values[0])
			}
		}
	}
	same("foo/bar", "Foo/Bar.git", "github.com/foo/bar", "https://github.com/foo/bar", "https://github.com/foo/bar.git",
		"https://GitHub.com/Foo/Bar/", "git@github.com:foo/bar.git", "ssh://git@github.com/foo/bar")
	same("foo/bar@main//skills", "https://github.com/foo/bar.git@main//skills")
	same("https://gitlab.com/g/p.git", "git@gitlab.com:g/p", "ssh://git@gitlab.com:22/g/p.git")
	same("/src/app", "/src/app/", "/src/./app")

	differ := [][2]string{
		{"foo/bar", "foo/bar@main"},
		{"foo/bar", "foo/bar//skills"},
		{"https://gitlab.com/g/p", "https://gitlab.com/G/P"},
		{"ssh://git@example.com:2222/g/p", "ssh://git@example.com/g/p"},
	}
	for _, pair := range differ {
		a, _ := Parse(pair[0])
		b, _ := Parse(pair[1])
		if a.Key() == b.Key() {
			t.Errorf("%q and %q share the key %q", pair[0], pair[1], a.Key())
		}
	}
}

// What is displayed parses back to the same source, reached the same way:
// only an https GitHub URL is shortened, since owner/repo stands for https.
func TestDisplayIsAReferenceToTheSameSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"https://github.com/Foo/bar.git", "Foo/bar"},
		{"https://github.com/foo/bar.git@main//skills", "foo/bar@main//skills"},
		{"git@github.com:foo/bar.git@main//skills", "git@github.com:foo/bar@main//skills"},
		{"http://github.com/foo/bar", "http://github.com/foo/bar"},
		{"https://gitlab.com/g/p.git", "https://gitlab.com/g/p"},
		{"git@gitlab.com:g/p.git@v1", "git@gitlab.com:g/p@v1"},
		{"/srv/app@main", "/srv/app@main"},
		{"/home/me/app", "~" + string(filepath.Separator) + "app"},
	} {
		r, err := Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		got := r.Display(filepath.FromSlash("/home/me"))
		if got != tc.want {
			t.Errorf("Display(%q) = %q, want %q", tc.in, got, tc.want)
		}
		back, err := Parse(got)
		if err != nil {
			t.Fatalf("Parse(%q): %v", got, err)
		}
		if back.Kind == Directory {
			continue // ~ is the caller's home, which Resolve reads
		}
		if back.CloneKey() != r.CloneKey() {
			t.Errorf("Display(%q) = %q keys as %q, want %q", tc.in, got, back.CloneKey(), r.CloneKey())
		}
	}
	if got := Display("/home/me/app", ""); got != "/home/me/app" {
		t.Errorf("Display with no home = %q, want the path as it is", got)
	}
	if got := Display("@", ""); got != "@" {
		t.Errorf("Display of a value that does not parse = %q, want it as given", got)
	}
}

// Two spellings fetched alike are one thing to cut from; one repository
// over another transport is not, since what reaches it differs.
func TestCloneKeyKeepsTheTransport(t *testing.T) {
	t.Parallel()
	key := func(value string) string {
		t.Helper()
		r, err := Parse(value)
		if err != nil {
			t.Fatal(err)
		}
		return r.CloneKey()
	}
	for _, same := range [][2]string{
		{"https://github.com/foo/bar.git", "https://GitHub.com/Foo/Bar"},
		{"foo/bar", "https://github.com/foo/bar.git"},
		{"git@github.com:foo/bar.git", "ssh://git@github.com/foo/bar"},
		{"git+ssh://git@github.com/foo/bar", "git@github.com:foo/bar"},
	} {
		if key(same[0]) != key(same[1]) {
			t.Errorf("%q and %q have CloneKeys %q and %q, want one", same[0], same[1], key(same[0]), key(same[1]))
		}
	}
	for _, other := range []string{"git@github.com:foo/bar", "http://github.com/foo/bar", "git://github.com/foo/bar"} {
		if key(other) == key("https://github.com/foo/bar") {
			t.Errorf("%q shares https's CloneKey", other)
		}
	}
	r, _ := Parse("https://github.com/foo/bar.git@main//skills")
	if got := r.RepositoryKey(); got != "github.com/foo/bar" {
		t.Errorf("RepositoryKey = %q, want the repository alone", got)
	}
}

func TestNameIsTheLastElement(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://github.com/foo/bar.git@main": "bar",
		"git@github.com:foo/bar.git":          "bar",
		"ssh://git@example.com:2222/a/b/":     "b",
		"file:///srv/git/app.git":             "app",
		"/src/app@main":                       "app",
	} {
		r, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Name(); got != want {
			t.Errorf("Name(%q) = %q, want %q", in, got, want)
		}
	}
}

// A directory that is there wins even when its own name holds `@` or `//`.
func TestResolveTakesADirectoryWhoseNameLooksLikeARef(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "GoogleDrive-me@example.com", "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Parse(filepath.Join("GoogleDrive-me@example.com", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if r, err = r.Resolve(base); err != nil {
		t.Fatal(err)
	}
	if r.Location != dir || r.Ref != "" || r.Path != "" {
		t.Fatalf("Resolve = %+v, want the directory %s", r, dir)
	}
	// Its spelling resolves back to it, which Parse alone cannot do.
	again, err := Parse(r.String())
	if err != nil {
		t.Fatal(err)
	}
	if again, err = again.Resolve(base); err != nil || again.Key() != r.Key() {
		t.Fatalf("Resolve(Parse(%q)) = %+v, %v; want the directory again", r.String(), again, err)
	}
	r, _ = Parse("app@main")
	if r, err = r.Resolve(base); err != nil || r.Ref != "main" {
		t.Fatalf("Resolve of a directory at a ref = %+v, %v; want the ref kept", r, err)
	}
}

func TestGitHubURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"https://github.com/foo/bar.git", "https://github.com/foo/bar"},
		{"https://github.com/foo/bar", "https://github.com/foo/bar"},
		{"https://github.com/foo/bar/", "https://github.com/foo/bar"},
		{"https://GitHub.com/foo/bar", "https://github.com/foo/bar"},
		{"https://user@github.com/foo/bar.git", "https://github.com/foo/bar"},
		{"ssh://git@github.com/foo/bar.git", "https://github.com/foo/bar"},
		{"ssh://git@github.com:22/foo/bar.git", "https://github.com/foo/bar"},
		{"git@github.com:foo/bar.git", "https://github.com/foo/bar"},
		{"github.com:my-org/my_repo.js", "https://github.com/my-org/my_repo.js"},
		{"git://github.com/foo/bar.git", "https://github.com/foo/bar"},
		// Not GitHub, or not a repository on it.
		{"", ""},
		{"https://gitlab.com/foo/bar.git", ""},
		{"git@gitlab.com:foo/bar.git", ""},
		{"https://github.com/foo", ""},
		{"https://github.com/foo/bar/baz", ""},
		{"https://github.com/-foo/bar", ""},
		{"/src/foo/bar", ""},
		{"file:///src/foo/bar", ""},
		{"file://github.com/foo/bar.git", ""},
		{"ftp://github.com/foo/bar.git", ""},
	} {
		if got := GitHubURL(tc.in); got != tc.want {
			t.Errorf("GitHubURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Every spelling of one repository's remote has one key, whatever its case.
func TestGitHubKey(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{
		"https://github.com/discobox-ai/discobox",
		"https://github.com/Discobox-AI/Discobox.git",
		"git@github.com:discobox-ai/discobox.git",
		"ssh://git@github.com/discobox-ai/DISCOBOX",
	} {
		if got := GitHubKey(remote); got != "discobox-ai/discobox" {
			t.Fatalf("GitHubKey(%q) = %q, want discobox-ai/discobox", remote, got)
		}
	}
	for _, remote := range []string{"", "/src/disco2", "https://gitlab.com/discobox-ai/discobox"} {
		if got := GitHubKey(remote); got != "" {
			t.Fatalf("GitHubKey(%q) = %q, want none", remote, got)
		}
	}
}

func TestEverySpellingOfARemoteIsOneRemote(t *testing.T) {
	t.Parallel()
	const want = "github.com/Acme/widget"
	for _, remote := range []string{
		"https://github.com/Acme/widget",
		"https://github.com/Acme/widget.git",
		"https://GitHub.com/Acme/widget.git/",
		"https://user:token@github.com:443/Acme/widget.git",
		"http://github.com:80/Acme/widget",
		"ssh://git@github.com/Acme/widget.git",
		"ssh://git@github.com:22/Acme/widget.git",
		"git+ssh://git@github.com/Acme/widget.git",
		"git+ssh://git@github.com:22/Acme/widget.git",
		"ssh+git://github.com:22/Acme/widget",
		"git://github.com/Acme/widget.git",
		"git@github.com:Acme/widget.git",
		"github.com:Acme/widget",
		"  git@github.com:/Acme//widget.git/ ",
	} {
		if got := NormalizeRemote(remote); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", remote, got, want)
		}
	}
	if got, want := NormalizeRemote("ssh://git@git.example.com:2222/team/app.git"), "git.example.com:2222/team/app"; got != want {
		t.Fatalf("a remote on another port = %q, want %q", got, want)
	}
}

// A path names a repository only on the machine it is a path on, so it is
// not a remote.
func TestAPathIsNotARemote(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{
		"",
		"/srv/git/app.git",
		"../app",
		"./app:odd",
		"file:///srv/git/app.git",
		`C:\src\app`,
		"C:/src/app",
		"https://github.com",
		"https://github.com/",
		"git@github.com:",
		"has space@host:path",
	} {
		if got := NormalizeRemote(remote); got != "" {
			t.Errorf("NormalizeRemote(%q) = %q, want none", remote, got)
		}
	}
}
