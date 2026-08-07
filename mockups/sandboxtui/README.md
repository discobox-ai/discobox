# sandboxtui — a launcher mockup

A mockup of a `disco` launcher, in difftui's style: no full screen colour, panes
told apart by a title bar, the cursor a chevron over a highlighted row, and
every command a single letter. The composer is Claude Code's: a rule above and below, a chevron
in front of the text, a mode line underneath, and a box that grows a line at a
time as you type.

It is inline, not an alt screen: it takes the rows it needs, scrolls with the
terminal, and leaves what it printed in the scrollback when it exits. The cost
of that is resize — the terminal reflows the frame already on screen, and the
renderer then paints a new one under the old instead of over it — so a width
change erases the screen before redrawing. Height alone reflows nothing and is
left alone, and so is the first size message, which is the window opening.

It is wired to nothing. The sandboxes are invented (`internal/ui/data.go`), and
every action prints the CLI command it would have run instead of running it.
The point is the interaction, not the plumbing.

```bash
cd mockups/sandboxtui && go run .
```

It carries its own `go.mod` and `go.work` so it stays out of the repository
workspace and cannot perturb the real modules.

## The idea

Starting something new is the common case, so the window opens with the cursor
already in a prompt: type, press Enter, done. The sandboxes you already have are
one press of Tab away, and once you are up there every letter is a command.

```
disco  discobox  ~/src/disco2 @ main                                   F1 help · Ctrl-C quit
                             Sandboxes                          3 selected  ·  8 sandboxes
   ▗▖
   ▍▋                       ▒▒● fix flaky pool reaper tests▒▒▒▒▒▒ claude ▒▒ disco2 ▒main@a3f9c21*▒ 2m ago ▒+142 −38▒▒
   ▌▋  ▁▁                    ❯▓● exec/terminal consolidation▓▓▓▓▓▓ claude ↑▓ disco2 ▓main@a3f9c21 ▓18m ago▓+903 −511▓
    ▄▆▇▄▄▃▂▆▅▃▂▁               ○ openapi: sandbox upgrade field   codex    disco2  main@1c713f6   1h ago  +61 −12
     ▎▌ ▗▄▖  ▆▅▄▃▂▆▄            ◐ userns pool ADR spike           claude   disco2  main@1c713f6   1h ago
     ▍▌ ▝▃▘▘▇▆ ▅▅  ▎▍           ● docs: adr 0021 acceptance       shell  ↑ obot    main@77e0a44   5h ago  +18 −4
      ▖▄   ▇▆  ▄▄ ▗ ▎
     ▃▄ ▇▄▃▂▁  ▁▃▆▃▆▖▖
    ▋     ▁▂▃     ▝▖▃▘
    ▌     ▖  ▝      ▄
    ▝▅▅▅▅▅▅   ▝▅▅▅▅▅▅
  Enter runs the prompt in a new sandbox, or just creates one when it is empty
────────────────────────────────────────────────────────────────────────────────────────────
❯ make the pool reaper stop leaking volumes
────────────────────────────────────────────────────────────────────────────────────────────
  ⏵⏵ claude · clean · detached · 2 env · 1 secret · ~/src/disco2 @ main
  Tab or ↑↓ select sandbox · Shift-Tab options · Alt-E editor · Ctrl-Enter newline · Ctrl-D quit
```

Two lines around the composer, and they say different kinds of thing: what
pressing Enter in the field does, which is a label on the field and sits against
it, and the keys, which are keys and sit on the status line at the bottom. A
message — "selected 3 sandboxes", "visual select cancelled" — displaces the keys
until the next one is pressed.

The header spans the window; under it the mark stands beside the list, and the
composer spans both again. The mark is `internal/ui/logo.chars`, embedded as
captured — it carries its own colours rather than going through a style. Below
100 columns it is dropped and the list takes the whole row: decoration is the
first thing a narrow terminal loses.

`NO_COLOR` is honoured (any non-empty value, per no-color.org): it forces the
renderer to plain ASCII, drops the mark — it is shading rather than line art,
with nothing left worth drawing without colour — and swaps the state glyphs for
the words. The cursor still reads: it is a chevron, and only the row highlight
behind it goes.


A row carries what the sandbox is costing while it runs: cpu and memory as a
share of their quota, and the disk as bytes — "14 GiB" tells you what a
percentage of a volume you cannot picture does not. All three take their colour
from the share, amber past 75% and red past 90%. A sandbox that is not up shows
dots rather than three zeroes, which would read as "idle" instead of "off". A name too long for its column is ellipsized, and `←→` walks
the row under the cursor sideways to read the rest.

Archived sandboxes are hidden until `A` asks for them; the title bar says how
many are waiting. `x` archives, which is reversible and asks nothing; `U`
brings one back; `P` purges, which is not reversible and asks first.

The keys along the bottom are only the ones the sandboxes under the cursor can
take — `u upgrade` when one is available, `U unarchive` and `P purge` only on
archived sandboxes — because a key list that offers purge on a running sandbox
is a key list you stop reading. The action menu (`.`) is the opposite: it keeps
the unavailable ones, with the reason.

The state is the coloured glyph in front of the name and nothing else — `●`
running, `◐` starting, `○` stopped, `▪` archived, `✗` error — because spelling
"running" out on every row costs a column to say what the dot already said.
Without colour that trade reverses: half of what the glyphs carry is their
colour, so the glyph goes and the word comes back in a column of its own.

A row carries where the sandbox came from and when you last touched it: the
origin folder (blue when it is not this one), the commit it was spawned from
(starred when it was cut from a snapshot of uncommitted work on top of that
commit), and the time since it was last used. The fixed columns drop off the
right end as the terminal narrows; the name never does.

## Keys

**Prompt** — Enter runs it, and an empty prompt is not an error: it creates a
sandbox with nothing given to the harness, which is the other thing you come
here for. Ctrl-Enter (or Alt-Enter, or Ctrl-J, which is what
most terminals actually send for Ctrl-Enter) is a newline, Ctrl-D on an empty
prompt quits, and Alt-E — or F2, where Option is not Meta — writes the prompt in
`$EDITOR`. That last one is the only thing here that is not a mock: it really
runs your editor, because the interaction cannot be judged without doing it.

Tab moves to the sandbox list and Tab again comes back; Shift-Tab is the run
options, a layer over both.

The arrows cross too, but they are line motions first — a line at a time,
wrapped rows included — and only walk out of the field from the row they cannot
move off. On that row they go to the start or the end of the line, and only
from there into the list: Up to where you last were, Down to the top. So
editing a multi-line prompt behaves like a text field, and leaving it is still
two presses in the direction you were already going.

**List** — `↑↓`/`kj` move, Space selects (commands then act on everything
selected), and both ends of the list lead back to the prompt — `↑` off the top
and `↓` past the bottom — as do Tab and Esc.
Nothing is picked out while the prompt has focus — the cursor belongs to the
pane that has it — but the selection stays put.

Selection is a background, not a column of bullets: it is the state of a row,
not a field of it. Three bands, shown above as `▒` selected, `▓` selected and
under the cursor, and a dim grey for the cursor alone. "Both" has to be its own
colour, or moving the cursor onto a selected row would hide that a command is
about to act on it.
`V` draws a range, difftui style: `↑↓` extend it, a command acts on the whole
of it, Space selects the whole of it so it outlives the mode, `V` or Esc
cancels.
Enter attaches; `s` shell, `d` diff, `y` apply, `i` status, `u` upgrade,
`t`/`T` stop and start, `x` archive, `U` unarchive, `P` purge, `.` opens all of
them as a menu. `A` shows or hides archived sandboxes, `f` filters to ones
started from this directory, `c` clears the selection.

Actions that do not apply stay on the menu with the reason — `upgrade` reads
"already on the current image" rather than vanishing.

**Run options** (Shift-Tab) — these are `disco run`'s flags and nothing else:
`--harness`, `--include-dirty`, `--detach`, `-e`, `-s`, plus `-C DIR@REF`, the
source the sandbox is cut from. A launcher that offers options the command does
not have is a launcher you cannot reproduce from a shell.

The project is not among them. It is set once for the session — `go run . -project obot`,
as `disco -p` sets it for a shell — and named in the header only when it is not
the default one, because a header that says "default" every time teaches you to
skip the header. It rides along in every command the window would run. `↑↓` move, `←→` change, Enter edits a text option or
adds an env/secret, Backspace drops the last one, Ctrl-R runs with them. The
chip strip above the prompt always shows what is set, so the panel never has to
be open to know what Enter will do. The panel shows the `disco run` command it
describes, live.

## Looking at it without a terminal

```bash
go test ./internal/ui -run TestFrames -v
```

renders every state — prompt, list, multiselect, action menu, options, run — to
the test log.
