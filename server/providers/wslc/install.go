package wslc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// wslcCommand is the WSL Containers component's own CLI.
//
// The driver never runs it. It talks to the COM session manager directly
// (internal/wslcsession), and what a pool actually needs is that class being
// registered — which nothing can ask about without trying to activate it, and
// which fails with a bare REGDB_E_CLASSNOTREG when it is not. The CLI is
// installed by the same WSL package that registers the class, so its presence
// is the honest, checkable stand-in, and it is also what a user can run
// themselves to confirm the same thing.
const wslcCommand = "wslc"

// WSLCCommandEnv overrides the program EnsureInstalled looks for, primarily so
// the refusal can be exercised on a Windows host that has WSL Containers:
// naming something that does not exist (`DISCOBOX_WSLC_COMMAND=wslc-missing`)
// is the only way to see what a host without it sees, short of uninstalling
// WSL. It equally names a wslc this process would not otherwise find, since it
// accepts a full path.
//
// It has to be set for the *server* process. The CLI hands its own environment
// to a server it autolaunches, so exporting it in the shell is enough — but
// only if there is no server running already, which is what would be reused
// instead (`discobox admin server shutdown` first).
const WSLCCommandEnv = "DISCOBOX_WSLC_COMMAND"

// wslcInstallHint is the one command that fixes a Windows host without WSL
// Containers. The `--pre-release` matters: wslc ships only in that channel
// today, so a plain `wsl --update` reports the machine is already up to date
// and leaves it exactly as it was.
const wslcInstallHint = "install it by running `wsl --update --pre-release` in a Windows terminal, then start discobox again"

// wslcVersionTimeout bounds the version call. It answers immediately when WSL
// is healthy; when it is not, this is the difference between an error and a
// server that never finishes starting.
const wslcVersionTimeout = 30 * time.Second

// EnsureInstalled reports whether this host has the WSL Containers component
// that Windows pools run on.
//
// It is the Windows server's startup precondition. wslc is the platform default
// provider there (`sandbox.PlatformDefaultProvider`), so a host without it has
// no pool backend at all: every sandbox create would reach COM activation and
// fail with an HRESULT that names neither the missing component nor the command
// that installs it, one sandbox at a time, forever. Refusing to start says it
// once, at the only moment anyone can act on it.
//
// Presence is the whole contract, and the only thing that refuses a host. What
// a pool actually needs is a registered COM class; this program is a stand-in
// for it, and a stand-in must not invent failures the real thing would not have
// — so a wslc that is found but exits non-zero is reported and startup
// continues. Not finding it at all is the one condition strong enough to refuse
// on, because then there is nothing to stand in for anything.
func EnsureInstalled(ctx context.Context) error {
	program, overridden := wslcProgram()
	path, searched, err := lookWSLC(program, overridden)
	if err != nil {
		return fmt.Errorf("WSL Containers (%s) is not installed: discobox runs every Windows pool "+
			"in a WSL Containers VM, so %s. Searched %s %s: %w",
			program, wslcInstallHint, searched, escapeHatch(overridden), err)
	}
	reportVersion(ctx, path)
	return nil
}

// reportVersion puts the installed version in the log, because support
// questions about a Windows host start with which build of WSL it is running.
//
// Diagnostic, never a gate. No minimum is enforced — a version this driver
// cannot work with is not something a number can be trusted to predict — and a
// program that will not run is logged rather than refused, for the reason in
// EnsureInstalled. A canceled context is not reported at all: the server is
// being shut down, which is nobody's WSL problem.
func reportVersion(ctx context.Context, path string) {
	versionCtx, cancel := context.WithTimeout(ctx, wslcVersionTimeout)
	defer cancel()
	output, err := exec.CommandContext(versionCtx, path, "--version").CombinedOutput()
	switch {
	case ctx.Err() != nil:
		return
	case err != nil:
		slog.WarnContext(ctx, "WSL Containers is installed but would not report its version",
			"path", path, "error", err, "output", strings.TrimSpace(string(output)))
	default:
		slog.InfoContext(ctx, "WSL Containers is available", "version", firstLine(output), "path", path)
	}
}

// wslcProgram is the program to look for, and whether that was somebody's
// choice rather than the default. What is overridden is reported because an
// override that is forgotten explains an otherwise impossible refusal.
func wslcProgram() (string, bool) {
	if name := strings.TrimSpace(os.Getenv(WSLCCommandEnv)); name != "" {
		return name, true
	}
	return wslcCommand, false
}

// lookWSLC finds wslc, on PATH or where the WSL package puts it.
//
// PATH is the answer for anyone who can run `wslc --version` themselves, and it
// is what the server inherits from the shell that autolaunched it. The install
// directory is checked after it because this is a proxy for a registered COM
// class, not for a program anything here runs: a wslc that exists but is not on
// this process's PATH is a working host, and refusing to start on it would be
// the check inventing a problem. Getting the directory wrong costs nothing —
// PATH has already failed by then, and the error is the same either way.
//
// An overridden program skips that fallback entirely: someone who named the
// program named the whole answer, and quietly finding the real wslc instead is
// exactly what would make the override useless for testing the refusal.
//
// It also reports where it looked, because a refusal that names only the
// program leaves the one uncertain case — a wslc installed somewhere neither of
// these covers — with nothing to act on but a command that will report the
// machine is already up to date.
func lookWSLC(program string, overridden bool) (path, searched string, err error) {
	if path, err = exec.LookPath(program); err == nil {
		return path, "", nil
	}
	places := []string{"PATH"}
	if overridden {
		return "", strings.Join(places, " and "), err
	}
	for _, programFiles := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramW6432")} {
		if programFiles == "" {
			continue
		}
		directory := filepath.Join(programFiles, "WSL")
		if slices.Contains(places, directory) {
			continue
		}
		places = append(places, directory)
		candidate := filepath.Join(directory, wslcCommand+".exe")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, "", nil
		}
	}
	return "", strings.Join(places, " and "), err
}

// escapeHatch is the rest of what a refused host can do about it. Which half it
// needs depends on how it got here: a host told what to look for is told that
// is why, and one that was not is told it can say where wslc lives — the answer
// for the case this check is least sure of, an install in a third location.
func escapeHatch(overridden bool) string {
	if overridden {
		return "(this is the program named by " + WSLCCommandEnv + ")"
	}
	return "(if it is installed somewhere else, name it in " + WSLCCommandEnv + ")"
}

// firstLine is the version line `wslc --version` answers with, defended against
// a build that answers with more than one or with nothing at all.
func firstLine(output []byte) string {
	for line := range strings.Lines(string(output)) {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return "unknown"
}
