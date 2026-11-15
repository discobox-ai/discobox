package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	guestRepo  = "ghcr.io/example/discobox-vm"
	kernelRepo = "ghcr.io/example/discobox-vm-kernel"
)

// fakeRegistry maps repository:tag to a digest.
type fakeRegistry map[string]string

func (f fakeRegistry) Digest(_ context.Context, ref string) (string, error) { return f[ref], nil }

func digest(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// fixture is a git repository laid out like this one where it matters.
type fixture struct {
	t   *testing.T
	dir string
	g   git
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir()}
	f.g = git{dir: f.dir}
	f.git("init", "-q", "-b", "main")
	f.write("vm-image/Dockerfile", "FROM debian\n")
	f.write("vm-image/kernel/Dockerfile", "FROM kernel\n")
	f.pin(digest('a'), digest('b'))
	f.commit("initial")
	f.git("tag", "vm/v1")
	f.git("tag", "vm-kernel/v1")
	return f
}

func (f *fixture) git(args ...string) string {
	f.t.Helper()
	//nolint:gosec // G204: git, with the test's own fixed arguments.
	cmd := exec.CommandContext(f.t.Context(), "git", append([]string{"-C", f.dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(path, content string) {
	f.t.Helper()
	full := filepath.Join(f.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// pin writes both pin constants.
func (f *fixture) pin(guest, kernel string) {
	f.write("server/providers/guestimage/image.go",
		"package guestimage\n\n// DefaultVMImage is pinned.\nconst DefaultVMImage = \""+guestRepo+"@"+guest+"\"\n")
	f.write("server/providers/libkrun/provider.go",
		"package libkrun\n\nconst (\n\tother = \"x\"\n\tDefaultKernelImage = \""+kernelRepo+"@"+kernel+"\"\n)\n")
}

func (f *fixture) commit(msg string) {
	f.t.Helper()
	f.git("add", "-A")
	f.git("commit", "-q", "-m", msg)
}

func (f *fixture) check(reg resolver, rev string) report {
	f.t.Helper()
	r, err := check(context.Background(), f.g, reg, rev)
	if err != nil {
		f.t.Fatalf("check: %v", err)
	}
	return r
}

func published() fakeRegistry {
	return fakeRegistry{guestRepo + ":v1": digest('a'), kernelRepo + ":v1": digest('b')}
}

func TestCurrentPinsRelease(t *testing.T) {
	f := newFixture(t)
	f.write("README.md", "outside vm-image\n")
	f.commit("unrelated")
	f.git("tag", "-a", "-m", "release", "v1.0.0")

	r := f.check(published(), "v1.0.0")
	if !r.Releasable() || r.Overridden() || len(r.StaleConsts()) != 0 {
		t.Fatalf("want releasable without override:\n%s", r)
	}
	for _, res := range r.Results {
		if res.Tag == "" || len(res.Changed) != 0 {
			t.Errorf("%s: tag %q changed %v", res.Pin.Const, res.Tag, res.Changed)
		}
	}
	if r.Notes() != "" {
		t.Errorf("notes for a clean release: %q", r.Notes())
	}
}

func TestStaleGuestRefuses(t *testing.T) {
	f := newFixture(t)
	f.write("vm-image/Dockerfile", "FROM debian:sid\n")
	f.commit("guest change")
	f.git("tag", "v1.0.0")

	r := f.check(published(), "v1.0.0")
	if r.Releasable() {
		t.Fatalf("want refused:\n%s", r)
	}
	if got := r.StaleConsts(); !slices.Equal(got, []string{"DefaultVMImage"}) {
		t.Fatalf("stale = %v, want only the guest", got)
	}
	if got := r.Results[0].Changed; !slices.Equal(got, []string{"vm-image/Dockerfile"}) {
		t.Errorf("changed = %v", got)
	}
	if out := r.String(); !strings.Contains(out, "DefaultVMImage") || !strings.Contains(out, "vm/v1") ||
		!strings.Contains(out, "vm-image/Dockerfile") {
		t.Errorf("report does not name the pin, its tag and the path:\n%s", out)
	}
}

func TestStaleKernelRefusesWithoutStalingGuest(t *testing.T) {
	f := newFixture(t)
	f.write("vm-image/kernel/Dockerfile", "FROM kernel:7\n")
	f.commit("kernel change")
	f.git("tag", "v1.0.0")

	r := f.check(published(), "v1.0.0")
	if r.Releasable() {
		t.Fatalf("want refused:\n%s", r)
	}
	if got := r.StaleConsts(); !slices.Equal(got, []string{"DefaultKernelImage"}) {
		t.Fatalf("stale = %v, want only the kernel (a kernel/ change must not stale the guest)", got)
	}
	if got := r.Results[1].Changed; !slices.Equal(got, []string{"vm-image/kernel/Dockerfile"}) {
		t.Errorf("changed = %v", got)
	}
}

func TestRepinnedGuestIsCurrent(t *testing.T) {
	f := newFixture(t)
	f.write("vm-image/Dockerfile", "FROM debian:sid\n")
	f.commit("guest change")
	f.git("tag", "vm/v2")
	f.pin(digest('c'), digest('b'))
	f.commit("pin vm/v2")
	f.git("tag", "v1.0.0")

	reg := published()
	reg[guestRepo+":v2"] = digest('c')
	r := f.check(reg, "v1.0.0")
	if !r.Releasable() || len(r.StaleConsts()) != 0 {
		t.Fatalf("want current:\n%s", r)
	}
	if r.Results[0].Tag != "vm/v2" {
		t.Errorf("guest tag = %q, want vm/v2", r.Results[0].Tag)
	}
}

func TestUnmatchedDigestRefuses(t *testing.T) {
	f := newFixture(t)
	f.pin(digest('f'), digest('b'))
	f.commit("pin an unpublished guest")
	f.git("tag", "v1.0.0")

	r := f.check(published(), "v1.0.0")
	if r.Releasable() {
		t.Fatalf("want refused:\n%s", r)
	}
	if r.Results[0].Tag != "" || !strings.Contains(r.String(), "no vm/v* tag") {
		t.Errorf("want the guest pin reported as matching no tag:\n%s", r)
	}
}

func TestOverride(t *testing.T) {
	f := newFixture(t)
	f.write("vm-image/kernel/Dockerfile", "# a comment\nFROM kernel\n")
	f.commit("kernel comment")
	f.git("tag", "-a", "-m", "v1.0.0\n\n"+overrideTrailer+": kernel change is comments only", "v1.0.0")

	r := f.check(published(), "v1.0.0")
	if !r.Releasable() || !r.Overridden() {
		t.Fatalf("want released by override:\n%s", r)
	}
	if !slices.Equal(r.Overrides, []string{"kernel change is comments only"}) {
		t.Errorf("overrides = %q", r.Overrides)
	}
	notes := r.Notes()
	for _, want := range []string{"kernel change is comments only", "DefaultKernelImage", "vm-kernel/v1"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
}

// A commit message is whoever wrote the pull request's; only the tag, which
// the owner cuts, may override.
func TestCommitTrailerDoesNotOverride(t *testing.T) {
	f := newFixture(t)
	f.write("vm-image/Dockerfile", "FROM debian:sid\n")
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "guest change\n\n"+overrideTrailer+": trust me")
	f.git("tag", "-a", "-m", "release", "v1.0.0")
	f.git("tag", "v1.0.1")
	for _, rev := range []string{"v1.0.0", "v1.0.1", "HEAD"} {
		if r := f.check(published(), rev); r.Releasable() || len(r.Overrides) != 0 {
			t.Errorf("%s: a commit trailer released a stale pin:\n%s", rev, r)
		}
	}
}

func TestOverrideElsewhereDoesNotCount(t *testing.T) {
	f := newFixture(t)
	f.git("tag", "-a", "-m", "v0.9.0\n\n"+overrideTrailer+": an old reason", "v0.9.0")
	f.write("vm-image/Dockerfile", "FROM debian:sid\n")
	f.commit("guest change")
	f.git("tag", "-a", "-m", "release", "v1.0.0")

	if r := f.check(published(), "v1.0.0"); r.Releasable() {
		t.Fatalf("an earlier tag's trailer must not release this one:\n%s", r)
	}
}

func TestGuestInputsOutsideVMImage(t *testing.T) {
	for _, path := range []string{"pkg/vsock/conn.go", "pool-agent/cmd/discobox-vsock-guest/main.go"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t)
			f.write(path, "package x\n")
			f.commit("vsock change")
			r := f.check(published(), "HEAD")
			if got := r.StaleConsts(); !slices.Equal(got, []string{"DefaultVMImage"}) {
				t.Fatalf("stale = %v, want the guest: %s is compiled into it", got, path)
			}
		})
	}
}

func TestDocsAndTestsDoNotStale(t *testing.T) {
	f := newFixture(t)
	f.write("vm-image/DESIGN.md", "notes\n")
	f.write("vm-image/kernel/REVIEW.md", "notes\n")
	f.write("pkg/vsock/conn_test.go", "package vsock\n")
	f.commit("docs and tests")
	if r := f.check(published(), "HEAD"); len(r.StaleConsts()) != 0 {
		t.Fatalf("docs and tests never reach an image:\n%s", r)
	}
}

func TestRunRecordsOverride(t *testing.T) {
	f := newFixture(t)
	f.write("vm-image/Dockerfile", "FROM debian:sid\n")
	f.commit("guest change")
	f.git("tag", "-a", "-m", "release", "v1.0.0")
	f.git("tag", "-a", "-m", "release\n\n"+overrideTrailer+": guest published separately", "v1.0.1")

	dir := t.TempDir()
	summary := filepath.Join(dir, "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summary)
	ctx := context.Background()

	refusedNotes := filepath.Join(dir, "refused.md")
	err := run(ctx, []string{"-repo", f.dir, "-rev", "v1.0.0", "-notes", refusedNotes}, published())
	if err == nil || !strings.Contains(err.Error(), "DefaultVMImage") {
		t.Fatalf("run = %v, want a refusal naming DefaultVMImage", err)
	}
	if data, _ := os.ReadFile(refusedNotes); len(data) != 0 {
		t.Errorf("notes for a refused release: %q", data)
	}

	notes := filepath.Join(dir, "notes.md")
	if err := run(ctx, []string{"-repo", f.dir, "-rev", "v1.0.1", "-notes", notes}, published()); err != nil {
		t.Fatalf("run with override: %v", err)
	}
	data, _ := os.ReadFile(notes)
	if !strings.Contains(string(data), "guest published separately") {
		t.Errorf("notes do not record the override: %q", data)
	}
	data, _ = os.ReadFile(summary)
	if !strings.Contains(string(data), "guest published separately") || !strings.Contains(string(data), "vm-image/Dockerfile") {
		t.Errorf("job summary does not record the override and the paths: %q", data)
	}
}

func TestStringConst(t *testing.T) {
	src := "package p\n\nconst (\n\ta = 1\n\tB, C = \"b\", \"c\"\n)\n"
	if got, err := stringConst(src, "C"); err != nil || got != "c" {
		t.Errorf("C = %q, %v", got, err)
	}
	if _, err := stringConst(src, "a"); err == nil {
		t.Error("a non-string constant must be an error")
	}
	if _, err := stringConst(src, "D"); err == nil {
		t.Error("a missing constant must be an error")
	}
}
