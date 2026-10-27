# cli — review rules

## Writing to the screen

- **Work that runs under a full-screen program may not choose a stream.** A
  create runs inside the console's window, and `discobox run` runs one step
  ahead of the terminal it is about to attach. Anything either of them calls
  that has something to say takes a `noteFunc` (`statusline.go`) and reports
  through it; it never takes a `*cobra.Command` in order to reach
  `ErrOrStderr()`, and never reaches for `os.Stderr`. Prefer passing a
  `context.Context` and the sink, so writing to the wrong screen is impossible
  rather than merely discouraged — see `writeProjectSSHConfig`.

- **A line a command prints is its answer; everything else is a note.** The
  discobox `-d` printed, the stanzas `admin ssh-config` emitted, "Created
  discobox …, attaching when it is ready" — those are what was asked for. Which
  key got enrolled, which `ssh_config` got rewritten, what a declared source
  resolved to: those are notes, and where they go is the caller's call
  (`printedNotes`, `statusLine.note`, the window's busy line).

- **Nothing may be left standing above a terminal handover.** A row printed
  before an attach is content the discobox's own full-screen program never drew
  and will not redraw, so everything it paints lands shifted. The status line
  exists for this — it rewrites one row and takes it back down — so narrate on
  it and clear before the stream changes hands.

- **Text a discobox wrote is escaped before it is printed.** A refusal's
  reason, a request's justification, an argv, a hook payload, a prompt, and every ID or name that arrived beside
  them: anything composed inside a discobox goes through `terminalSafe`
  (`audit.go`) on its way to a terminal, and JSON carrying it through
  `writeTerminalSafeJSON`. It is display data, and printed raw a control
  sequence in it is an instruction to the reader's terminal. That includes a
  sandbox command's stderr folded into an error: the console puts the error on
  its status line, outside any pane's emulator. A repository's `.discobox`
  files count too, local or remote: a box can push the commit a create reads,
  and a JSON key or value decodes `\u001b` to a raw escape. Quote what they
  name (`sandboxcreate.quoteKeys`, `%q`) and escape the line it lands in.
  An API error the sandbox agent answered with is that text too: moving a
  step off an exec onto an API call (the files API, ADR 26-10-22-499) moves
  the box's words out of a stderr that was escaped into an error message that
  is not, unless the call's error is (`sandboxFileResponse`).

- **Escape the form of a line that is drawn.** A line kept in two forms —
  its text, and the tokens or spans it is drawn from — is escaped in the form
  the screen draws: the policy rendering's tokens and notes, not only its
  text, and a raw JSON view through `writeTerminalSafeJSON`, not an encoder
  that passes C1 controls and bidirectional overrides. Twice one form was
  escaped while the other reached the screen.
- **The status line owns its row while it is up.** Anything else written to that
  stream goes through `print`, `note`, or `suspend`; written past it, a line
  comes out with the spinner glued to its front.

## Flags

- **A flag's usage backquotes only the name of its value.** pflag reads the
  first backquoted text in a usage as that name and prints it after the flag
  (`--body ID`), so a command quoted that way makes a boolean flag look as if
  it took one: `--policy-as-asked discobox secret request get`. Quote a
  command in usage text with plain quotes.
- **A typed value is a `pflag.Value`, parsed as it is set.** A length of time
  is `lifetime.Flag`, a size `byteSizeFlag`: a `StringVar` parsed in `RunE`
  is the pattern they replaced, five ways for five flags. Its `String` is
  empty until set, so help prints no default; what leaving it out means goes
  in the usage. `Changed` alone is not "given" for a bool a `=false` can
  clear: `listCleared` reads the value.

## Command names and arguments

The rules are DESIGN.md's "Command Naming", and `command_conventions_test.go`
enforces the ones a tree walk can see. These are the ones it cannot:

- **A placeholder is checked against the resolver behind it.** `Use` saying
  `DISCOBOX` promises a name works. `trust ls DISCOBOX` called
  `resolveSandboxID`, which takes IDs only, and nothing caught it. Read which
  `resolve*` the command calls before accepting its `Use`.
- **A DISCOBOX goes through `selectSandbox`** (or `resolveListedSandbox`
  where one server is all there is): the NAME `discobox ls` prints, an ID or a
  short ID, on every server; a full ID nothing lists is taken on the primary
  as it stands, which is what keeps a deleted discobox's audit readable.
  `resolveSandboxID` is the raw `admin box` forms' rule. `selectSandbox` once
  resolved IDs only, so `attach mybox` sent a name to the server as an ID.
- **A listed name never beats an ID-shaped reference.** The box writes its
  listed name (its meta name, its terminal's title), so a box named `01hq`
  would take what was meant for the discobox whose ID begins that way: `apply`
  its commits, `push` the user's source, `shell` their keystrokes.
  `matchSandboxName` refuses a name that is a short ID of another discobox;
  anything that matches a name keeps going through it. The check is only as
  good as the IDs it is given: with a server silent an ID-shaped name is
  refused rather than matched.
- **One-server and many-server resolution are two paths that must agree.**
  `selectSandbox` resolves with `resolveListedSandbox` on one server and
  `selectSandboxOnEveryServer` on several; a rule added to one (the ID-shadow
  refusal, the unlisted full ID) is added to both, through the shared
  `matchSandboxName` where it can be.
- **Use the App `selectSandbox` returns.** It is aimed at the server the
  discobox is on; a call made on the App it was called on goes to the primary.
- **Which positional is which is decided by position and flags, never by
  what an argument looks like.** `run`'s first argument is SOURCE unless
  `--no-source` is given; a SOURCE that does not resolve is an error naming
  the form that works, not a guess that it was a prompt. Only zero arguments
  open a picker. A command that runs a command reads `[DISCOBOX] [--] [ARG...]`
  through `splitDiscoboxCommand`; one that parses no flags (`tools ssh`) has
  to say what a leading `-` means itself, since nothing stops it reaching
  DISCOBOX.
- **`-q` prints what the group's other verbs take back,** not necessarily an
  ID: a remote's name, a harness secret's env name, a skills entry the way
  `skills rm` takes it.
- **A flag removed is removed from what teaches it.** `--discobox-id` was in
  the in-box skills (`access/skills/discobox-access`, `orchestrate.sh`), the
  verify recipes, scripts and the latency harness; a skill a box carries that
  names a gone flag is an agent's first failed command.
- **A flag that "does nothing" is checked before it goes.** `admin exec
  create -i` was called a docker-compatibility no-op; it is what sends stdin
  to the command.
- **A command rename keeps the old spelling as an alias, and teaches only the
  new one.** A flag rename does not: the old flag goes (ADR 26-10-24-254), so
  everything that teaches it moves in the same change. Skills (`.agents/skills`, `sandbox-agent/features/*/skills`,
  `access/skills`), bats tests, harness hints (`harness/*/image.json`,
  `configure.sh`), design docs and the argv the console builds
  (`tui_local.go`, `tui/options.go`) move with it. The `run` rename also
  missed, on a first pass:
  - format strings (`"%[1]s new"`, `"%s new %q"`);
  - the bare word in help prose ("By default new opens…").

  Leave what records history: ADRs and audit argv in tests. A persisted configuration
  key is renamed only with a read path for the old one, as `client.yaml`'s
  `new:` is read as `run:`. Tests drive the canonical
  name. A blind replace of a word hits more than the command, so read every
  replaced line:
  - quoted words, such as dialog actions and fixtures;
  - another CLI's flag of the same name: renaming `run --json` hit a comment
    about `discobox-access request --json`, which kept its name;
  - format strings, as `%d new keys` became `%d run keys` in a test's failure
    message.

- **Changing where flag parsing stops requires auditing argv producers.**
  Scripts and console launchers must put CLI flags before DISCOBOX; otherwise
  those flags become the remote command. Check callers outside Go tests too.

## Secrets and bindings

- **A person's binding verbs print sentences, not IDs.** `secret bind`,
  `unbind` and `bound` say what changed for whom; an ID, a raw record or a
  table of target kinds belongs in `admin binding`. A new field on a binding
  is said in the sentence, or it is raw.
- **A binding's target is `[DISCOBOX]`, `--harness` or `--project-wide`,
  exactly one,** read by `resolveBindingTarget`. A new `secret` verb that names
  a target takes the same three (`command_conventions_test.go` checks).
- **A binding's words are `--as`, `--route` (one per flag) and `--for`**, on
  the person's verbs and `admin binding` alike. `--to`, `--env` and `--ttl` for
  a binding are a second vocabulary; `--ttl` is a token's staleness.
- **A subcommand's flag must not reuse a root persistent flag's name.** It
  shadows the global one on that command: a local `--project` made
  `secret bind … --project acme` stop choosing the project.
- **A variable, route or name a discobox wrote is escaped wherever printed**:
  a request's env name and routes, a child binding's variable and routes
  (`routesText` escapes its own output). The secret's name beside them was
  never the only box-written cell.
- **Unbinding takes back the half asked for.** A binding that both delivers a
  variable and may be given on is updated, not deleted, by an unbind of one
  half.
- **`-s` is a new secret's value only, read from where it is.**
  `KEY[=VALUE|=@FILE][@HOST]` makes an anonymous secret, from `$KEY` or a
  file before a literal value: help and examples lead with those, since a value
  on the command line reaches shell history and the process list;
  nothing in `-s`, `--from-json`'s `secrets` or the console's Secrets row may
  name a stored secret — that is `--bind`. The host is what follows the last
  `@`, and a word with no dot after it is refused, not taken for a host: a
  password's tail is not a route. One with a dot is taken, so every create
  says where each inline secret is sent; a create path that does not print
  `InlineSecretLine` hides a misread `@`.
- **A default variable is `secretenv.Name` of the secret's name, everywhere.**
  `--bind` without `:VAR`, `secret bind` without `--as` and the console's
  binding form must agree; a secret given by ID is looked up first
  (`NameBindingVariables`, `defaultBindingVariable`), never upper-snaked as an
  ID.
- **A secret is created with a route.** The CLI and the console both refuse a
  secret with none and say `*` is anywhere (ADR 26-10-27-939); a route-less
  secret is sent nowhere and fails far from where it was made.
- **A refresh request is dismissed, never denied.** It is its own kind
  (`secret-refresh-requests`); answering one through the credential request
  endpoints is a 404 at best.

## Handing the sandbox's paths to host tools

- **A folder from the server's record is checked before a host tool gets it**
  (`sandboxSSHFolder`): absolute in the sandbox, not a Windows share, no
  control character. A host tool reads it on the far side of an SSH host
  (`ssh-remote+<host><folder>`), where `@attacker.example/x` is another host,
  and the record is not the client's alone: a box can create boxes, and an
  archive is anyone's (ADR 26-10-15-417). Anything new that puts a sandbox path
  into a host-side URL or command goes through the same check.
- **A URL joins a sandbox path to its host through `/`.** `tools.Remote`'s
  `{workdir.urlpath}` always begins with one, a drive path written
  `/C:/src/app`: a Windows path begins with its drive, and `C:\x@host` read as
  authority is another host.
- **A command printed for the user to paste is not guarded yet.** `apply`'s
  next steps put a sandbox directory into `git -C <dir>` unquoted. Quote it for
  the user's shell before adding another such command, and do not treat this
  section as covering them.

## Crossing into Windows from WSL

- **Ask Windows through `internal/wsl`, never with a copy of it.** Detection,
  `wslpath`, finding a Windows program (`cmd.exe`, `powershell.exe`) and
  reading a Windows variable each have a detail a copy forgets — a
  distribution with `appendWindowsPath=false`, a probe that never returns — and
  four copies had drifted into two meanings of "is this WSL" before they were
  folded into one.
- **WSL is the two variables WSL sets, never the kernel's release string.**
  That string says "microsoft" in every container on a WSL2 kernel (Docker
  Desktop, a wslc pool's boxes), where there is no Windows to reach, and the
  CLI's tests clear the variables to stay out of a developer's real Windows
  profile.

## Reading files a paste names

- **The box can write what gets pasted.** OSC 52 puts its text on the user's
  clipboard, and its screen is what the user copies from, so pasted text that
  leads to reading a file on this machine checks what is actually opened — an
  image by name and bytes (`clipimage.ReadFile`), on a local drive — and never
  trusts the text that named it. A `file://` link's fragment, a `.png` symlink
  and a Windows share path all led somewhere else.

- A remote's URL path is decoded text. Render it through `terminalSafe` in
  credential consent, and reject controls before forming Git credential input:
  a declared repository can percent-encode OSC or CSI sequences in its URL.
