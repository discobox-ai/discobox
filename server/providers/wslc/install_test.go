package wslc

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// A Windows host without WSL Containers cannot run a single pool, and the only
// thing that changes that is one command in one channel. An error that says
// wslc is missing without saying `wsl --update --pre-release` sends the user to
// `wsl --update`, which reports the machine is already up to date.
//
// Where it looked and how to name an install it did not find are part of that:
// the install directory is a guess, and a host that keeps wslc somewhere else
// is refused with nothing to act on unless the error says so.
func TestEnsureInstalledSaysHowToInstallWSLContainers(t *testing.T) {
	t.Setenv(WSLCCommandEnv, "")
	t.Setenv("PATH", t.TempDir())
	// The install directory is searched too, so an empty one is what "not
	// installed" means here rather than an unset variable this host never had.
	programFiles := t.TempDir()
	t.Setenv("ProgramFiles", programFiles)
	t.Setenv("ProgramW6432", programFiles)

	err := EnsureInstalled(t.Context())
	if err == nil {
		t.Fatal("EnsureInstalled = nil with no wslc anywhere, want a refusal")
	}
	for _, want := range []string{
		wslcCommand,
		"wsl --update --pre-release",
		"PATH",
		filepath.Join(programFiles, "WSL"),
		WSLCCommandEnv,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// The check is presence, not a version comparison: any wslc that runs is one
// this server starts on. Pinning a minimum here would refuse hosts on the
// strength of a number rather than on anything that failed.
func TestEnsureInstalledAcceptsAnyWorkingWSLC(t *testing.T) {
	t.Setenv(WSLCCommandEnv, "")
	t.Setenv("PATH", dirWithFakeWSLC(t, 0, "wslc 2.9.4.0"))

	if err := EnsureInstalled(t.Context()); err != nil {
		t.Fatalf("EnsureInstalled = %v, want nil when wslc answers", err)
	}
}

// wslc is found where the WSL package installs it, not only on PATH. The check
// stands in for a registered COM class, which the driver reaches through COM
// and never through PATH — so a host with wslc installed and a PATH that does
// not name it is a working host, and refusing to start on it would be this
// check inventing a problem.
//
// This runs everywhere rather than on Windows alone. The install directory is
// the one guess in the whole check, and a test of it that only ever runs in CI
// is a test nobody working on this has seen pass.
func TestEnsureInstalledFindsWSLCOffPath(t *testing.T) {
	programFiles := t.TempDir()
	installed := filepath.Join(programFiles, "WSL")
	if err := os.MkdirAll(installed, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFakeWSLC(t, filepath.Join(installed, wslcCommand+".exe"), 0, "wslc 2.9.4.0")
	t.Setenv(WSLCCommandEnv, "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ProgramFiles", programFiles)
	t.Setenv("ProgramW6432", programFiles)

	if err := EnsureInstalled(t.Context()); err != nil {
		t.Fatalf("EnsureInstalled = %v, want wslc to be found where the WSL package installs it", err)
	}
}

// An installed wslc that will not run is still an installed wslc. What a pool
// needs is the COM class this program stands in for, and the class is
// registered by the package that put the program there — so refusing here would
// refuse a host whose pools would have run, which is the one failure a proxy
// check must not invent. It is logged instead.
func TestEnsureInstalledDoesNotRefuseAWSLCThatWillNotRun(t *testing.T) {
	t.Setenv(WSLCCommandEnv, "")
	t.Setenv("PATH", dirWithFakeWSLC(t, 1, "WSL is not installed"))

	if err := EnsureInstalled(t.Context()); err != nil {
		t.Fatalf("EnsureInstalled = %v, want a wslc that exits non-zero to be reported, not refused", err)
	}
}

// The refusal has to be reachable on a host that does have WSL Containers,
// because a check nobody can exercise is a check nobody has seen work. Naming a
// program that does not exist is what produces the real error — the same text a
// host without wslc gets, plus the note that the name was chosen here, so a
// variable left set months later explains an otherwise impossible refusal.
func TestEnsureInstalledLooksForTheOverriddenProgram(t *testing.T) {
	// A working wslc is on PATH, exactly as it is on the host this override
	// exists for. Finding it anyway would make the override useless.
	t.Setenv("PATH", dirWithFakeWSLC(t, 0, "wslc 2.9.4.0"))
	t.Setenv(WSLCCommandEnv, "wslc-missing")

	err := EnsureInstalled(t.Context())
	if err == nil {
		t.Fatal("EnsureInstalled = nil, want the overridden program to be the one that has to exist")
	}
	for _, want := range []string{"wslc-missing", "wsl --update --pre-release", WSLCCommandEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// The other direction: an override names a wslc to accept, full path included,
// for a host where it is installed somewhere this check would not look.
func TestEnsureInstalledAcceptsAnOverriddenProgram(t *testing.T) {
	installed := filepath.Join(dirWithFakeWSLC(t, 0, "wslc 2.9.4.0"), fakeWSLCName())
	t.Setenv("PATH", t.TempDir())
	t.Setenv(WSLCCommandEnv, installed)

	if err := EnsureInstalled(t.Context()); err != nil {
		t.Fatalf("EnsureInstalled = %v, want the overridden program to be accepted", err)
	}
}

// dirWithFakeWSLC returns a directory holding a wslc that prints message and
// exits with code, named so that this host can run it off PATH.
func dirWithFakeWSLC(t *testing.T, code int, message string) string {
	t.Helper()
	dir := t.TempDir()
	writeFakeWSLC(t, filepath.Join(dir, fakeWSLCName()), code, message)
	return dir
}

// writeFakeWSLC writes a stand-in wslc at path. It is a batch file on Windows
// and a shell script elsewhere, because what the check does with what it finds
// is run it, and that is the platform's own answer either way.
//
// A path ending in .exe is the exception: Windows will not run a batch file
// under that name, which is the case the version call is deliberately not a
// gate for — the program is found, reported as unrunnable, and startup goes on.
func writeFakeWSLC(t *testing.T, path string, code int, message string) {
	t.Helper()
	script := "#!/bin/sh\necho '" + message + "'\nexit " + strconv.Itoa(code) + "\n"
	if runtime.GOOS == "windows" {
		script = "@echo off\r\necho " + message + "\r\nexit /b " + strconv.Itoa(code) + "\r\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// fakeWSLCName is what a stand-in wslc has to be called to be found on PATH and
// runnable: a batch file on Windows, since a script cannot be named .exe and
// still run there.
func fakeWSLCName() string {
	if runtime.GOOS == "windows" {
		return wslcCommand + ".bat"
	}
	return wslcCommand
}
