package boot

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

// selfIdentity is a home directory owned by whoever runs the test. seedGitConfig
// chowns what it writes, and only root may chown to somebody else, so the ids
// have to be this process's own for the write path to be exercised at all.
func selfIdentity(t *testing.T) identity {
	t.Helper()
	return identity{uid: os.Getuid(), gid: os.Getgid(), name: "dev", home: t.TempDir()}
}

// fakeGit backs a booter for seedGitConfig: the keys git would report already
// set, and the config writes recorded.
type fakeGit struct {
	set   map[string]string
	runs  [][]string
	noGit bool
}

func newFakeGitBooter(alreadySet map[string]string) (*booter, *fakeGit) {
	f := &fakeGit{set: alreadySet}
	if f.set == nil {
		f.set = map[string]string{}
	}
	return &booter{
		run: func(name string, args ...string) error {
			f.runs = append(f.runs, append([]string{name}, args...))
			// git creates the file it is asked to write, and seedGitConfig
			// chowns it afterwards; a fake that records without creating would
			// leave that chown untested.
			return touchGitConfig(args)
		},
		lookup: func(_ string, args ...string) (string, bool) {
			// env HOME=... GIT_CONFIG_GLOBAL=... git config --get <key>
			key := args[len(args)-1]
			value, ok := f.set[key]
			return value, ok
		},
		exists: func(name string, _ ...string) bool {
			return !f.noGit || name != "git"
		},
	}, f
}

// touchGitConfig creates the file the invocation's GIT_CONFIG_GLOBAL names,
// standing in for the write the real git would have done.
func touchGitConfig(args []string) error {
	for _, arg := range args {
		path, ok := strings.CutPrefix(arg, "GIT_CONFIG_GLOBAL=")
		if !ok {
			continue
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		return file.Close()
	}
	return nil
}

// configWrites reduces recorded runs to the key/value pairs git was asked to
// set, so assertions read as intent rather than as argv.
func (f *fakeGit) configWrites() map[string]string {
	out := map[string]string{}
	for _, run := range f.runs {
		i := slices.Index(run, "--global")
		if i < 0 || i+2 >= len(run) {
			continue
		}
		out[run[i+1]] = run[i+2]
	}
	return out
}

func TestSeedGitConfigSetsBothKeysWhenNeitherIsConfigured(t *testing.T) {
	b, f := newFakeGitBooter(nil)
	git := sandboxconfig.GitIdentity{UserName: "Ada Lovelace", UserEmail: "ada@example.com"}
	if err := b.seedGitConfig(selfIdentity(t), git); err != nil {
		t.Fatalf("seedGitConfig: %v", err)
	}
	want := map[string]string{"user.name": "Ada Lovelace", "user.email": "ada@example.com"}
	if got := f.configWrites(); !mapsEqual(got, want) {
		t.Fatalf("config writes = %v, want %v", got, want)
	}
}

// The point of keying on the value rather than on the file: a .gitconfig full of
// aliases still has no identity, and is exactly the case that needs seeding.
func TestSeedGitConfigFillsOnlyTheMissingKey(t *testing.T) {
	b, f := newFakeGitBooter(map[string]string{"user.email": "mine@example.com"})
	git := sandboxconfig.GitIdentity{UserName: "Manifest", UserEmail: "manifest@example.com"}
	if err := b.seedGitConfig(selfIdentity(t), git); err != nil {
		t.Fatalf("seedGitConfig: %v", err)
	}
	want := map[string]string{"user.name": "Manifest"}
	if got := f.configWrites(); !mapsEqual(got, want) {
		t.Fatalf("config writes = %v, want %v: a configured key is never overwritten", got, want)
	}
}

func TestSeedGitConfigWritesNothingWhenBothAreConfigured(t *testing.T) {
	b, f := newFakeGitBooter(map[string]string{"user.name": "Mine", "user.email": "mine@example.com"})
	git := sandboxconfig.GitIdentity{UserName: "Manifest", UserEmail: "manifest@example.com"}
	if err := b.seedGitConfig(selfIdentity(t), git); err != nil {
		t.Fatalf("seedGitConfig: %v", err)
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %v, want none", f.runs)
	}
}

// A key git reports as empty is not configured. Git stores an empty value
// happily, and it commits no better than a missing key does.
func TestSeedGitConfigTreatsAnEmptyValueAsUnset(t *testing.T) {
	b, f := newFakeGitBooter(map[string]string{"user.name": "  "})
	git := sandboxconfig.GitIdentity{UserName: "Ada"}
	if err := b.seedGitConfig(selfIdentity(t), git); err != nil {
		t.Fatalf("seedGitConfig: %v", err)
	}
	if got := f.configWrites(); got["user.name"] != "Ada" {
		t.Fatalf("config writes = %v, want user.name seeded over an empty value", got)
	}
}

func TestSeedGitConfigWritesNothingWhenUnconfigured(t *testing.T) {
	b, f := newFakeGitBooter(nil)
	if err := b.seedGitConfig(selfIdentity(t), sandboxconfig.GitIdentity{}); err != nil {
		t.Fatalf("seedGitConfig: %v", err)
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %v, want none", f.runs)
	}
}

// Half an identity is still worth seeding: git prompts for whichever half is
// missing rather than for both.
func TestSeedGitConfigSeedsAPartialIdentity(t *testing.T) {
	b, f := newFakeGitBooter(nil)
	if err := b.seedGitConfig(selfIdentity(t), sandboxconfig.GitIdentity{UserEmail: "ada@example.com"}); err != nil {
		t.Fatalf("seedGitConfig: %v", err)
	}
	want := map[string]string{"user.email": "ada@example.com"}
	if got := f.configWrites(); !mapsEqual(got, want) {
		t.Fatalf("config writes = %v, want %v", got, want)
	}
}

// An image without git has nothing to configure, and must not fail to boot over
// it -- the rule ensureAdditionalGroups applies to a group the image never made.
func TestSeedGitConfigSkipsWhenGitIsNotInstalled(t *testing.T) {
	b, f := newFakeGitBooter(nil)
	f.noGit = true
	git := sandboxconfig.GitIdentity{UserName: "Ada", UserEmail: "ada@example.com"}
	if err := b.seedGitConfig(selfIdentity(t), git); err != nil {
		t.Fatalf("seedGitConfig = %v, want nil: a missing git must not fail boot", err)
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %v, want none", f.runs)
	}
}

// End to end against the real git binary: the behavior the fake cannot prove is
// that an existing file survives having identity added to it.
func TestSeedGitConfigPreservesAnExistingFileAgainstRealGit(t *testing.T) {
	// The read is deliberately not scoped to --global, so a system-level
	// identity on the machine running this test would otherwise decide it.
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	id := selfIdentity(t)
	path := filepath.Join(id.home, ".gitconfig")
	existing := "[alias]\n\tco = checkout\n[commit]\n\tgpgsign = true\n"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	git := sandboxconfig.GitIdentity{UserName: "Ada Lovelace", UserEmail: "ada@example.com"}
	if err := newBooter().seedGitConfig(id, git); err != nil {
		t.Fatalf("seedGitConfig: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .gitconfig: %v", err)
	}
	for _, want := range []string{"co = checkout", "gpgsign = true", "Ada Lovelace", "ada@example.com"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("gitconfig = %q, want it to still contain %q", got, want)
		}
	}
}

// The same per-key rule against real git: an address the user set in the
// sandbox survives, and the name they never set gets filled in.
func TestSeedGitConfigFillsOnlyTheMissingKeyAgainstRealGit(t *testing.T) {
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	id := selfIdentity(t)
	path := filepath.Join(id.home, ".gitconfig")
	if err := os.WriteFile(path, []byte("[user]\n\temail = mine@example.com\n"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	git := sandboxconfig.GitIdentity{UserName: "Manifest", UserEmail: "manifest@example.com"}
	if err := newBooter().seedGitConfig(id, git); err != nil {
		t.Fatalf("seedGitConfig: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .gitconfig: %v", err)
	}
	if !strings.Contains(string(got), "mine@example.com") {
		t.Fatalf("gitconfig = %q, want the user's own address kept", got)
	}
	if strings.Contains(string(got), "manifest@example.com") {
		t.Fatalf("gitconfig = %q, want the manifest address not to overwrite it", got)
	}
	if !strings.Contains(string(got), "Manifest") {
		t.Fatalf("gitconfig = %q, want the unset name seeded", got)
	}
}

func TestGitIdentityConfiguredIgnoresWhitespace(t *testing.T) {
	if (sandboxconfig.GitIdentity{UserName: "  ", UserEmail: "\t"}).Configured() {
		t.Fatal("Configured = true for a whitespace-only identity, want false")
	}
	if !(sandboxconfig.GitIdentity{UserEmail: "ada@example.com"}).Configured() {
		t.Fatal("Configured = false with an email set, want true")
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
