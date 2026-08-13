package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	idpkg "github.com/obot-platform/discobox/id"
)

// newShellCommand implements `disco shell`: the everyday counterpart to the
// raw, fully configurable `disco box exec create`, matching `disco exec`'s
// old behavior but with the sandbox and command sharing one positional list
// instead of a --sandbox-id flag. Cobra alone cannot tell SANDBOX_ID and CMD
// apart, so RunE resolves it: see resolveShellTarget.
func (a *App) newShellCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shell [SANDBOX_ID] [CMD...]",
		Short: "Run a command in a sandbox, or its login shell",
		Long: `Run a command in a sandbox and stream it to this terminal, or open its login
shell.

When the first argument names one of the sandboxes "disco ls" shows for the
current project directory — its ID, a short prefix of it, or a name unique
among them — that sandbox is used and every argument after it is the command.
Otherwise there is no SANDBOX_ID: every argument is the command, and the
sandbox is taken from "disco ls" instead — the only one when there is one,
otherwise you are asked to pick.

Without a command this starts the sandbox user's login shell. Which shell that
is is resolved inside the sandbox from that user's passwd entry, so it is the
sandbox's shell, not this machine's.

Stdin is always attached, and a PTY is allocated only when this terminal is one,
so piping and redirecting behave like a local command. Signals are forwarded to
the remote process, and shell exits with its exit code.`,
		Example: `  disco shell
  disco shell go test ./...
  disco shell sbx_01hq bash
  disco shell sbx_01hq -- ls -la`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, _, cmdArgs, err := a.resolveShellTarget(cmd, args)
			if err != nil {
				return err
			}
			// A PTY is only correct when every stream this exec touches is one:
			// allocating one for a pipe would echo input back and dress output in
			// escape sequences the consumer never asked for.
			tty := isTerminalStream(cmd.InOrStdin()) && isTerminalStream(cmd.OutOrStdout()) && isTerminalStream(cmd.ErrOrStderr())
			// No command means the sandbox user's shell. Only the sandbox can say
			// which shell that is, so the request asks for one rather than naming it.
			body, err := createSandboxExecBody(sandboxExecCreateOptions{interactive: true, tty: tty, shell: len(cmdArgs) == 0}, cmdArgs)
			if err != nil {
				return err
			}
			exec, err := a.createSandboxExec(cmd.Context(), projectID, sandboxID, body)
			if err != nil {
				return err
			}
			if err := a.attachSandboxExec(cmd.Context(), projectID, sandboxID, exec.ID, true, tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}
			return a.returnSandboxExecStatus(cmd.Context(), projectID, sandboxID, exec.ID)
		},
	}
	// Stop parsing flags at the first positional argument so the command's own
	// flags belong to it: `disco shell sbx_01hq sh -c ...` passes -c through
	// instead of failing on an unknown shorthand.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// resolveShellTarget splits shell's positional arguments into the sandbox and
// the command, since Cobra sees one flat list and cannot tell them apart on
// its own. args[0] is tried against the sandboxes "disco ls" shows for the
// current project directory (matchSandboxArg); a match consumes it as
// SANDBOX_ID and leaves the rest as the command. No match — including no
// args at all — means every argument is the command, and the sandbox falls
// back to the same picker `disco apply` uses when SANDBOX_ID is omitted.
func (a *App) resolveShellTarget(cmd *cobra.Command, args []string) (projectID, sandboxID string, client *apiclientgen.Client, cmdArgs []string, err error) {
	projectID, err = a.projectIDValue()
	if err != nil {
		return "", "", nil, nil, err
	}
	client, err = a.apiClient()
	if err != nil {
		return "", "", nil, nil, err
	}
	// A full generated ID needs no listing to recognize: its shape alone is
	// unambiguous, and the server is the one that validates it exists — the same
	// no-round-trip-it-doesn't-need path a fully-specified --sandbox-id takes
	// elsewhere.
	if len(args) > 0 && idpkg.IsGenerated(args[0]) {
		return projectID, args[0], client, args[1:], nil
	}
	sandboxes, err := a.listProjectSandboxes(cmd.Context(), client, projectID, false)
	if err != nil {
		return "", "", nil, nil, err
	}
	if len(args) > 0 {
		id, ok, matchErr := matchSandboxArg(args[0], sandboxes)
		if matchErr != nil {
			return "", "", nil, nil, matchErr
		}
		if ok {
			return projectID, id, client, args[1:], nil
		}
	}
	sandboxID, err = pickOne(cmd, "Select a sandbox", sandboxPickerItems(sandboxes), pickerOptions{
		empty:     "no sandboxes were started from this directory; start one with `disco run`, or name the sandbox as the first argument",
		ambiguous: "more than one sandbox was started from this directory; name the sandbox as the first argument",
		recentKey: "sandbox:" + projectID,
	})
	return projectID, sandboxID, client, args, err
}

// matchSandboxArg reports whether arg names one of sandboxes, the same
// candidates "disco ls" shows for the current project directory: a full
// generated ID, an exact sandbox name, or a short ID. A full
// generated ID (id.IsGenerated) is trusted outright: its shape — a resource
// prefix plus 16 random characters — is unique enough that no shell command
// word could collide with it by accident. A short ID is matched against these
// candidates exactly like a bare SANDBOX_ID argument would be elsewhere;
// matching none of them is not an error; it just means arg is not a sandbox
// reference, so the caller treats it as the start of a command instead.
// Matching several is reported as ambiguous, the same as any other short-ID
// collision in the CLI, since arg's shape said it was meant as an ID.
func matchSandboxArg(arg string, sandboxes []apimodel.Sandbox) (id string, ok bool, err error) {
	if idpkg.IsGenerated(arg) {
		return arg, true, nil
	}
	// An exact name, before any ID matching: a name is what the listing shows
	// and what people type, and matching it in full leaves no room for the
	// guessing a prefix invites. Names are unique within a project
	// (idx_sandbox_project_name), so a duplicate here would mean the candidate
	// list spans projects; it is reported rather than picked between.
	switch named := sandboxesNamed(arg, sandboxes); len(named) {
	case 0:
	case 1:
		return named[0], true, nil
	default:
		return "", false, fmt.Errorf("%q names more than one sandbox from this directory (%s); use the sandbox ID", arg, strings.Join(named, ", "))
	}
	if !isResolvableShortID(arg) {
		return "", false, nil
	}
	ids := make([]string, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		ids = append(ids, sandbox.ID)
	}
	switch matches := idpkg.ResolveShort(arg, ids); len(matches) {
	case 0:
		return "", false, nil
	case 1:
		return matches[0], true, nil
	default:
		return "", false, fmt.Errorf("%q matches more than one sandbox from this directory (%s); use a longer prefix", arg, strings.Join(matches, ", "))
	}
}

// sandboxesNamed returns the IDs of the sandboxes whose name is exactly arg.
// The match is the whole name and nothing less: a partial name would compete
// with short-ID matching for the same argument, and "did you mean a name or an
// ID" is not a question a command word should have to answer.
func sandboxesNamed(arg string, sandboxes []apimodel.Sandbox) []string {
	var ids []string
	for _, sandbox := range sandboxes {
		if sandbox.Config.Name == arg {
			ids = append(ids, sandbox.ID)
		}
	}
	return ids
}
