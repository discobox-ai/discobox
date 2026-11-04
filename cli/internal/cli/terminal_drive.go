package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/execstream/client"
)

// send, send-key, capture and wait drive a discobox's terminal from the top
// level, named as rex names them (ADR 26-11-04-204 §§2-5). A rex block is a
// discobox terminal and a rex session a discobox. Each takes its DISCOBOX
// first and always — a script that drove the wrong box because only one was
// running is worse than one that fails — and the terminal by -t, primary by
// default. The raw forms stay under `admin terminal`.

// defaultIdle is how long `--until idle` waits for the terminal to be quiet
// when it names no duration.
const defaultIdle = 5 * time.Second

// terminalFlag adds -t/--terminal to cmd, primary by default, completed from
// the terminals of the discobox already named.
func (a *App) terminalFlag(cmd *cobra.Command, terminal *string) {
	cmd.Flags().StringVarP(terminal, "terminal", "t", primaryExecID, "use `TERMINAL`")
	_ = cmd.RegisterFlagCompletionFunc("terminal", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return a.completeTerminals(cmd, args[:1], toComplete)
	})
}

// completeDiscoboxFirst completes a DISCOBOX first, and nothing after it.
func (a *App) completeDiscoboxFirst(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return a.completeSandboxes(cmd, args, toComplete)
}

// driveTerminal resolves the discobox and terminal one of these commands
// names, and gives the App aimed at the discobox's server.
func (a *App) driveTerminal(cmd *cobra.Command, discobox, terminal string) (*App, string, string, string, *apiclientgen.Client, error) {
	a, projectID, sandboxID, api, err := a.selectSandbox(cmd, discobox)
	if err != nil {
		return nil, "", "", "", nil, err
	}
	terminalID, err := a.resolveSandboxExecID(cmd.Context(), projectID, sandboxID, terminal)
	if err != nil {
		return nil, "", "", "", nil, err
	}
	return a, projectID, sandboxID, terminalID, api, nil
}

// sendTerminalInput types parts into the terminal, printing nothing unless
// -o json asks for the route's answer, as rex's send prints nothing.
func (a *App) sendTerminalInput(cmd *cobra.Command, discobox, terminal string, parts []apimodel.SandboxExecInputPart) error {
	a, projectID, sandboxID, terminalID, api, err := a.driveTerminal(cmd, discobox, terminal)
	if err != nil {
		return err
	}
	res, err := api.SendSandboxExecInput(cmd.Context(), &apimodel.SandboxExecInputBody{Input: parts},
		apiclientgen.SendSandboxExecInputParams{ProjectId: projectID, SandboxId: sandboxID, ExecId: terminalID})
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
	return nil
}

func (a *App) newSendCommand() *cobra.Command {
	var terminal string
	cmd := &cobra.Command{
		Use:   "send DISCOBOX TEXT...",
		Short: "Send text to a discobox's terminal",
		Long: `Type TEXT into a discobox's terminal without attaching: the words, joined by
single spaces, all of them text. It arrives as a paste when the program asked
for bracketed paste, so a message with newlines in it stays one message.

It never presses Enter: submit with "discobox send-key DISCOBOX Enter". Then
"discobox wait" counts from this input, so it never misses the answer. It
prints nothing; -o json prints the resume point.`,
		Example: `  discobox send mybox 'run the tests again' && discobox send-key mybox Enter
  discobox send mybox -t term_1 'ls -la'`,
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: a.completeDiscoboxFirst,
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.Join(args[1:], " ")
			if text == "" {
				return fmt.Errorf("nothing to send")
			}
			return a.sendTerminalInput(cmd, args[0], terminal, []apimodel.SandboxExecInputPart{{Text: apiclientgen.NewOptString(text)}})
		},
	}
	a.terminalFlag(cmd, &terminal)
	return cmd
}

// sendKeyNames is the key names send-key's help lists, in tmux's spelling
// first (terminalKeyNames takes the API's own beside them).
const sendKeyNames = `Enter, Tab, Escape (Esc), BSpace, DC (Delete), Up, Down, Left, Right, Home,
End, PPage (PageUp), NPage (PageDown), Space, and C-a through C-z (^A too)`

func (a *App) newSendKeyCommand() *cobra.Command {
	var terminal string
	cmd := &cobra.Command{
		Use:   "send-key DISCOBOX KEY...",
		Short: "Send key presses to a discobox's terminal",
		Long: `Press each KEY in a discobox's terminal, in order, without attaching. Keys are
named as tmux names them:

  ` + strings.ReplaceAll(sendKeyNames, "\n", "\n  ") + `

A word that names no key is refused, never typed: text is "discobox send". It
prints nothing; -o json prints the resume point.`,
		Example: `  discobox send-key mybox Enter
  discobox send-key mybox C-c
  discobox send-key mybox Escape Up Enter`,
		Args: cobra.MinimumNArgs(2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return a.completeSandboxes(cmd, args, toComplete)
			}
			keys := []string{"Space"}
			for key := range terminalKeyNames {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			return keys, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			parts, err := terminalKeyParts(args[1:])
			if err != nil {
				return err
			}
			return a.sendTerminalInput(cmd, args[0], terminal, parts)
		},
	}
	a.terminalFlag(cmd, &terminal)
	return cmd
}

// terminalKeyParts is each key name as the input route takes it, refusing a
// word that names no key.
func terminalKeyParts(keys []string) ([]apimodel.SandboxExecInputPart, error) {
	parts := make([]apimodel.SandboxExecInputPart, 0, len(keys))
	for _, key := range keys {
		// Space is a key here, never a pasted " " (admin terminal input's
		// reading of it), which a program in bracketed paste takes as text.
		name, ok := terminalKeyNames[key]
		if key == "Space" {
			name, ok = key, true
		}
		if !ok {
			return nil, fmt.Errorf("%q names no key; keys are %s. Text is \"discobox send\"", key, strings.ReplaceAll(sendKeyNames, "\n", " "))
		}
		parts = append(parts, apimodel.SandboxExecInputPart{Key: apiclientgen.NewOptString(name)})
	}
	return parts, nil
}

func (a *App) newCaptureCommand() *cobra.Command {
	var terminal string
	var scrollback int64
	var trim bool
	cmd := &cobra.Command{
		Use:   "capture DISCOBOX",
		Short: "Print a discobox's screen and scrollback",
		Long: `Print what a discobox's terminal shows, as a person looking at it would read it:
the rendered screen, not the escape sequences that drew it. Reading does not
attach, resize the terminal, or count as activity.

Every row of the screen is printed, blank ones included, so a row can be found
by its place on the screen; --trim drops the blank rows below the last line
written. A discobox that is not running is read from the screen it last
posted, and says so on stderr.`,
		Example: `  discobox capture mybox
  discobox capture mybox --scrollback 200 --trim`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeDiscoboxFirst,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, projectID, sandboxID, terminalID, api, err := a.driveTerminal(cmd, args[0], terminal)
			if err != nil {
				return err
			}
			params := apiclientgen.GetSandboxExecScreenParams{ProjectId: projectID, SandboxId: sandboxID, ExecId: terminalID}
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
			noteStoredAnswer(cmd, screen.ObservedAt.Or(time.Time{}))
			text := screenText(screen)
			if !trim {
				text = strings.Join(append(slices.Clone(screen.Scrollback), screen.Lines...), "\n") + "\n"
			}
			_, err = io.WriteString(cmd.OutOrStdout(), text)
			return err
		},
	}
	a.terminalFlag(cmd, &terminal)
	cmd.Flags().Int64Var(&scrollback, "scrollback", 0, "also print up to `N` lines above the screen")
	cmd.Flags().BoolVar(&trim, "trim", false, "drop the blank rows below the last line written")
	return cmd
}

// waitUntil is what `wait --until` asked for, as the wait route takes it.
type waitUntil struct {
	hooks   []string
	program []apimodel.SandboxExecWaitProgramState
	quiet   time.Duration
	exit    bool
}

// The kinds a blocked program may be waiting on (OSC 7501).
var blockedKinds = []string{"permission", "question", "auth"}

// parseWaitUntil reads each --until: blocked[:KIND], finished, idle[=DURATION],
// exit, or a hook by name, hook:NAME for one named like a keyword
// (ADR 26-11-04-204 §3).
func parseWaitUntil(values []string) (waitUntil, error) {
	var until waitUntil
	if len(values) == 0 {
		return until, fmt.Errorf("nothing to wait for: give --until blocked, finished, idle, exit, or a hook")
	}
	for _, value := range values {
		switch {
		case value == "blocked":
			until.program = append(until.program, apimodel.SandboxExecWaitProgramState{State: apiclientgen.SandboxExecWaitProgramStateStateBlocked})
		case strings.HasPrefix(value, "blocked:"):
			kind := strings.TrimPrefix(value, "blocked:")
			if !slices.Contains(blockedKinds, kind) {
				return until, fmt.Errorf("--until %s: a blocked kind is %s", value, strings.Join(blockedKinds, ", "))
			}
			until.program = append(until.program, apimodel.SandboxExecWaitProgramState{
				State: apiclientgen.SandboxExecWaitProgramStateStateBlocked,
				Kind:  apiclientgen.NewOptSandboxExecWaitProgramStateKind(apiclientgen.SandboxExecWaitProgramStateKind(kind)),
			})
		case value == "finished":
			// No longer working, by whichever word the harness uses for it.
			for _, state := range []apiclientgen.SandboxExecWaitProgramStateState{
				apiclientgen.SandboxExecWaitProgramStateStateIdle,
				apiclientgen.SandboxExecWaitProgramStateStateDone,
				apiclientgen.SandboxExecWaitProgramStateStateError,
			} {
				until.program = append(until.program, apimodel.SandboxExecWaitProgramState{State: state})
			}
		case value == "idle" || strings.HasPrefix(value, "idle="):
			quiet := defaultIdle
			if text, ok := strings.CutPrefix(value, "idle="); ok {
				parsed, err := time.ParseDuration(text)
				if err != nil || parsed < time.Second || parsed%time.Second != 0 {
					return until, fmt.Errorf("--until %s: idle is a whole number of seconds, such as idle=10s", value)
				}
				quiet = parsed
			}
			if until.quiet == 0 || quiet < until.quiet {
				until.quiet = quiet
			}
		case value == "exit":
			until.exit = true
		default:
			hook := strings.TrimPrefix(value, "hook:")
			if hook == "" {
				return until, fmt.Errorf("--until %s names no hook", value)
			}
			until.hooks = append(until.hooks, hook)
		}
	}
	return until, nil
}

func (a *App) newWaitCommand() *cobra.Command {
	var terminal, after string
	var untils []string
	var limit time.Duration
	cmd := &cobra.Command{
		Use:   "wait DISCOBOX",
		Short: "Wait until a discobox is blocked, finished, idle, or exits",
		Long: `Wait until something happens in a discobox's terminal, and say what.

UNTIL is one of:
  blocked[:KIND]   the agent is waiting on a person (KIND: permission, question, auth)
  finished         the agent ended its turn
  idle[=DURATION]  no output or input for DURATION (default 5s)
  exit             the terminal's program exited
  HOOK             a harness hook, such as Stop or Notification (hook:NAME
                   for a hook named like one of the words above)

blocked and finished are what the agent reports of itself; a harness that
reports nothing is waited on by a HOOK, idle or exit. Repeat --until to wait
for whichever comes first. By default only what happens after the last input
sent to the terminal counts, so "send" then "wait" never misses the answer.
Without --for, wait has no time limit; with it, wait exits 124 when DURATION
passes. It prints what held, then the point a later --after resumes from.`,
		Example: `  discobox send mybox 'run the tests again' && discobox send-key mybox Enter
  discobox wait mybox --until finished --until blocked
  discobox wait mybox --until idle=10s --for 5m || echo timed out
  discobox send-key mybox C-c`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeDiscoboxFirst,
		RunE: func(cmd *cobra.Command, args []string) error {
			until, err := parseWaitUntil(untils)
			if err != nil {
				return err
			}
			if limit < 0 {
				return fmt.Errorf("--for is a length of time, such as 5m")
			}
			a, projectID, sandboxID, terminalID, api, err := a.driveTerminal(cmd, args[0], terminal)
			if err != nil {
				return err
			}
			var deadline time.Time
			if limit > 0 {
				deadline = time.Now().Add(limit)
			}
			result, err := a.waitTerminal(cmd.Context(), api, apiclientgen.WaitSandboxExecParams{ProjectId: projectID, SandboxId: sandboxID, ExecId: terminalID}, until, after, deadline)
			if err != nil {
				return err
			}
			if a.output == "json" {
				if err := writeTerminalSafeJSON(cmd.OutOrStdout(), result); err != nil {
					return err
				}
			} else if _, err := fmt.Fprintln(cmd.OutOrStdout(), waitAnswer(result)); err != nil {
				return err
			}
			if result.Reason == apiclientgen.SandboxExecWaitResultReasonTimeout {
				return client.ExitError{Code: 124}
			}
			return nil
		},
	}
	a.terminalFlag(cmd, &terminal)
	cmd.Flags().StringArrayVar(&untils, "until", nil, "wait for `UNTIL` (repeatable)")
	cmd.Flags().DurationVar(&limit, "for", 0, "give up after `DURATION`")
	cmd.Flags().StringVar(&after, "after", "", "count only what happens after resume `POINT`")
	return cmd
}

// waitTerminal waits on the terminal until something in until holds or the
// deadline passes, calling the route again as often as its 60s bound needs
// (ADR 0137 §3), each call resuming where the last left off. The first call
// counts from the terminal's last input unless after names a point
// (ADR 26-11-04-204 §4). A zero deadline waits without limit.
func (a *App) waitTerminal(ctx context.Context, api *apiclientgen.Client, params apiclientgen.WaitSandboxExecParams, until waitUntil, after string, deadline time.Time) (*apimodel.SandboxExecWaitResult, error) {
	body := &apimodel.SandboxExecWaitBody{Until: apimodel.SandboxExecWaitUntil{
		HookEvents:    until.hooks,
		ProgramStates: until.program,
	}}
	if until.quiet > 0 {
		body.Until.QuietSeconds = apiclientgen.NewOptInt64(int64(until.quiet / time.Second))
	}
	if until.exit {
		body.Until.Exit = apiclientgen.NewOptBool(true)
	}
	if after != "" {
		body.Until.After = apiclientgen.NewOptString(after)
	} else {
		body.Until.FromLastInput = apiclientgen.NewOptBool(true)
	}
	for {
		timeout := maxTerminalWait
		if !deadline.IsZero() {
			// What is left of --for, rounded up to the route's whole seconds.
			left := (time.Until(deadline) + time.Second - 1).Truncate(time.Second)
			timeout = min(timeout, max(time.Second, left))
		}
		body.TimeoutSeconds = int64(timeout / time.Second)
		res, err := api.WaitSandboxExec(ctx, body, params)
		if err != nil {
			return nil, err
		}
		result, err := expectResponse[apimodel.SandboxExecWaitResult](res)
		if err != nil {
			return nil, err
		}
		if result.Reason != apiclientgen.SandboxExecWaitResultReasonTimeout {
			return result, nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return result, nil
		}
		body.Until.After = apiclientgen.NewOptString(result.ResumeAfter)
		body.Until.FromLastInput = apiclientgen.OptBool{}
	}
}

// waitAnswer is the line wait prints: what held, written as --until would
// write it, then the resume point.
func waitAnswer(result *apimodel.SandboxExecWaitResult) string {
	var held string
	switch result.Reason {
	case apiclientgen.SandboxExecWaitResultReasonHook:
		hook, _ := result.Hook.Get()
		// The canonical name where the hook has one (ADR 0146 §6). Both are
		// the box's words.
		held = "hook:" + terminalSafe(hook.CanonicalEvent.Or(hook.Event)) + " " + terminalSafe(hook.ID)
	case apiclientgen.SandboxExecWaitResultReasonProgram:
		status, _ := result.ProgramStatus.Get()
		if status.State == apiclientgen.SandboxAgentProgramStatusStateBlocked {
			held = "blocked"
			if kind, ok := status.Kind.Get(); ok {
				held += ":" + string(kind)
			}
		} else {
			held = "finished"
		}
	case apiclientgen.SandboxExecWaitResultReasonQuiet:
		held = "idle"
	default:
		held = string(result.Reason)
	}
	return held + " " + terminalSafe(result.ResumeAfter)
}
