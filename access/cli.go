// Package access implements the in-sandbox credential CLI: a thin client of
// the agent credentials protocol.
//
// Its only dependency is the protocol package, so it can be lifted into another
// repository without bringing a runtime with it.
//
// # Interface shape
//
// The primary consumer is an LLM agent, and the three operations do not have the
// same shape, so they do not get the same interface:
//
//   - "run" wraps a command, so it takes argv. The security property is that
//     the declared command *is* the argv executed; encoding it as JSON would
//     put a translation step between what the model wrote and what runs, and
//     lose the child's exit code as the wrapper's own.
//   - "request" carries nested, free-text fields — a justification and a list
//     of use descriptions — through a shell that treats quotes and apostrophes
//     as syntax. That is the payload JSON is for, so --json reads it from stdin.
//   - "list" takes nothing, where flags are already precise.
//
// There is no "get" that hands back a bare value: every use this CLI supports
// goes through "run", so it is always the judge's argv, never a value with no
// command attached to it. See judge.go and DESIGN.md's "What it must never do"
// for why. The protocol underneath still has a use call that mints one — a
// scripted caller that cannot exec through this CLI needs it — but that is a
// gap in what can be secured today, not a supported way to reach a credential.
//
// --json means "talk to me in JSON" for whichever direction the command has:
// structured output everywhere, and a structured body on stdin for "request".
//
// # The judge
//
// "run" does not execute a command until a model has agreed the command is the
// use a human approved it for (ADR 0079). The model is reached through
// discobox-prompt, which the harness image provides, and every failure of that
// gate — no wrapper, no answer, an unreadable answer, or a refusal — stops the
// command. See judge.go for what the gate is and is not.
package access

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/discobox-ai/discobox/agentcreds"
)

// Name is how the CLI refers to itself in help and diagnostics.
const Name = "discobox-access"

// Exit statuses. A wrapped command's own status passes through "run"
// unchanged, exactly as env(1) and timeout(1) do, so these are what the CLI
// itself reports when it never got as far as running anything.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Run executes the CLI and returns a process exit status.
func Run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitUsage
	}
	command, rest := args[0], args[1:]

	// Signals reach the wrapped child through the same context, so Ctrl-C on a
	// `run` interrupts the command rather than orphaning it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "list":
		return runList(ctx, rest)
	case "request":
		return runRequest(ctx, rest)
	case "run":
		return runWrapped(ctx, rest)
	case "help", "-h", "--help":
		usage(os.Stdout)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "%s: unknown command %q\n", Name, command)
		usage(os.Stderr)
		return exitUsage
	}
}

// newClient builds the protocol client from the environment. The CLI knows
// nothing else about where it is running.
func newClient() *agentcreds.Client {
	base := strings.TrimSpace(os.Getenv(agentcreds.URLEnv))
	if base == "" {
		base = agentcreds.DefaultBaseURL
	}
	return agentcreds.NewClient(base, agentcreds.WithToken(os.Getenv(agentcreds.TokenEnv)))
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `%[1]s — ask for and use credentials you were not provisioned with.

  %[1]s list [--json]
      Show the credentials you may use and what each was approved for.

  %[1]s run --use USE_ID -- COMMAND [ARGS ...]
      Run COMMAND with the credential in its environment, and exit with
      COMMAND's own status. Prefer this: the command it runs is the command it
      declares, and the value never leaves that one child process.

      COMMAND is judged against the use it was approved for before it runs, by
      a model reached through %[5]s. A command broader than the
      approved use is refused with code "denied" and never started.

  %[1]s request [--json] [flags]
      Ask a human for a credential. Returns a request id immediately unless
      you wait for an answer.

      With --json, the request is read from stdin, which keeps quotes and
      apostrophes in your justification out of the shell's hands:

        %[1]s request --json <<'EOF'
        {
          "name": "github",
          "envVar": "GITHUB_TOKEN",
          "host": "api.github.com",
          "justification": "the user's task asks me to open a PR",
          "uses": [{"description": "Open a PR against the current repo"}],
          "wait": true,
          "timeoutSeconds": 3600
        }
        EOF

      With flags: --name, --env-var, --host, --why, --use (repeatable),
      --wait, --timeout.

There is no command that prints a credential's value on its own. "run" is the
only way to use one — the value goes straight into the child it names and
nowhere else, which is what lets a model judge the command before it runs.

--json also makes failures structured, on stderr:

    {"error":{"code":"denied","message":"..."}}

  invalid      the call was malformed; fix it and retry
  denied       you may not use this; ask for it with request
  not_found    the id means nothing here
  unavailable  the service could not answer; retry

The value you receive is opaque and short-lived. Do not log it, write it to a
file, or reuse it after it expires — ask for it again instead.

Configured by %[2]s (default %[3]s) and %[4]s. The judge runs
%[5]s, which every harness image provides; %[6]s names a
different one.
`, Name, agentcreds.URLEnv, agentcreds.DefaultBaseURL, agentcreds.TokenEnv, DefaultPromptCommand, PromptCommandEnv)
}

// usageError reports a mistake in how the command was invoked, which is
// distinct from the service refusing it.
func usageError(out *emitter, format string, args ...any) int {
	out.fail(agentcreds.CodeInvalid, fmt.Sprintf(format, args...))
	return exitUsage
}

// parse runs a flag set, reporting a parse failure as a usage error. flag
// already printed its own message, so this only settles the exit status.
func parse(flags *flag.FlagSet, args []string) bool {
	flags.SetOutput(os.Stderr)
	return flags.Parse(args) == nil
}
