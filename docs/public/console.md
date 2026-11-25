# Console guide

Open the console with `discobox console`, or its alias `c`. `discobox
configure` opens the console on its harnesses screen. `discobox run` and
`discobox attach BOX` open a box's workspace directly.

## Create and find a task

The window opens on the first box, or on the prompt when there are no boxes.
Tab cycles through the prompt, the box list and the filter. Type a prompt and
press Enter. The create card asks where to cut the source from, and Enter
confirms. Tab on the card shows more options, such as the harness and extra
sources. Shift-Enter, Ctrl-Enter, Alt-Enter and Ctrl-J add a newline. Alt-E or
F2 opens the prompt in your external editor. Shift-Tab changes the harness.
Ctrl-O opens run options outside a terminal pane.

In the list, the arrow keys or j and k move the cursor, g and G jump to the
first and last box, and Page Up and Page Down move a screen. Tab moves to the
filter, and Enter opens its choices. Space marks a server, source or tag, and
Enter applies the selection. With several tags marked, the list shows the
boxes that carry all of them. The filter also sets the server and source for a
new task. The console remembers your filters and unfinished prompts.

| List key | Action |
| --- | --- |
| Enter / s | Attach / open a shell |
| Space | Select a box for batch actions |
| V | Extend a range with arrows; Space selects it; Esc cancels |
| c | Clear selection |
| v / z | Open VS Code / Zed |
| e / E | Rename / settings |
| t / T | Stop / start |
| Z | Pause. Freezes the box and keeps its memory. Start or open it to resume |
| x or Delete | Archive |
| U / P | Unarchive / purge |
| A | Show or hide archived boxes |
| u / R | Upgrade / repair in place |
| H | Review harness settings and login changes |
| . | Open all available actions |
| r | Refresh |
| Esc | Return to the prompt |

The bottom line lists the actions for the current selection. Purge asks you
to confirm, and archive does not. Check for uncommitted work before you
discard a box. The git column shows the branch and commit. A `*` marks
uncommitted work, and an up arrow marks a clean tree with commits ahead of the
starting commit.

## Work in terminals

The workspace shows harness terminals on the left and shells on the right.
The primary terminal is number 0. Service tabs have their own numbers: S1, S2
and so on. A second connection joins the same sessions. Switching boxes leaves
their sessions running. To start a stopped box in the sidebar, press Enter or
double-click it. Moving the cursor onto it does not start it.

The default leader is **Ctrl-A**. Press it, release it, then press the next
key. `DISCOBOX_LEADER=ctrl+b` changes it, as does
`discobox console --leader ctrl+b`. A bare letter means Ctrl plus that letter;
Alt plus a letter also works. Ctrl-C cannot be the leader.

| Leader then key | Action |
| --- | --- |
| q | Quit the window, keeping sessions running |
| d | Detach to the list, or close the window after a direct attach |
| a | Return to the primary terminal |
| c | Start another harness terminal |
| s | Start another shell |
| X | End the current nonprimary terminal or shell |
| Left / Right, h / l | Move between panes and their tabs |
| z | Zoom the focused column and restore it |
| m | Toggle mouse capture for selecting and copying |
| t / T | Stop / start the box |
| Z | Pause the box |
| x / U | Archive / unarchive |
| u / R | Upgrade / repair |

Each pane's border shows its keys. When the list stays beside the
workspace, leader then h, from the leftmost pane, moves focus to the list.
Leader then Up or Down, or k or j, switches boxes. With that sidebar showing
there is no detach-to-list action, so switch boxes or quit the window instead.

Other keys go to the focused terminal, including Ctrl-C, which interrupts
the program in the pane, not the console. Leader then X cannot end the primary
tab or a service tab. Use a service's stop action to stop it. An exited shell
leaves its final output on screen. The arrow keys, Page Up, Page Down, g and G
scroll it, and q, Esc or Enter dismisses it. When the primary terminal exits,
the workspace screen closes.

In a harness terminal, Ctrl-V or Alt-V with an image on the clipboard, or
dragging in an image file, copies the image into the box and pastes its path
there. With no image on the clipboard, Ctrl-V and Alt-V reach the program as
typed. Some shortcuts need your outer terminal to report modified keys. If one
does not work, check that terminal's keyboard protocol settings.

## Help and recovery

F1 opens help anywhere, and `?` opens it on lists. Help scrolls. `/` searches
it, Enter keeps the matches, n and N move between them, c copies the help, and
Esc closes it. The help in the console covers more screens and the actions
each one offers, including credentials, secrets, harnesses and services.

Ctrl-L repaints and refreshes, and inside a pane it repaints the program too.
Outside panes, Ctrl-C quits. At an empty prompt, Ctrl-D also quits, and so does
`/exit` or `/quit` alone in the prompt. Leaving the window keeps sessions
running. To stop the box, use the stop action.
