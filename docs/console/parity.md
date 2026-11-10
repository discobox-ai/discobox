# Console parity ledger

Every console command is reachable in both consoles, the TUI and the desktop
app, each the way its platform expects (ADR 26-11-07-598 §1). A command one
console lacks is a bug, or an entry here with its reason and the issue that
closes it, or why none will ("by design", "upstream"). An entry is removed in
the change that closes the gap.

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
palette; macOS is under "Platforms" below.

Beyond the registry the ledger holds what a person can do on each console's
screens that the other cannot (keys, pages, pointer gestures, what a window
does on its own), and where the desktop app runs. An entry closed "by design"
says why the difference is wanted, and one closed "upstream" names the library
that lacks what it needs; every other entry names its issue.

## The desktop app lacks

| Command | Reason | Closed by |
| --- | --- | --- |
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
| A link a program made (OSC 8), or a URL written as text, the same scheme-led, row-bounded detection; ⌘-click, Ctrl-click elsewhere | Yes: the desktop is the outermost terminal, so a web URL in text is a link to the modifier as in any terminal; the core follows a pane link only if it is a web address (absolute HTTP(S) with a host after forwarding) or a box address answered inside the console; other targets are refused with a status-line reason | `term::tests::a_link_a_program_made_is_found_at_its_cells`, `a_url_written_as_text_is_found_at_its_cells`, `bridge.TestAFollowedNonWebLinkIsRefused` |
| A link pointed at a box's forwarded port, opened at the local end on a ⌘-click (Ctrl-click elsewhere); `discobox:` links the console claims, followed on a plain left click too, before the program hears it, and lit under the pointer (text to select once the mouse is taken back) | Yes: where a link goes is the core's (`console.OpenedURL`, `ParseBoxLink`, `BoxLinkServer`, `AddressedBox`), asked over the bridge's `follow` | `console.TestForwardedURLPointsAtTheLocalEnd`, `TestAddressedBox`, `bridge.TestAFollowedURLGoesThroughTheHeadersForward`, `TestAFollowedBoxLinkEntersItsBox`, `contract::a_followed_link_is_answered_with_where_it_goes`, `term::tests::a_box_link_is_claimed_wherever_it_is`, `a_box_link_lights_under_the_pointer`, `command_tests::a_boxs_localhost_url_opens_at_its_forwarded_port`, `a_box_link_in_a_pane_opens_its_box`, `a_box_link_is_text_to_a_taken_back_mouse`, `a_middle_press_on_a_box_link_is_not_the_links`, `a_late_answer_to_a_link_is_dropped` |
| Mouse reporting in the mode and encoding the program asked for, taken back with `mouse` or Shift | Yes | `term::tests::the_mouse_is_reported_only_as_the_program_asked`, `a_press_the_program_heard_is_owed_its_release`, `command_tests::mouse_and_repaint_act_on_the_focused_pane` |
| The wheel to whoever can scroll, three lines a tick: the program, a pager's arrows, the scrollback | Yes | `term::tests::the_wheel_goes_to_whoever_can_scroll` |
| A program's status (OSC 7501) and a prompt (OSC 133 A) dropping it | Yes: the bridge reads the primary's stream by the protocol's rules (`programstatus.Set.Observe`), the core chooses between it and the poll (`console.Sandbox.AgentHeard`, the choice the TUI says on its terminal's tab and title), and the desktop's workspace header says it (`console.Header.Agent`) | `programstatus.TestObserveTakesReportsQueriesAndPrompts`, `console.TestAgentHeardIsTheNewerOfTheStreamAndThePoll`, `bridge.TestThePrimarysHeaderIsTheCoresChoice`, `contract::a_primarys_report_reaches_the_header`, `command_tests::the_header_says_what_the_core_sent` |
| Scrollback, and a find through it | Yes; termpane has no find, and the TUI's terminal has its own | `term::tests::find_walks_the_scrollback_and_scrolls_to_each_match`, `a_resize_drops_a_finds_match_and_the_next_step_finds_again`, `a_find_opens_again_on_what_it_last_looked_for`, `command_tests::find_opens_on_the_focused_pane_and_keeps_its_keys`, `a_find_closes_when_its_pane_loses_the_keys` |
| Wide characters, emoji and combining marks at their cells | Yes | `term::tests::a_wide_character_is_drawn_at_its_own_cells`, `combining_marks_stay_with_their_character` |
| An input method's composition at the cursor, and its commit typed | Yes | `command_tests::an_input_method_composes_at_the_cursor_and_commits_to_the_program` |
| Repaint after a size loss: the size asserted again, then the screen asked for | Yes, as `repaint` | `command_tests::mouse_and_repaint_act_on_the_focused_pane` |
| An image pasted (an empty paste, Ctrl-V or Alt-V on a harness terminal) or dropped (a paste of image paths) carried into the box and its path pasted, the pane's input held meanwhile (the TUI's `imagepaste.go`, not termpane's) | Yes: the `paste` gesture, `paste image` on a harness terminal (both with an image or image files copied to the clipboard), and files dropped on a pane (X11, macOS and Windows; Wayland reports no drags). Pasted text that is nothing but image paths (a path copied as text) is pasted as text on the desktop: it never reads files a clipboard's text names. Dictation through Ctrl-V (text a tool put on the clipboard a moment before, which the TUI's Windows clipboard watch tells from a key pressed by hand) is the TUI's alone: the desktop has no clipboard watch, and its Ctrl-V with text there is the program's | `paste_tests::a_pasted_image_is_carried_in_and_its_path_pasted_before_what_was_typed`, `the_paste_image_key_carries_an_image_in_on_a_harness_terminal_only`, `files_dropped_on_a_pane_are_carried_in_or_pasted_as_paths`, `bridge.TestUploadDroppedFiles` |
| The keymap, the prefix and reserved keys | Not a terminal behaviour here: the desktop's keys are the registry's (`desktop/src/keys.rs`) | `command_tests::no_shortcut_is_written_outside_the_spec` |

## Screens and keys the desktop app lacks

Things the TUI does that are not registry commands, and that the desktop app
does not do. Each has a way round on the desktop, said in its reason.

| Behaviour | Reason, and the way round | Closed by |
| --- | --- | --- |
| The automatic push: while a workspace is open, commits made on this machine are sent into the box's origin (`cli/internal/tui/push.go`, the core's `Workspace.PushLook` and `Pushed`) | The bridge does not run the core's push loop yet. `discobox push` sends them | #961 |
| The first harness change a project sees is explained before it is saved (`cli/internal/tui/harness_changes.go`, `Project.HarnessChangesExplained`) | The explainer's rule and text are the TUI's, not the core's, so the bridge cannot offer it. The notice and `review config` are there | #957 |
| The window title follows the box on screen and its primary's report (`model.go`'s title) | The app's title is fixed; the header and the list say what the agent is doing | #959 |
| The prompt and a comment opened in an external editor (Alt-E, F2) | No editor hand-off from the composer or the comment field; F2 is `rename` on the desktop | #958 |
| Questions answered by their letter (`y`, `n`, a choice's key); the create card's `1`–`9` and `e`; Shift-Tab on the prompt for the harness; Ctrl-D at an empty prompt | The sheets take the arrows and Enter; the harness is on the create card and in `options`; `primary+q` and `/exit` quit | #958 |
| Read-only cards (help, a harness's config card, an audit record) searched (`/`, `n`, `N`), copied (`c`) and scrolled; an issue tab's `y` (copy its URL) and line scroll | The help is one grid closed by Esc; an issue tab scrolls by page and the wheel, and `o` opens its page | #958 |
| Left and Right change the server on the harnesses and secrets pages | Each page is the server the list is narrowed to; `servers` changes it | #958 |
| The list's `page up`, `page down`, `scroll left` and `scroll right` gestures (bound in the TUI, not on the desktop) | The wheel scrolls the list, and a long name is cut | #958 |
| `discobox configure`, `discobox run` and `discobox attach BOX` open the console on its harnesses page, a new box or a box's workspace | `discobox desktop` takes no arguments; the palette goes to any of them | #958 |
| A leader key, and `--leader` | None needed: the desktop's chords reach no program (ADR 26-11-07-598 §2) | None: by design |
| The focused box's OSC 7501 report passed on to the terminal the console runs in | The desktop is the outermost terminal; it says the report in its header | None: by design |

## Screens and keys the TUI lacks

Things the desktop app does that the TUI does not. The pointer gestures are a
desktop app's; the TUI reaches each through its keys.

| Behaviour | Reason, and the way round | Closed by |
| --- | --- | --- |
| A request that arrives raises a native notification, and pressing it opens that request's card (`desktop/src/notify.rs`) | A terminal program has no notification service of its own; the TUI's band and the list's mark say it | None: by design |
| The column divider dragged, a double press halving it; a chip dragged to reorder its column, a double press maximizing it | Pointer layout is the desktop's (ADR 26-11-07-876 §2); the TUI zooms with `maximize` | None: by design |
| A folder dropped on the composer is the next create's source | A terminal hands a drop over as typed text; the TUI's create card takes a typed source | None: by design |
| A modified click marks a row (primary-click, Shift-click a run) | The TUI's rows take a plain click; Space and the range gesture mark | None: by design |
| Tooltips: each control's command, detail and shortcut | The TUI says its keys on its hint line and pane borders | None: by design |

## Platforms

Where the desktop app runs and what each platform gives it. The TUI runs
wherever the CLI does.

| Gap | Reason | Closed by |
| --- | --- | --- |
| The app runs on Linux only; on macOS and Windows `discobox desktop` says the platform is not yet supported | No signed builds for macOS and Windows yet, and Rune on Windows is unproven (ADR 26-11-07-983 §4) | #901, #789 |
| On macOS the native menu bar is a hand list of some commands (`MENU` in `desktop/src/main.rs`); the rest are in the palette | Rune's native menus are not built from the registry yet | #908 |
| A request's notification is raised on Linux only | macOS and Windows have their own notification services, not wired yet | #960 |
| Off macOS, `primary+1`–`9` (a tab) and `primary+alt+1`–`9` (a service's tab) never match: Shift turns the digit into another character | Rune reports the logical key only; `primary+0` is mapped back from `)` on a US layout | #914 |
| Files dropped on a pane do not reach the window on Wayland | winit reports no file drags on Wayland; a paste carries an image in | None: upstream |
