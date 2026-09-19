package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/execstream/client"
)

// The terminal's read, type, and wait routes (ADR 0137), raw: one API call
// each. What a caller builds from them — sending a message and waiting for the
// agent's turn to end — needs to know which hooks end a turn for which
// harness, and belongs to a tool above this one.

// maxTerminalWait is the longest one wait call may hold (ADR 0137 §3).
const maxTerminalWait = 60 * time.Second

func (a *App) newSandboxTerminalScreenCommand(sandboxID *string) *cobra.Command {
	var scrollback int64
	cmd := &cobra.Command{
		Use:   "screen TERMINAL_ID",
		Short: "Print a discobox terminal's screen as text",
		Long: `Print what a discobox terminal shows, as a person looking at it would read it:
the rendered screen, not the stream of escape sequences that drew it. Reading
does not attach, resize the terminal, or count as activity.

Pass "primary" for the discobox's harness terminal.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeTerminals(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, api, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			terminalID, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, args[0])
			if err != nil {
				return err
			}
			params := apiclientgen.GetSandboxExecScreenParams{ProjectId: projectID, SandboxId: resolvedSandboxID, ExecId: terminalID}
			if scrollback > 0 {
				params.Scrollback = apiclientgen.NewOptInt64(scrollback)
			}
			res, err := api.GetSandboxExecScreen(cmd.Context(), params)
			if err != nil {
				return err
			}
			screen, err := expectResponse[apimodel.SandboxExecScreen](res)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return writeTerminalSafeJSON(cmd.OutOrStdout(), screen)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), screenText(screen))
			return err
		},
	}
	cmd.Flags().Int64Var(&scrollback, "scrollback", 0, "Also print up to this many lines above the screen")
	return cmd
}

// screenText is the scrollback and screen, without the blank rows below the
// last line anything was written on.
func screenText(screen *apimodel.SandboxExecScreen) string {
	lines := append(slices.Clone(screen.Scrollback), screen.Lines...)
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// terminalKeyNames maps what a key argument may be written as to the name the
// API takes. tmux's spellings are accepted beside the API's own.
var terminalKeyNames = func() map[string]string {
	names := map[string]string{
		"BSpace": "Backspace", "DC": "Delete", "PPage": "PageUp", "PgUp": "PageUp",
		"NPage": "PageDown", "PgDn": "PageDown", "Esc": "Escape", "Return": "Enter",
	}
	for _, key := range []string{"Enter", "Tab", "Escape", "Backspace", "Delete", "Up", "Down", "Left", "Right", "Home", "End", "PageUp", "PageDown"} {
		names[key] = key
	}
	for c := 'a'; c <= 'z'; c++ {
		names["C-"+string(c)] = "C-" + string(c)
		names["^"+strings.ToUpper(string(c))] = "C-" + string(c)
	}
	return names
}()

// terminalInputParts reads input arguments the way tmux send-keys does: an
// argument that names a key is that key, and anything else is text. With
// literal every argument is text.
func terminalInputParts(args []string, literal bool) []apimodel.SandboxExecInputPart {
	parts := make([]apimodel.SandboxExecInputPart, 0, len(args))
	for _, arg := range args {
		if key, ok := terminalKeyNames[arg]; ok && !literal {
			parts = append(parts, apimodel.SandboxExecInputPart{Key: apiclientgen.NewOptString(key)})
			continue
		}
		if arg == "Space" && !literal {
			arg = " "
		}
		if arg == "" {
			continue
		}
		parts = append(parts, apimodel.SandboxExecInputPart{Text: apiclientgen.NewOptString(arg)})
	}
	return parts
}

func (a *App) newSandboxTerminalInputCommand(sandboxID *string) *cobra.Command {
	var literal bool
	cmd := &cobra.Command{
		Use:   "input TERMINAL_ID KEY|TEXT...",
		Short: "Type keys and text into a discobox terminal",
		Long: `Type into a discobox terminal without attaching. Like tmux send-keys, an
argument that names a key is that key and anything else is text: Enter, Tab,
Escape, Backspace, Delete, Up, Down, Left, Right, Home, End, PageUp, PageDown,
Space, and C-a through C-z (tmux's BSpace, DC, PPage, NPage and ^C spellings
work too). --literal sends every argument as text.

Text arrives as a paste when the program asked for bracketed paste, so a
multi-line message is not submitted at its first newline. Pass "primary" for
the discobox's harness terminal.

It prints a resume point, taken just before the input was delivered: pass it
to wait as --after, and a hook the input causes is found however soon it is
recorded.`,
		Example: `  discobox admin terminal input primary --discobox-id sbx_1 "run the tests again" Enter
  discobox admin terminal input primary --discobox-id sbx_1 C-c`,
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: a.completeTerminals(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			parts := terminalInputParts(args[1:], literal)
			if len(parts) == 0 {
				return fmt.Errorf("nothing to send")
			}
			projectID, resolvedSandboxID, api, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			terminalID, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, args[0])
			if err != nil {
				return err
			}
			res, err := api.SendSandboxExecInput(cmd.Context(), &apimodel.SandboxExecInputBody{Input: parts},
				apiclientgen.SendSandboxExecInputParams{ProjectId: projectID, SandboxId: resolvedSandboxID, ExecId: terminalID})
			if err != nil {
				return err
			}
			result, err := expectResponse[apimodel.SandboxExecInputResult](res)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return writeTerminalSafeJSON(cmd.OutOrStdout(), result)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), result.ResumeAfter)
			return err
		},
	}
	cmd.Flags().BoolVarP(&literal, "literal", "l", false, "Send every argument as text")
	return cmd
}

func (a *App) newSandboxTerminalWaitCommand(sandboxID *string) *cobra.Command {
	var hooks []string
	var after string
	var quiet, timeout time.Duration
	var exit bool
	cmd := &cobra.Command{
		Use:   "wait TERMINAL_ID",
		Short: "Wait once for something to happen in a discobox terminal",
		Long: `Block until one of the conditions given holds, and say which: a harness hook
event (--hook), no output or input for a while (--quiet), or the terminal's
program exiting (--exit). It is one wait, held at most 60s.

It prints the reason — hook (with the event and hook ID), quiet, or exit — and
exits 0, or prints timeout and exits 124. The last field is where the next
wait resumes: pass it as --after, as input's output is passed, and a hook
recorded between two calls is not missed. Without --after, only hooks recorded
after the wait began count. Pass "primary" for the discobox's harness
terminal.`,
		Example: `  after=$(discobox admin terminal input primary --discobox-id sbx_1 "run the tests" Enter)
  discobox admin terminal wait primary --discobox-id sbx_1 --hook Stop --after "$after"
  discobox admin terminal wait primary --discobox-id sbx_1 --quiet 10s --exit`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeTerminals(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(hooks) == 0 && quiet <= 0 && !exit {
				return fmt.Errorf("nothing to wait for: give --hook, --quiet or --exit")
			}
			if quiet > 0 && quiet%time.Second != 0 {
				return fmt.Errorf("--quiet is counted in whole seconds")
			}
			if timeout <= 0 || timeout > maxTerminalWait || timeout%time.Second != 0 {
				return fmt.Errorf("--timeout runs from 1s to %s, in whole seconds", maxTerminalWait)
			}
			projectID, resolvedSandboxID, api, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			terminalID, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, args[0])
			if err != nil {
				return err
			}
			body := &apimodel.SandboxExecWaitBody{
				Until:          apimodel.SandboxExecWaitUntil{HookEvents: hooks},
				TimeoutSeconds: int64(timeout / time.Second),
			}
			if after != "" {
				body.Until.After = apiclientgen.NewOptString(after)
			}
			if quiet > 0 {
				body.Until.QuietSeconds = apiclientgen.NewOptInt64(int64(quiet / time.Second))
			}
			if exit {
				body.Until.Exit = apiclientgen.NewOptBool(true)
			}
			res, err := api.WaitSandboxExec(cmd.Context(), body,
				apiclientgen.WaitSandboxExecParams{ProjectId: projectID, SandboxId: resolvedSandboxID, ExecId: terminalID})
			if err != nil {
				return err
			}
			result, err := expectResponse[apimodel.SandboxExecWaitResult](res)
			if err != nil {
				return err
			}
			if a.output == "json" {
				if err := writeTerminalSafeJSON(cmd.OutOrStdout(), result); err != nil {
					return err
				}
			} else if hook, ok := result.Hook.Get(); ok {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s %s\n", result.Reason, hook.Event, hook.ID, result.ResumeAfter)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", result.Reason, result.ResumeAfter)
			}
			if result.Reason == apiclientgen.SandboxExecWaitResultReasonTimeout {
				return client.ExitError{Code: 124}
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&hooks, "hook", nil, "Harness hook event to wait for, such as Stop (repeatable)")
	cmd.Flags().StringVar(&after, "after", "", "Only hooks recorded after this resume point, as input or a previous wait printed it")
	cmd.Flags().DurationVar(&quiet, "quiet", 0, "Wait for no output or input for this long (e.g. 10s)")
	cmd.Flags().BoolVar(&exit, "exit", false, "Wait for the terminal's program to exit")
	cmd.Flags().DurationVar(&timeout, "timeout", maxTerminalWait, "Give up after this long, at most 60s")
	return cmd
}
