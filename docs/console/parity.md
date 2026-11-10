# Console parity ledger

Every console command is reachable in both consoles, the TUI and the desktop
app, each the way its platform expects (ADR 26-11-07-598 §1). A command one
console lacks is a bug, or an entry here with its reason and the issue that
closes it. An entry is removed in the change that closes the gap.

Both consoles run the shared parity scenarios
(`cli/internal/console/testdata/parity/*.json`) in CI, each pressing its own
bindings: the TUI in `go tool task ci:test`, the desktop app in
`go tool task ci:desktop`, on Linux.

## What the ledger covers

The commands of the console core's registry (`cli/internal/console/commands.go`),
by their registry IDs, which the core's spec hands to the desktop app
(`cli/internal/console/spec.go`, exported to `desktop/src/spec.json`). The
desktop app's test `every_registry_command_is_answered_or_in_the_ledger`
fails when a registry command is neither answered by the app nor listed
below, and when an entry here names a command the app answers.

Moving between panes and boxes is in the registry too (`next pane`,
`previous box`, `sidebar`): where focus goes is each console's own (ADR
26-11-07-876 §2), but the commands and their keys are the registry's, and
held here like any other. Gestures (the list's cursor, a pane by its number)
are declared in the registry with each console's keys, and are not commands.

Off macOS the desktop's menu bar is drawn in the window from the registry
(each command's desktop `menu`), so every command is in a menu as well as the
palette. On macOS the native menu bar is still a hand list of some commands
(`MENU` in `desktop/src/main.rs`), until #908 builds it from the registry.

## The desktop app lacks

| Command | Reason | Closed by |
| --- | --- | --- |
| `audit` | Needs the audit view. | #816 |
| `issue` | Needs the issue tab. | #817 |
| `pr` | Needs the pull request tab. | #817 |
| `files` | Needs the pull request's files-changed review. | #817 |
| `credentials` | Needs the secrets page. | #813 |
| `requests` | Needs the credential requests page. | #814 |
| `rejections` | Needs the rejected credentials page. | #814 |
| `dismiss` | The app has no notices to dismiss until it raises requests as notifications. | #814 |
| `all` | The command is for a window opened on one box, which the desktop has none of: its list is always beside the workspace, and "all discoboxes" in the sidebar is `detach`. | None: by design |

## The TUI lacks

Nothing.

## Terminal behaviours

A pane is a terminal in both consoles: termpane in the TUI
([`termpane/DESIGN.md`](../../termpane/DESIGN.md)), and `alacritty_terminal`
in the desktop app (`desktop/src/term.rs`). Each behaviour termpane has is the
desktop's too, with the test that holds it, or an entry here with its reason.
The desktop's own keys for a pane (copy, paste, find, a screen back or on) are
registry gestures; the TUI leaves those to the terminal it runs in.

| Behaviour | Desktop | Held by |
| --- | --- | --- |
| Selection: drag, a word on a double click, a line on a triple, a block with Alt, past an edge scrolling; copied on release | Yes | `term::tests::a_drag_selects_and_a_click_lets_it_go`, `a_double_click_is_a_word_and_a_triple_a_line`, `an_alt_drag_selects_a_block`, `a_drag_past_the_top_scrolls_back_and_keeps_selecting` |
| Copy chord over a selection, right button copies one | Yes, as the `copy` gesture | `command_tests::the_copy_and_paste_gestures_are_the_panes` |
| Paste, bracketed when the program asks, markers stripped | Yes, as the `paste` gesture | `term::tests::a_paste_is_bracketed_when_asked_and_cannot_close_itself`, `command_tests::the_copy_and_paste_gestures_are_the_panes` |
| A program's copy (OSC 52); a clipboard read answered with silence | Yes | `term::tests::a_programs_copy_is_taken_and_a_read_is_answered_with_silence`, `command_tests::a_programs_copy_reaches_the_clipboard`, `only_the_box_on_screen_copies` |
| A link a program made (OSC 8) | Yes: ⌘-click, Ctrl-click elsewhere | `term::tests::a_link_a_program_made_is_found_at_its_cells` |
| A link pointed at a box's forwarded port, and `discobox:` links the console claims | No: the desktop opens a link as written | #921 |
| Mouse reporting in the mode and encoding the program asked for, taken back with `mouse` or Shift | Yes | `term::tests::the_mouse_is_reported_only_as_the_program_asked`, `a_press_the_program_heard_is_owed_its_release`, `command_tests::mouse_and_repaint_act_on_the_focused_pane` |
| The wheel to whoever can scroll, three lines a tick: the program, a pager's arrows, the scrollback | Yes | `term::tests::the_wheel_goes_to_whoever_can_scroll` |
| A program's status (OSC 7501) and a prompt (OSC 133 A) dropping it | Yes: the bridge reads the primary's stream by the protocol's rules (`programstatus.Set.Observe`), the core chooses between it and the poll (`console.Sandbox.AgentHeard`, the choice the TUI says on its terminal's tab and title), and the desktop's workspace header says it (`console.Header.Agent`) | `programstatus.TestObserveTakesReportsQueriesAndPrompts`, `console.TestAgentHeardIsTheNewerOfTheStreamAndThePoll`, `bridge.TestThePrimarysHeaderIsTheCoresChoice`, `contract::a_primarys_report_reaches_the_header`, `command_tests::the_header_says_what_the_core_sent` |
| Scrollback, and a find through it | Yes; termpane has no find, and the TUI's terminal has its own | `term::tests::find_walks_the_scrollback_and_scrolls_to_each_match`, `a_resize_drops_a_finds_match_and_the_next_step_finds_again`, `a_find_opens_again_on_what_it_last_looked_for`, `command_tests::find_opens_on_the_focused_pane_and_keeps_its_keys`, `a_find_closes_when_its_pane_loses_the_keys` |
| Wide characters, emoji and combining marks at their cells | Yes | `term::tests::a_wide_character_is_drawn_at_its_own_cells`, `combining_marks_stay_with_their_character` |
| An input method's composition at the cursor, and its commit typed | Yes | `command_tests::an_input_method_composes_at_the_cursor_and_commits_to_the_program` |
| Repaint after a size loss: the size asserted again, then the screen asked for | Yes, as `repaint` | `command_tests::mouse_and_repaint_act_on_the_focused_pane` |
| The keymap, the prefix and reserved keys | Not a terminal behaviour here: the desktop's keys are the registry's (`desktop/src/keys.rs`) | `command_tests::no_shortcut_is_written_outside_the_spec` |
