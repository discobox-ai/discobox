package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// defaultPager is what git falls back to, and so what a user's muscle memory
// expects to be driving.
const defaultPager = "less"

// pagerEnvDefaults are the settings git installs for less when the user has not
// chosen their own, and they are not cosmetic:
//
//	R  pass ANSI color through instead of printing escape codes literally
//	F  exit immediately when the output fits on one screen, so a two-line diff
//	   does not have to be dismissed
//	X  do not send the terminal's init/deinit sequences, so the diff stays on
//	   screen after the pager exits
//
// Without R in particular, a rendered diff arrives as unreadable escape noise.
var pagerEnvDefaults = map[string]string{"LESS": "FRX", "LV": "-c"}

// startPager routes out through the user's pager, returning the writer to use
// and a function that closes it and waits for the pager to exit.
//
// It pages only when there is a terminal to page to; anything redirected,
// piped, or captured in a test is left exactly as it was. A pager of "cat" —
// the conventional way to say "do not page" through the environment — is
// honored by not starting one at all.
func startPager(ctx context.Context, out io.Writer, enabled bool) (io.Writer, func() error) {
	noop := func() error { return nil }
	if !enabled || !isTerminalStream(out) {
		return out, noop
	}
	command := pagerCommand(os.Getenv("DISCOBOX_PAGER"), os.Getenv("GIT_PAGER"), os.Getenv("PAGER"))
	if command == "" {
		return out, noop
	}

	// Through a shell, so PAGER="less -R" and friends keep working.
	//nolint:gosec // the command is the user's own PAGER, run the way git runs it
	pager := exec.CommandContext(ctx, "sh", "-c", command)
	pager.Stdout, pager.Stderr = out, os.Stderr
	pager.Env = pagerEnv(os.Environ())
	input, err := pager.StdinPipe()
	if err != nil {
		return out, noop
	}
	if err := pager.Start(); err != nil {
		return out, noop
	}
	return input, func() error {
		if err := input.Close(); err != nil && !isBrokenPipe(err) {
			return err
		}
		// A pager the user quit early takes its input pipe down with it, which
		// is a normal way to stop reading a long diff rather than a failure.
		if err := pager.Wait(); err != nil && !isBrokenPipe(err) {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return nil
			}
			return err
		}
		return nil
	}
}

// pagerCommand picks the pager, and reports empty when the answer is "none".
//
// DISCOBOX_PAGER comes first so this tool can be pointed somewhere else without
// disturbing git, then git's own GIT_PAGER and the conventional PAGER, then
// less. The order matches the rest of the CLI's DISCOBOX_-prefixed environment.
func pagerCommand(discoboxPager, gitPager, pager string) string {
	for _, candidate := range []string{discoboxPager, gitPager, pager, defaultPager} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if candidate == "cat" {
			return ""
		}
		return candidate
	}
	return ""
}

// pagerEnv adds the pager settings git supplies, without overriding a choice
// the user has already made.
func pagerEnv(environ []string) []string {
	for name, value := range pagerEnvDefaults {
		if _, ok := os.LookupEnv(name); ok {
			continue
		}
		environ = append(environ, name+"="+value)
	}
	return environ
}

// isBrokenPipe reports whether an error is the pager having stopped reading,
// which happens every time someone quits less before the end of a long diff.
func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed)
}
