package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/obot-platform/discobox/execstream/client"
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
