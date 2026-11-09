# Console guide

Open the console with `discobox console` (`c` is an alias). `discobox
configure` opens its harnesses screen. `discobox run` and `discobox attach BOX` open a workspace directly.

## Create and find a task

The window opens on the first box, or on the prompt when there are none.
Tab cycles through the prompt, box list and filter. Type a prompt and press
Enter. The create card asks where to cut the source from; Enter confirms.
Tab on the card exposes options such as the harness and additional sources.
Shift-Enter adds a newline; Ctrl-Enter, Alt-Enter and Ctrl-J also do so.
Alt-E or F2 opens the prompt in your external editor. Shift-Tab changes the
harness. Ctrl-O opens run options outside a terminal pane.

In the list, use arrows or j/k to move, g/G for first/last, and Page Up/Down
to move a screen. Tab moves to the filter; Enter opens its choices. Space
marks a server, source or tag and Enter applies the selection. Multiple tags
match boxes carrying every selected tag. The filter also determines the
server and source for a new task. Filters and unfinished prompts are remembered.

| List key | Action |
| --- | --- |
| Enter / s | Attach / open a shell |
| Space | Select a box for batch actions |
| V | Extend a range with arrows; Space selects it; Esc cancels |
| c | Clear selection |
| v / z | Open VS Code / Zed |
| e / E | Rename / settings |
| t / T | Stop / start |
| x or Delete | Archive |
| U / P | Unarchive / purge |
| A | Show or hide archived boxes |
| u / R | Upgrade / repair in place |
| H | Review harness settings and login changes |
| . | Open all available actions |
| r | Refresh |
| Esc | Return to the prompt |

The bottom line offers actions available for the current selection. Purge
asks for confirmation; archive does not. Check for uncommitted work before
discarding a box. The git column shows branch and commit, with `*` for dirty
work and an up arrow for clean commits ahead of the starting commit.

## Work in terminals

The workspace shows harness terminals on the left and shells on the right.
The primary terminal is number 0. Service tabs have separate S1, S2 numbers.
A second connection joins the same sessions. Switching boxes leaves their
sessions running. A stopped box in the sidebar requires Enter or a double
click to start; moving the cursor onto it does not start it.

The default leader is **Ctrl-A**. Press it, release it, then press the next
key. `DISCOBOX_LEADER=ctrl+b` changes it, as does
`discobox console --leader ctrl+b`. A bare letter means Ctrl plus that letter;
Alt plus a letter is also supported. Ctrl-C cannot be the leader.

| Leader then key | Action |
| --- | --- |
| q | Quit the window, keeping sessions running |
| d | Detach where a return to the list is available; a direct attach closes |
| a | Return to the primary terminal |
| c | Start another harness terminal |
| s | Start another shell |
| X | End the current nonprimary terminal or shell |
| Left / Right, h / l | Move between panes and their tabs |
| z | Zoom the focused column and restore it |
| m | Toggle mouse capture for selecting and copying |
| t / T | Stop / start the box |
| x / U | Archive / unarchive |
| u / R | Upgrade / repair |

The visible pane border shows its keys. Where the list remains beside the
workspace, leader h from the leftmost pane returns focus to the list; leader
Up/Down (or k/j) switches boxes. There is no detach-to-list action beside
that sidebar: switch boxes or quit the window instead.

Other keys go to the focused terminal, including Ctrl-C. This interrupts
the program in the pane, not the console. The primary and service tabs cannot
be ended with leader X. Use a service's stop action to stop it. An exited
shell leaves its final output readable; arrows, Page Up/Down and g/G scroll,
and q, Esc or Enter dismisses it. The primary terminal exiting closes the
workspace screen.

An image pasted with Ctrl-V or Alt-V in a harness terminal, or a dragged
image file, is copied into the box and its path pasted there. This differs
from ordinary text paste. Some shortcuts depend on your outer terminal
reporting modified keys; consult its keyboard protocol settings if needed.

## Help and recovery

F1 opens help anywhere; `?` does so on lists. Help scrolls. `/` searches it,
Enter keeps the matches, n/N moves between them, c copies the help, and Esc
closes it. The in-app help describes additional screens and context-specific
actions, including credentials, secrets, harnesses and services.

Ctrl-L repaints and refreshes; inside a pane it repaints the program too.
Outside panes, Ctrl-C quits. At an empty prompt, Ctrl-D also quits; `/exit`
or `/quit` alone in the prompt does the same. Leaving the window keeps
sessions running. Use the stop action when you want to stop the box.
