# Console guide

Open the console with `discobox console` (`c` is an alias). `discobox
configure` opens its harnesses screen. `discobox run` and `discobox attach BOX` open a workspace directly.

## The palette

The palette is the way in to everything the console does. Type any part of a
box's name, ID, repository or tags, or of a command's name, and Enter goes to
the box or runs the command. Tab turns the highlighted box or command into a
chip, so you can pick the other half: a command first and then the box it
acts on, or a box first and then a command. Esc drops the chip, then clears
the text, then closes the palette. It searches every server and source,
whatever the list's filter narrows to, but leaves archived boxes out until
you include them with Alt-A in the palette.

Open it with **Ctrl-:** from the list, the prompt or a terminal pane, when
your terminal speaks the Kitty keyboard protocol; it opens nothing over a
dialog, the run options or the create card. The leader then k (Ctrl-A, then
k, by default) opens it from the list or a pane in any terminal, and `:`
opens it from the box list. A harness or shell pane's bottom line leads with
the palette: it offers Ctrl-: once the terminal has said it tells such keys
apart, and the leader until then. The keys below are shortcuts to what the
palette finds.

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
| : | Open the palette |
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
| k | Open the palette |
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
Up/Down (or j for down) switches boxes, and so do Ctrl-Shift-K/J with no
leader in a terminal that speaks the Kitty keyboard protocol. There is no
detach-to-list action beside that sidebar: switch boxes or quit the window
instead.

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

## The desktop app

`discobox desktop` opens the same console in a native window: the same boxes,
commands, questions and terminals, operated the way a desktop app is. It runs
on Linux; on macOS and Windows the command says the platform is not yet
supported. The first run downloads the app this release of the CLI pins,
checks its digest, and keeps it in the discobox state directory; later runs
start it straight away. The app talks to the server only through the
`discobox` command that started it, so `--server`, `--token` and your
configured servers work as they do for `discobox console`. Closing the window
keeps sessions running.

### Keys

There is no leader. A desktop shortcut is written with **primary**, which is
Ctrl+Shift on Linux and Windows and ⌘ on macOS. Every other key goes to the
focused terminal, so Ctrl-A, Ctrl-C and Ctrl-W reach the program in the pane.

| Shortcut | Action |
| --- | --- |
| primary+K or primary+P | Command palette |
| F1 | Help: every command, by menu, with its shortcut |
| F10 | Menu bar (from the list) |
| Shift+F10, or a right click | A box's actions, or a pane's menu |
| primary+N | New discobox |
| primary+T / primary+Alt+T | Another shell / another harness terminal |
| primary+W | End the current terminal or shell |
| primary+Enter | Maximize the focused column and restore it |
| Ctrl+Tab / Ctrl+Shift+Tab | Next / previous pane |
| primary+Alt+Down / Up | Next / previous discobox |
| primary+B | Show or hide the list |
| primary+0 | Detach: back to all discoboxes |
| primary+Alt+O / primary+Alt+S | The box's tools / services |
| primary+L / primary+Alt+A | Filter by server / show archived |
| primary+R | Refresh |
| F2 | Rename |
| primary+Q | Quit, keeping sessions running |

Every command is also in the menu bar and the palette, whether or not it has a
shortcut. `theme` (in the View menu) draws the window dark, light or as the
system does; the choice is kept for the next window. The palette finds commands and boxes together: type a command such
as `stop` and a space, and the boxes it can act on follow; press Tab on a box,
and the commands it can take follow. Esc drops the chip, then the query, then
the palette. A command that cannot act says why, in the menu and when run.

### The list and new boxes

The composer under the list is what a new box is given to do: Return creates
it, Shift+Return adds a line. primary+N opens the create card, where the
source, harness, pool and server are chosen; `run options` sets every option
for the next create. Drop a folder on the composer to cut the next box from
it. Unfinished prompts are kept for the next window.

The control at the head of the list filters by server, source and tags. Space
marks a box, Shift+Space marks a range, primary+D clears the marks, and
Ctrl-click (⌘-click on macOS) or Shift-click marks with the pointer. A
lifecycle action from a box's menu or the palette acts on every marked box.

### Workspaces and terminals

A box's harness terminals are on the left and its shells on the right, each
column with its tabs in its frame. Drag the divider to resize the columns and
double-click it to halve them; drag a tab along its strip to reorder it, and
double-click it to maximize its column. Service tabs follow the harness
terminals in the left column; they show output and are stopped from the
services menu, not closed.
Switching to another box keeps the first one's sessions running.

The header says what the agent is doing, and shows the box's issue and pull
request, git state, ports and tags; a forwarded web port opens in the browser.
A notice under it says when a credential request is waiting on the box, a
credential was refused, or harness changes are ready to review; click it to
act, or close it.

In a terminal:

- Drag to select (double-click a word, triple-click a line, Alt-drag a
  block); the selection is copied. primary+C copies and primary+V pastes.
- Ctrl-click (⌘-click on macOS) opens a link. A `localhost` link the box
  printed opens at the port the console forwards; a discobox address opens
  that box on a plain click.
- A program that uses the mouse gets it; hold Shift, or use `toggle mouse`
  from the pane's menu, to select instead.
- primary+F finds text in the scrollback; Shift+Page Up and Page Down scroll
  it.
- Ctrl-V or Alt-V in a harness terminal, with an image on the clipboard, or
  an image file dropped on a pane, copies the image into the box and pastes
  its path. Files dropped that are not all images are pasted as paths. On
  Wayland the window receives no dropped files; paste instead.

### Pages, tabs and requests

`harnesses` and `credentials` open their pages beside the list, and `audit`
opens a box's audit timeline there. On a page, the arrows move, Enter opens
the row, Shift+F10 opens its actions and Esc leaves. In the audit timeline,
`/` searches as you type, as the terminal console's does: Enter keeps the
search and Esc clears it. Questions and forms open
as a sheet over the window; Tab moves between fields, Enter submits, and Esc
cancels. A destructive question starts on No.

`issue` and `pull request` open as tabs among the box's shells. In one, `c`
writes a comment (Ctrl+Enter, or ⌘↩ on macOS, posts it), `x` closes or
reopens, `m` merges a pull request, `f` shows its files changed, `o` opens
the page on GitHub, and `r` reads it again.

A credential request that arrives while the window is open raises a desktop
notification on Linux; clicking it opens the request.

### Differences from the terminal console

- The desktop app has no `all` command: its list is always beside the
  workspace, and `detach` returns to every box.
- primary+1 to 9 (a tab by its number) and primary+Alt+1 to 9 (a service's
  tab) do not work on Linux yet, because Shift turns the digit into another
  character. Click the tab, or use Ctrl+Tab.
- The prompt and comments cannot be opened in an external editor, and
  questions are answered with the arrows and Enter rather than by letter.
