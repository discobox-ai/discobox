package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/discobox-ai/discobox/execstream/client"
)

// ExitCode reports the exit status the CLI should exit with silently, which is
// only ever the status of a process the caller attached to. It deliberately
// matches client.ExitError alone: errors from helper subprocesses such as git
// also carry an ExitCode, and exiting on those would replace their message with
// a bare status code.
func ExitCode(err error) (int, bool) {
	var exit client.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), true
	}
	return 0, false
}

func printAttachErrorFrame(stderr io.Writer) func([]byte) error {
	return func(payload []byte) error {
		message := strings.TrimSpace(string(payload))
		if message != "" {
			_, _ = fmt.Fprintln(stderr, message)
		}
		return nil
	}
}

// interruptNotice tells the caller that the Ctrl-C they just pressed never
// reached the remote, and that another one quits.
//
// It is the only thing this client says while an attach is stalled: the stream
// itself stays silent on purpose (a slow discobox is not a broken one), so the
// notice appears exactly when the caller has already concluded something is
// wrong and is trying to get out. raw ends the lines the way a raw terminal
// needs them, since the session is still holding the terminal when this runs.
func interruptNotice(stderr io.Writer, raw bool, what string) func() {
	eol := "\n"
	if raw {
		eol = "\r\n"
	}
	return func() {
		_, _ = fmt.Fprintf(stderr, "%sNot responding: your interrupt has not reached %s.%sPress Ctrl-C again to quit.%s", eol, what, eol, eol)
	}
}

// interruptedExit ends a command whose attach was escaped locally. The notice
// already said what happened, so all that is left to carry is the status a
// shell reports for a command its user interrupted; ExitCode makes it a silent
// exit rather than a printed error.
func interruptedExit() error { return client.ExitError{Code: 130} }
