# 26-11-04-204 — Help is grouped and colored, and a terminal is driven from the top level with `send`, `send-key`, `capture` and `wait`

- **Status**: Accepted
- **Date**: 2026-11-04

  Accepted by the owner by merging #548, 2026-11-04. §4 amended before
  implementation landed, by the owner's call: the last input is the last the
  input route wrote, not any write to the terminal.

- **Amends**: [0137](0137-a-terminal-can-be-read-typed-into-and-waited-on-without-attaching.md)
  §3, whose wait gains two conditions and a resume point of its own, and whose
  `quietSeconds` counts from the point the wait counts from rather than from
  the call. The rest of 0137, its 60s bound included, stands.
- **Relates to**: [0146](0146-a-hook-keeps-the-name-its-harness-used-and-gains-a-canonical-one.md),
  a hook's canonical name; [26-10-16-424](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md),
  the program status a terminal reports; [26-10-24-254](26-10-24-254-a-discobox-is-one-positional-and-flags-take-one-type-each.md),
  `DISCOBOX` as the one positional.
- **Issue**: #543

## Context

`discobox --help` prints cobra's one flat "Available Commands:" list, in
registration order (`cli/internal/cli/root.go`, `newRootCommand`), with no
grouping and no color. Its flags include transport knobs few people need:
`--auto-start-server`, `--iroh-log`, `--iroh-relay`. Cobra's `help` command is
renamed `no-help` and hidden, because cobra's template lists a command named
`help` even when it is hidden. `ls` answers to `ps`, and help prints it.

Driving a box's terminal without attaching is three raw commands under
`discobox admin terminal` (`cli/internal/cli/sandbox_terminal_io.go`), one API
call each (ADR 0137):

- `screen DISCOBOX TERMINAL_ID [--scrollback N]` prints the rendered screen,
  less the blank rows below the last line written (`screenText`).
- `input DISCOBOX TERMINAL_ID KEY|TEXT... [--literal]` reads its arguments as
  tmux `send-keys` does (`terminalKeyNames`, `terminalInputParts`): a word that
  names a key is the key, any other word is text. It prints a resume point.
- `wait DISCOBOX TERMINAL_ID [--hook H] [--idle D] [--exit] [--after P]
  [--timeout D]` is one wait of at most 60s (`maxTerminalWait`), and exits 124
  on timeout. Its `--timeout` is its own flag. There is no global `--timeout`.

Nearly every use passes `primary` as `TERMINAL_ID`. And `wait` cannot say what
a person driving an agent wants to know, "the agent is done" or "the agent is
waiting on me", except through a harness's hook names, which ADR 0137 §3 leaves
to the caller.

The box already knows those two things. Its primary terminal's program reports
its state over OSC 7501 (ADR 26-10-16-424): `idle`, `working`, `done`,
`blocked` with a `kind` of `permission`, `question` or `auth`, and `error`
(`pkg/programstatus`). The shim's screen emulator keeps the records as each
report arrives (`shimruntime/screen.go`, `programstatus.Set.Apply`), and
`discobox ls`'s AGENT column and `discobox events`' `program.state` show the
effective one. The wait does not read it: it waits on hooks from the
collector's store, and on output, input and exit from the shim
(`sandbox-agent/server/terminal_io.go`, `WaitSandboxExec`).

Two facts about today's wait bear on "send, then wait":

- **Where it counts from.** Without `--after` a wait counts hooks from the
  moment it is called (`HarnessHookResumePoint`). `input` prints the point
  taken just before it delivers, for the caller to pass as `--after`. A caller
  that drops it and waits later can miss a quick reply.
- **What the shim knows of input.** Every write to the PTY, from the input
  route or from an attached person's keys, goes through `shimRuntime.writeInput`,
  which stamps `Runtime.inputAt` (`NoteInput`). The shim already holds when
  the terminal was last written to.

[rex](https://www.superlogical.com/rex/), the terminal multiplexer whose
program-status protocol the box speaks, has the help style and the command
names wanted here: `capture`, `send`, `send-key`, `wait`. A rex block is a
discobox terminal and a rex session is a discobox.

## Decision

### 1. Help is grouped, and drawn the way rex draws it

Root help lists commands under groups (cobra's `AddGroup`/`GroupID`), in this
order:

| Group | Commands |
| --- | --- |
| Basic commands | `run`, `ls`, `attach`, `exec`, `start`, `stop`, `restart`, `rm`, `rename`, `tag` |
| Automation | `send`, `send-key`, `capture`, `wait`, `events` |
| Interface | `console` |
| Files & ports | `cp`, `push`, `proxy`, `tools` |
| Resources | `secret`, `trust` |
| Setup | `configure`, `admin` |
| Other | `completion`, `help`, `version` |

`console` is the interactive UI, so it gets a group of its own rather than
sitting among setup commands. A command added later names its group, and a
test fails on a listed top-level command with none.

One help template, the root's, draws every command's help:

- Section headers are bold: `Usage:`, each group, `Options:`,
  `Global options:`. A command whose subcommands have no groups lists them
  under `Commands:`.
- Command and flag names are cyan, their descriptions aligned in one column.
  Aliases are inline, `ls, list`.
- A flag's value is an uppercase name: `--color WHEN`, `--server SERVER`,
  `--for DURATION`. It is the backquoted word in the flag's usage, pflag's
  own convention (`UnquoteUsage`), so a usage reads "set color \`WHEN\` to
  auto, always, or never". A flag's usage is a lowercase fragment, as rex
  writes it. A command's short help stays a sentence (cli/DESIGN.md "Command
  Naming").
- A command's own flags are under `Options:`; the persistent flags it
  inherits are under `Global options:`.

`--color WHEN` is a new global flag: `auto` (the default), `always` or
`never`. `auto` colors a stream when it is a terminal that shows color, as
`streamHasColor` already decides it, `NO_COLOR` included. It governs the color
the CLI draws on its own output: help, and the status line. It does not reach
the console, which draws its own screen, or a box's terminal output passed
through `attach`.

What help offers changes too. Each spelling below keeps working; only what
help teaches moves:

| Change | How the old spelling keeps working |
| --- | --- |
| `--auto-start-server`, `--iroh-log`, `--iroh-relay` are hidden from help | They are hidden flags, as `--project` is |
| `ps` is no longer shown as `ls`'s alias | A hidden second registration of `ls`, as `tui` is of `console` (cobra prints every alias) |
| `shell` is renamed `exec` | `shell` is a hidden second registration. The in-box skills teach `exec` |
| `id` moves to `admin id`, with what exists for operating the system (ADR 0112) | `id` is a hidden second registration at the top level |
| `help` is listed, as cobra's `help [command]` | The `no-help` workaround goes: the template lists commands by group, so a hidden command never shows |
| `version` is listed under Other | Unchanged; `--version` still answers too |

`exec` is the word for running a command in a box, and `discobox admin exec`
stays the raw exec group beneath it, as `rm` sits above `admin box rm`.

Target root help (color aside):

```
Discobox runs coding agents in isolated sandboxes on this machine.

Make a discobox with "discobox run":

  discobox run . 'fix the failing tests'
  discobox run -H codex -d . 'fix the failing tests'

Usage:
  discobox [command] [flags]

Basic commands:
  run           Run a prompt in a new discobox
  ls, list      List discoboxes
  attach        Open a discobox: its terminal and what runs beside it
  exec          Run a command in a discobox, or its login shell
  start         Start discoboxes
  stop          Stop discoboxes
  restart       Restart discoboxes
  rm            Archive discoboxes
  rename        Name a discobox
  tag           Set and remove a discobox's tags

Automation:
  send          Send text to a discobox's terminal
  send-key      Send key presses to a discobox's terminal
  capture       Print a discobox's screen and scrollback
  wait          Wait until a discobox is blocked, finished, idle, or exits
  events        Print discoboxes' state changes as they happen

Interface:
  console       Open the discobox console

Files & ports:
  cp            Copy files to and from a discobox
  push          Push local commits to a discobox, for it to rebase onto
  proxy         Forward a discobox's listening ports to local ports
  tools         Run a development tool against a discobox

Resources:
  secret        Manage project secrets
  trust         Manage the hosts discoboxes trust by a pinned certificate

Setup:
  configure     Set the default harness and manage what is saved to each
  admin         Manage advanced Discobox configuration

Other:
  completion    Generate shell completion
  help          Help about any command
  version       Print the client and server versions

Options:
      --color WHEN          set color WHEN to auto, always, or never (default auto)
      --debug               print HTTP requests and the git commands run on this machine
  -h, --help                help for discobox
  -o, --output FORMAT       output FORMAT: table or json (default table)
      --server SERVER       connect to SERVER, an endpoint or a registered server's name
      --token TOKEN         bearer TOKEN for API requests
  -v, --version             print the client and server versions

Use "discobox [command] --help" for more information about a command.
```

### 2. `send`, `send-key`, `capture` and `wait` drive a terminal from the top level

| Command | Does what | Over |
| --- | --- | --- |
| `discobox send DISCOBOX [-t TERMINAL] TEXT...` | types TEXT, the words joined by single spaces, all of it text | the input route, one `text` part |
| `discobox send-key DISCOBOX [-t TERMINAL] KEY...` | presses each KEY in order | the input route, one `key` part each |
| `discobox capture DISCOBOX [-t TERMINAL] [--scrollback N] [--trim]` | prints the screen as text | the screen route |
| `discobox wait DISCOBOX [-t TERMINAL] [--until UNTIL]... [--for DURATION] [--after POINT]` | waits for the first UNTIL to hold | the wait route, called again as needed (§5) |

Common to the four:

- **`DISCOBOX` is required**, the first positional, resolved by
  `selectSandbox` like every other (ADR 26-10-24-254 §1). None picks a box
  when it is left out, as `attach` and `exec` do: a script that drove the
  wrong box because only one was running is worse than one that fails.
- **`-t, --terminal TERMINAL`** names the terminal, `primary` by default,
  resolved by `resolveSandboxExecID` as `admin terminal` resolves its
  `TERMINAL_ID`.
- **`admin terminal` is unchanged**, the raw interface beside `create`, `logs`
  and `rm`. There is no top-level `terminal` group.

`send` delivers its text as a paste when the program asked for bracketed
paste (ADR 0137 §2), so a message with newlines in it stays one message. It
never presses Enter: a message is submitted by `send-key DISCOBOX Enter`.

`send-key` takes key names only, spelled as tmux spells them: `Enter`, `Tab`,
`Escape`/`Esc`, `BSpace`, `DC`, `Up`, `Down`, `Left`, `Right`, `Home`, `End`,
`PPage`, `NPage`, `Space`, and `C-a` through `C-z` (`^A` too), with the API's
own spellings beside them. That is the set `terminalKeyNames` accepts today.
Unlike `admin terminal input`, a word that names no key is refused, never
typed: `send-key mybox Entr` is a typo, not text to send. Its help lists the
names, with examples.

`send` and `send-key` print nothing, as rex's do. With `-o json` they print
the input route's answer, its resume point included.

`capture` without `--trim` prints every row of the screen, blank rows
included, so a script can address a row by its position on the screen.
`--trim` drops the blank rows below the last line written, which is what
`admin terminal screen` prints. Each line's trailing blanks are trimmed either
way, by the route (ADR 0137 §1).

### 3. `wait --until` names what to wait for

`--until UNTIL` is repeatable, and the wait returns on whichever holds first.
It replaces `--hook`, `--idle` and `--exit` on the top-level command (`admin
terminal wait` keeps them):

| UNTIL | Holds when | Served from |
| --- | --- | --- |
| `blocked` | the program reports `blocked`, of any kind | the program's status (§4) |
| `blocked:KIND` | the program reports `blocked` with that kind: `permission`, `question` or `auth` | the program's status |
| `finished` | the program reports `idle`, `done` or `error`: it is no longer working | the program's status |
| `idle[=DURATION]` | no output and no input for DURATION, 5s when left out | the shim's output and input |
| `exit` | the terminal's program exited | the shim |
| `HOOK`, or `hook:HOOK` | a harness hook by its own or its canonical name (ADR 0146), such as `Stop` or `Notification` | the hook collector |

- A program-status UNTIL holds only on a report **after** the point the wait
  counts from (§4). A state that held already does not count: a box that was
  `idle` before it was sent a message is not `finished` until it reports again.
- `finished` includes `error`, since an agent that failed has also stopped,
  and the answer says which state it was (below).
- A kind outside the three is refused, naming them, so a misspelled kind does
  not wait forever.
- A word that is none of the keywords is a hook name. A hook whose name is a
  keyword, or begins `blocked:` or `idle=`, is written `hook:NAME`.
- `finished` and `blocked` mean a program that speaks OSC 7501. Claude Code
  does. A harness that does not reports no status, and is waited on by its
  hooks, `idle` or `exit`, as `wait --help` says. The client does not stand a
  hook in for a status it never sees (see "Alternatives rejected").
- `--until` is required: a `wait` with none is refused.

`wait` prints one line: what held, written as `--until` would write it, then
the resume point last. That is `blocked:question POINT` (`blocked POINT` with
no kind reported), `finished POINT`, `idle POINT`, `exit POINT`, or
`hook:EVENT HOOK_ID POINT`, EVENT being the canonical name where the hook has
one (ADR 0146). It exits 0. `-o json` prints the route's answer, which carries
the program's status record for a status condition and both of a hook's names.

### 4. By default a wait counts from the terminal's last input

**What a wait counts from, unless `--after` says otherwise, is the last input
sent to the terminal through the input route** (`send`, `send-key`, `admin
terminal input`), so `send` then `wait` never misses the reply, however soon
the reply comes and however late `wait` is called.

- The wait route's `until` gains `fromLastInput: true`. The shim stamps
  `sentAt` as the input route writes, before the bytes go in; the sandbox
  agent asks the shim for it, and counts hooks, status reports and quiet from
  it. A terminal never sent input counts from the call, as today. It is
  refused beside `after`.
- An attach's input does not move it. An attach's frames carry what the
  person's own terminal sends unasked, focus-in and focus-out reports and
  answers to the program's queries, as well as their typing. One landing after
  the agent's report would put the point past it, and a wait with no `--for`
  would then wait for a turn that never comes.
- The CLI's `wait` sends `fromLastInput` on its first call, unless `--after
  POINT` is given. `--after` stays for scripts that chain waits.
- `quietSeconds` counts from no earlier than the point the wait counts from,
  rather than from the call (amending ADR 0137 §3). Without `after` or
  `fromLastInput` that point is the call, so nothing changes for a wait that
  names neither. With them, `idle=5s` returns as soon as the terminal has
  been quiet for five seconds since the input, and a wait made again after
  60s (§5) does not restart the quiet period it was halfway through.
- Hook times come from the store's clock and `sentAt` from the shim's, both
  the box's wall clock. A hook the input caused is recorded after the input
  was written.

### 5. The wait route reads the program's status, and stays bounded

- The wait route's `until` gains `programStates`, a list of `{state, kind?}`
  in OSC 7501's own terms. The CLI maps its keywords onto them: `finished` is
  `idle`, `done` and `error`, and `blocked:question` is `{blocked,
  question}`. The server learns no word of the CLI's.
- The shim answers it, in the same call that waits on output and exit
  (`ShimWait`). It sees each report as `Apply` stores it. It matches the
  effective status (ADR 26-10-16-424 §3) after each report whose time is past
  the point the wait counts from. The answer's reason is `program`, with the
  status record. That is a notification, not a poll (ADR 0115).
- **One call still holds at most 60s** (ADR 0137 §3). `wait` with no `--for`
  waits without limit, by calling again with the resume point the last call
  answered, until something holds. `--for DURATION` (a Go duration) gives up
  when it has passed, prints `timeout POINT`, and exits 124. Each call asks
  for at most what is left of it.
- The top-level `wait` has no `--timeout`. The request timeout is the API
  client's, as for every command, and `admin terminal wait --timeout` keeps
  its meaning.
- A wait on a box that stops while it waits fails with the route's error, as
  any command on a stopped box does. Waiting never starts a box (ADR 0137 §2).

Target `wait` help:

```
Wait until something happens in a discobox's terminal, and say what.

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
passes. It prints what held, then the point a later --after resumes from.

Examples:
  discobox send mybox 'run the tests again' && discobox send-key mybox Enter
  discobox wait mybox --until finished --until blocked
  discobox wait mybox --until idle=10s --for 5m || echo timed out
  discobox send-key mybox C-c

Usage:
  discobox wait DISCOBOX [flags]

Options:
  -t, --terminal TERMINAL  wait on TERMINAL (default primary)
      --until UNTIL        wait for UNTIL (repeatable)
      --for DURATION       give up after DURATION
      --after POINT        count only what happens after resume POINT
  -h, --help               help for wait

Global options:
      --color WHEN         set color WHEN to auto, always, or never (default auto)
  ...
```

## Alternatives rejected

**`send` records the resume point on this machine for `wait` to pick up.**
The issue's fallback, if the server could not say when the terminal was last
written to. It can: the shim stamps the input route's writes (`sentAt`). A
local record also misses what it cannot see: a `send` from another machine or
another box. And it is state the CLI would have to keep, per box and
terminal, and expire.

**Any input moves the point, a person typing into an attach included**, as
first decided. An attach carries what the person's terminal sends unasked
(focus reports, query answers) in the same frames as their keys, and the shim
cannot tell them apart. A focus report after the agent's report would hide
that report from the wait, which then hangs.

**`wait` counts from the call, as `admin terminal wait` does.** Then `send`,
`send-key Enter`, `wait --until finished` loses a quick turn, one that ends
before `wait` is called, and waits for a turn that never comes. A wait made to
see the answer to an input should count from that input.

**`finished` means the hook `Stop`, or falls back to it.** ADR 26-10-16-424
removed status derived from hooks, and ADR 0137 left the server holding no
harness's vocabulary. A client-side table is the same table in a different
place. The program's own report is definitive where it exists, and where it
does not the caller names the hook. Mixing the two would also answer
`finished` twice for one turn in a harness that does both.

**`finished` is OSC 7501's `done` only.** The spec's `done` is a result the
person has not seen yet, and `idle` is waiting for the next instruction.
Which of the two a harness reports when an interactive turn ends is the
harness's choice, and either one, reported after the input, is the turn's end.

**A wait held longer than 60s.** ADR 0137 rejected an unbounded hold because
it pins a connection through every hop of the relay, and nothing here changes
that. With `fromLastInput`, the resume point, and quiet counted from that point,
calling again misses nothing and waits no longer than one long call.

**`BOX` as the placeholder, as rex writes it.** Every command takes a
discobox as `DISCOBOX` (ADR 26-10-24-254, cli/DESIGN.md "Command Naming").
These four are no different.

**`send-key` types a word that names no key, as `admin terminal input` does.**
A command named for keys that types a misspelled key's name into the agent's
prompt is a surprise; text has `send`.

**A top-level `terminal` group, or moving `admin terminal`.** The four
commands are the common uses. The rest (`create`, `logs`, `rm`, raw `input`)
is the raw interface, and stays under `admin` with the raw forms of
everything else (ADR 0112).

**`console` among the basic commands, or under Setup.** It is neither a
command that acts on one box nor configuration: it is the interactive
interface, and the group says so.

## Deferred

- **`capture --format text|vt`.** The screen route answers text only, from
  the emulator's cells. A `vt` form, the screen with its colors and
  attributes as escape sequences, needs the route to carry them. Revisit when
  a caller needs to see attributes.
- **rex's layout commands and `new`, `run`, `kill`.** Out of scope:
  `split`, `focus`, `zoom`, `move`, `swap`, `resize` arrange a window
  discobox does not have, and `run` and `rm` already mean what they mean
  here.

## Consequences

- `send BOX '…' && send-key BOX Enter && wait BOX --until finished` returns
  on the reply, including when the agent was already finished before.
- A person typing into an attached terminal does not move the point a later
  `wait` counts from. A script that waits without having sent anything counts
  from the last `send`, and may find a report that came before the person's
  typing.
- For a harness that reports no OSC 7501 status, `--until finished` and
  `--until blocked` never hold. The help says so, and points to a hook.
- `shell` and `id` leave help. Docs, examples and the in-box skills move to
  `exec` and `admin id`; the old spellings keep working, hidden.
- The sandbox agent and shim gain `programStates` and `fromLastInput` in the
  wait, a mixed fleet has old agents that refuse them, and the CLI's `wait`
  reports such a refusal as the route's error rather than guessing.
- cli/DESIGN.md's "Aliases are shown" stands; `ps` becomes a hidden second
  registration instead. Its note that `version` is hidden goes, `version`
  being listed now.
