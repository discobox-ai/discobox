package execs

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// launcherScript is the image's discobox-shell as it sits in the image context,
// relative to this directory. The Dockerfile installs it as shellLauncher.
const launcherScript = "../image/discobox-shell"

// launcherBox is a sandbox as far as the launcher can tell: a login shell to
// fall back to, and a profile that — like direnv loading a repository's flake —
// puts a shell on PATH that was not there before it ran. Every shell here
// prints what it was started with, so a test reads which one ran and in what
// environment.
type launcherBox struct {
	t        *testing.T
	fallback string
	profile  string
	late     string
}

// flakeShell is the name of the shell only the profile makes findable. It is
// unlike any real shell's so the host's PATH cannot supply one.
const flakeShell = "discobox-test-flake-shell"

func newLauncherBox(t *testing.T) *launcherBox {
	t.Helper()
	if runtime.GOOS == "windows" {
		// The launcher runs in the Linux image: it joins PATH with ':' and
		// reads its arguments as POSIX paths, which a Windows host's are not.
		t.Skip("the shell launcher is a Linux image script")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("no bash to run the launcher with: %v", err)
	}
	dir := t.TempDir()
	b := &launcherBox{
		t:        t,
		fallback: filepath.Join(dir, "login-shell"),
		profile:  filepath.Join(dir, "profile"),
		late:     filepath.Join(dir, "late-bin"),
	}
	if err := os.Mkdir(b.late, 0o755); err != nil {
		t.Fatal(err)
	}
	b.writeShell(b.fallback, "login")
	b.writeShell(filepath.Join(b.late, flakeShell), "flake")
	profile := "export PATH=" + b.late + ":$PATH\nexport FROM_PROFILE=yes\n"
	if err := os.WriteFile(b.profile, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *launcherBox) writeShell(path, name string) {
	b.t.Helper()
	script := "#!/bin/sh\necho \"" + name + " $* SHELL=$SHELL FROM_PROFILE=${FROM_PROFILE:-no}\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		b.t.Fatal(err)
	}
}

func (b *launcherBox) run(args ...string) string {
	b.t.Helper()
	//nolint:gosec // The command is the image's own script and arguments this test wrote.
	cmd := exec.CommandContext(b.t.Context(), "bash", append([]string{launcherScript}, args...)...)
	cmd.Env = append(os.Environ(), "DISCOBOX_SHELL_PROFILE="+b.profile, "SHELL=/bin/sh", "FROM_PROFILE=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		b.t.Fatalf("launcher %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The client's $SHELL is a path on the client's machine. Here only its name
// counts, and it is looked up after the profile runs, so a shell the
// repository's dev environment provides is found — and starts in the
// environment that profile built.
func TestLauncherFindsAShellTheProfileProvides(t *testing.T) {
	b := newLauncherBox(t)
	got := b.run("/opt/homebrew/bin/"+flakeShell, b.fallback, "-l")
	want := "flake -l SHELL=" + filepath.Join(b.late, flakeShell) + " FROM_PROFILE=yes"
	if got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestLauncherRunsAnExistingPathAsGiven(t *testing.T) {
	b := newLauncherBox(t)
	own := filepath.Join(t.TempDir(), "own-shell")
	b.writeShell(own, "own")
	if got, want := b.run(own, b.fallback, "-l"), "own -l SHELL="+own+" FROM_PROFILE=yes"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// A shell the sandbox does not have is not an error: the person gets the
// sandbox's own login shell, with the arguments it was going to get anyway --
// and the environment it would have had, not one the lookup loaded. A login
// bash handed that would run the profile again, resetting PATH, and direnv,
// seeing the .envrc already loaded, would not put the repository's back.
func TestLauncherFallsBackToTheLoginShellUntouched(t *testing.T) {
	b := newLauncherBox(t)
	got := b.run("/usr/local/bin/discobox-test-no-such-shell", b.fallback, "-l")
	if want := "login -l SHELL=" + b.fallback + " FROM_PROFILE=no"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestLauncherHandsTheLoginShellOverUntouched(t *testing.T) {
	b := newLauncherBox(t)
	if got, want := b.run(b.fallback, b.fallback, "-l"), "login -l SHELL="+b.fallback+" FROM_PROFILE=no"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// A shell that runs /etc/profile itself is found the same way, but starts from
// the environment it was given, for the reason the fallback does.
func TestLauncherStartsAProfileReadingShellUntouched(t *testing.T) {
	b := newLauncherBox(t)
	bash := filepath.Join(b.late, "bash")
	b.writeShell(bash, "other-bash")
	if got, want := b.run("/opt/homebrew/bin/bash", b.fallback, "-l"), "other-bash -l SHELL="+bash+" FROM_PROFILE=no"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// The exact path wins over the same name on PATH: a brew install at
// /home/linuxbrew/.linuxbrew/bin/nu is the prefix the image's own brew uses, so
// that path can exist here as it is, and it is the one the person meant.
func TestLauncherPrefersTheExactPathToTheNameOnPath(t *testing.T) {
	b := newLauncherBox(t)
	exact := filepath.Join(t.TempDir(), flakeShell)
	b.writeShell(exact, "exact")
	if got, want := b.run(exact, b.fallback, "-l"), "exact -l SHELL="+exact+" FROM_PROFILE=yes"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// The profile and the .envrc are sourced into the launcher, so what they assign
// must not decide what it runs: a flake's dev environment exports `shell` as
// its own bash, and a launcher that read its answer back from that name after
// loading started the flake's bash for a person who asked for zsh.
func TestLauncherIsNotRedirectedByWhatTheProfileAssigns(t *testing.T) {
	b := newLauncherBox(t)
	decoy := filepath.Join(t.TempDir(), "decoy")
	b.writeShell(decoy, "decoy")
	profile := "export PATH=" + b.late + ":$PATH\nexport FROM_PROFILE=yes\n" +
		"export shell=" + decoy + " preferred=" + decoy + " fallback=" + decoy + " profile=/dev/null\n" +
		"set -- " + decoy + "\n"
	if err := os.WriteFile(b.profile, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	got := b.run(flakeShell, b.fallback, "-l")
	want := "flake -l SHELL=" + filepath.Join(b.late, flakeShell) + " FROM_PROFILE=yes"
	if got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
