package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	idpkg "github.com/discobox-ai/x/id"
)

// newShellCommand implements `discobox shell`: the everyday counterpart to the
// raw, fully configurable `discobox admin exec create`. It takes the discobox
// and the command as `[DISCOBOX] [-- CMD...]` (selectDiscoboxCommand): the
// argument before the -- is the discobox, and the command is everything after
// it.
func (a *App) newShellCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shell [DISCOBOX] [-- CMD...]",
		Short: "Run a command in a discobox, or its login shell",
		Long: `Run a command in a discobox and stream it to this terminal, or open its login
shell.

DISCOBOX is the NAME "discobox ls" prints, an ID, or a short prefix of one.
Without it the discobox is taken from "discobox ls": the only one when there is
one, otherwise you are asked to pick. The command goes after --, and every
argument after the -- is the command's, whatever it looks like.

Without a command this starts a shell. In a terminal it is the one
$DISCOBOX_SHELL names on this machine — or nu, when run from nushell, or else
$SHELL — when the discobox has it: that path, or a shell of the same name on
the discobox's PATH once its login profile and the directory's .envrc have
loaded, so a shell the repository's dev environment provides counts. Otherwise
it is the discobox user's login shell, from that user's passwd entry.

Stdin is always attached, and a PTY is allocated only when this terminal is one,
so piping and redirecting behave like a local command. Signals are forwarded to
the remote process, and shell exits with its exit code. If an interrupt stops
getting through — the discobox or the server has gone quiet — Ctrl-C again says
so, and one more quits shell and leaves the command where it is.`,
		Example: `  discobox shell
  discobox shell mybox
  discobox shell mybox -- go test ./...
  discobox shell sbx_01hq -- ls -la
  discobox shell -- git log --oneline`,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Everything below talks to the server the discobox is on.
			a, projectID, sandboxID, _, cmdArgs, err := a.selectDiscoboxCommand(cmd, args)
			if err != nil {
				return err
			}
			// A PTY is only correct when every stream this exec touches is one:
			// allocating one for a pipe would echo input back and dress output in
			// escape sequences the consumer never asked for.
			tty := isTerminalStream(cmd.InOrStdin()) && isTerminalStream(cmd.OutOrStdout()) && isTerminalStream(cmd.ErrOrStderr())
			// No command means a shell. Only the sandbox can say which shell its
			// user has, so the request asks for one rather than naming it, and
			// says which one this machine's user would rather have — for a
			// terminal only: a shell reading a script piped into it must be the
			// one the script was written for, not whatever $SHELL says.
			opts := sandboxExecCreateOptions{interactive: true, tty: tty, shell: len(cmdArgs) == 0}
			if opts.shell && tty {
				opts.env = preferredShellEnv()
			}
			body, err := createSandboxExecBody(opts, cmdArgs)
			if err != nil {
				return err
			}
			exec, err := a.createSandboxExec(cmd.Context(), projectID, sandboxID, body, true)
			if err != nil {
				return err
			}
			if err := a.attachSandboxExec(cmd.Context(), projectID, sandboxID, exec.ID, true, tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}
			return a.returnSandboxExecStatus(cmd.Context(), projectID, sandboxID, exec.ID)
		},
	}
	return cmd
}

// discoboxCommandLine is a `[DISCOBOX] [-- ARG...]` command line: the
// discobox it names, empty for none, and the command after the --.
type discoboxCommandLine struct {
	discobox string
	command  []string
	// dashed is whether the line had its --, which is what a hint about where
	// a command goes needs to know.
	dashed bool
}

// splitDiscoboxCommand reads a `[DISCOBOX] [-- ARG...]` command line. Which
// argument is which is decided by the -- alone, never by what an argument
// looks like (cli/DESIGN.md, "Command Naming"): before it is at most the
// discobox, and after it is the command, -- included past the first. More than
// one argument before it is refused with the line that works.
//
// pflag consumes a -- while it still parses flags and says where it stood
// (ArgsLenAtDash); a command that parses none of its own (tools ssh) sees the
// -- as an argument instead, and it is found here.
func splitDiscoboxCommand(cmd *cobra.Command, args []string) (discoboxCommandLine, error) {
	line := discoboxCommandLine{}
	before := args
	if dash := cmd.Flags().ArgsLenAtDash(); dash >= 0 {
		before, line.command, line.dashed = args[:dash], args[dash:], true
	} else if i := slices.Index(args, "--"); i >= 0 {
		before, line.command, line.dashed = args[:i], args[i+1:], true
	}
	switch len(before) {
	case 0:
	case 1:
		line.discobox = before[0]
	default:
		return discoboxCommandLine{}, fmt.Errorf("%s takes one DISCOBOX, and the command goes after --: %s %s -- %s",
			cmd.CommandPath(), cmd.CommandPath(), before[0], strings.Join(before[1:], " "))
	}
	return line, nil
}

// selectDiscoboxCommand is splitDiscoboxCommand and selectSandbox together:
// the discobox a `[DISCOBOX] [-- ARG...]` line names, on the server it is on,
// and the command. A DISCOBOX that names nothing on a line without its -- is
// most likely a command that left it out, so the error says where one goes.
func (a *App) selectDiscoboxCommand(cmd *cobra.Command, args []string) (app *App, projectID, sandboxID string, client *apiclientgen.Client, command []string, err error) {
	line, err := splitDiscoboxCommand(cmd, args)
	if err != nil {
		return nil, "", "", nil, nil, err
	}
	app, projectID, sandboxID, client, err = a.selectSandbox(cmd, line.discobox)
	if err != nil && line.discobox != "" && !line.dashed {
		return nil, "", "", nil, nil, fmt.Errorf("%w\na command goes after --: %s -- %s", err, cmd.CommandPath(), line.discobox)
	}
	return app, projectID, sandboxID, client, line.command, err
}

// nameMatch says which of a discobox's names an argument is allowed to be.
//
// configuredName is the name the discobox was created with and nothing else.
// It is what an argument that may be something else has to match: `discobox cp`
// reads a reference before a colon as a discobox only when it names one, and a
// generated name ("brave-otter") cannot collide with a local path by accident.
//
// listedName also matches the NAME `discobox ls` prints, which is that
// configured name only until something titles the discobox's primary terminal
// and the window title takes the column (SandboxDisplayName, server-side). It
// is therefore the only name the user of a running discobox ever sees, and it
// is free-form text a harness chose: two discoboxes running the same agent
// commonly carry the same one. Arguments that can only be discoboxes take it —
// there is no command word to mistake one for — and pay for it by having to be
// unique among the candidates, which sandboxesNamed reports on.
type nameMatch int

const (
	configuredName nameMatch = iota
	listedName
)

// matchSandboxArg reports whether arg names one of sandboxes, the same
// candidates `discobox ls` shows: a full
// generated ID, an exact sandbox name in the sense match asks for, or a short
// ID. A full generated ID (id.IsGenerated) is trusted outright: its shape — a
// resource prefix plus 16 random characters — is unique enough that nothing
// else could collide with it by accident. A short ID is matched against these
// candidates; matching none of them is not an error here, and what that means
// is the caller's to say. Matching several is reported as ambiguous, the same as any other short-ID
// collision in the CLI, since arg's shape said it was meant as an ID. An ID
// full or short may be spelled with a hyphen, as the discobox's hostname
// spells it ("sbx-…"). A full one is trusted outright like any full ID; a
// short one is read that way only after no name matched as typed.
func matchSandboxArg(arg string, sandboxes []apimodel.Sandbox, match nameMatch) (id string, ok bool, err error) {
	if full := idpkg.Canonical(idpkg.PrefixSandbox, arg); idpkg.IsGenerated(full) {
		return full, true, nil
	}
	if id, ok, err := matchSandboxName(arg, sandboxes, match); ok || err != nil {
		return id, ok, err
	}
	arg = idpkg.Canonical(idpkg.PrefixSandbox, arg)
	if !isResolvableShortID(arg) {
		return "", false, nil
	}
	switch matches := idpkg.ResolveShort(arg, sandboxIDs(sandboxes)); len(matches) {
	case 0:
		return "", false, nil
	case 1:
		return matches[0], true, nil
	default:
		return "", false, fmt.Errorf("%q matches more than one discobox (%s); use a longer prefix", arg, strings.Join(matches, ", "))
	}
}

// matchSandboxName is the name step of matchSandboxArg, and the whole of it
// where a short ID is resolved elsewhere (selectSandboxOnEveryServer): the
// one discobox arg names under match, before any ID matching. A name is what
// the listing shows and what people type, and matching it in full leaves no
// room for the guessing a prefix invites.
//
// A name more than one discobox answers to is refused rather than picked
// between. Configured names are unique within a project
// (idx_sandbox_project_name), so a duplicate under configuredName would mean
// the candidate list spans projects; under listedName a shared window title
// is an everyday duplicate.
//
// So is a name that is also a short ID of another discobox. A listed name is
// written by the box — its meta name, its terminal's title — so a box naming
// itself `01hq` would otherwise take the commands, source and keystrokes meant
// for the discobox whose ID begins that way.
func matchSandboxName(arg string, sandboxes []apimodel.Sandbox, match nameMatch) (id string, ok bool, err error) {
	named := sandboxesNamed(arg, sandboxes, match)
	switch len(named) {
	case 0:
		return "", false, nil
	case 1:
	default:
		return "", false, fmt.Errorf("%q names more than one discobox (%s); use the discobox ID", arg, strings.Join(named, ", "))
	}
	if short := idpkg.Canonical(idpkg.PrefixSandbox, arg); isResolvableShortID(short) {
		for _, other := range idpkg.ResolveShort(short, sandboxIDs(sandboxes)) {
			if other != named[0] {
				return "", false, fmt.Errorf("%q names discobox %s and is a short ID of %s; use the discobox ID", arg, named[0], other)
			}
		}
	}
	return named[0], true, nil
}

// sandboxIDs is the IDs of sandboxes, in order.
func sandboxIDs(sandboxes []apimodel.Sandbox) []string {
	ids := make([]string, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		ids = append(ids, sandbox.ID)
	}
	return ids
}

// sandboxesNamed returns the IDs of the sandboxes arg names, under match. The
// match is the whole name and nothing less: a partial name would compete with
// short-ID matching for the same argument, and "did you mean a name or an ID"
// is not a question a command word should have to answer.
//
// Every ID that answers to arg comes back, not the first: a caller that cannot
// tell which discobox was meant must say so rather than act on one of them.
//
// A judge answers to no name (ADR 26-09-22-838 §1 leaves it out of listings);
// its ID still resolves, through the short-ID step and the shadow check.
//
// An empty arg names nothing. It would otherwise answer to any discobox whose
// own name is empty, which is a match on the absence of a name rather than on a
// name.
func sandboxesNamed(arg string, sandboxes []apimodel.Sandbox, match nameMatch) []string {
	if arg == "" {
		return nil
	}
	var ids []string
	for _, sandbox := range sandboxes {
		// A judge is named by its ID alone: it is in the candidates so a name
		// cannot shadow its short ID, and is not one `discobox ls` prints.
		if sandbox.Config.HarnessMode.Value == apiclientgen.SandboxConfigHarnessModeJudge {
			continue
		}
		if sandbox.Config.Name == arg || (match == listedName && sandboxListedAs(sandbox, arg)) {
			ids = append(ids, sandbox.ID)
		}
	}
	return ids
}

// sandboxListedAs reports whether arg is the NAME `discobox ls` prints for
// sandbox.
//
// Two spellings count, because the table does not print the display name
// verbatim: truncateTableValue collapses runs of whitespace and cuts anything
// past the column width down to a prefix and an ellipsis. An agent's title
// commonly runs past it, so matching the raw name alone would mean the one
// spelling on the user's screen — the one they can select and paste — is the
// one that does not resolve, which is the failure listedName exists to end.
func sandboxListedAs(sandbox apimodel.Sandbox, arg string) bool {
	if sandbox.DisplayName == arg {
		return true
	}
	return truncateTableValue(sandbox.DisplayName, sandboxNameColumnWidth) == arg
}
