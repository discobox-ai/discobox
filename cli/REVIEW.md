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

- **A role a box named is never shown without what it grants.** A credential
  request's role names, descriptions and arguments are the asking discobox's
  words (ADR 26-11-02-022 §3): wherever one is drawn, `requestrole.Summary` of
  its routes is drawn beside it, roles are grouped by digest (never by name),
  and the box's text is escaped like a justification. A label shown alone is
  a box describing its own access to the person approving it.
- **Escape the form of a line that is drawn.** A line kept in two forms —
  its text, and the tokens or spans it is drawn from — is escaped in the form
  the screen draws: the policy rendering's tokens and notes, not only its
  text, and a raw JSON view through `writeTerminalSafeJSON`, not an encoder
  that passes C1 controls and bidirectional overrides. Twice one form was
  escaped while the other reached the screen.
- **Bound the work done on text from a box.** A diff, parse or layout run over
  content a sandbox supplied has a cost bound, not only escaped output: a
  line-by-line diff's memory grows with lines times edits, and a settings
  file under the 2 MiB capture limit was enough to exhaust the client.
  Bound the output, not only the input: re-indenting JSON grows by depth
  times values, and a 118 KB document indented to a gigabyte — the same
  mistake, made twice in one function.
- **Compare a harness file's stored and exported forms by content, not
  bytes.** A preview's `previous` is the stored file as the bundle shipped
  it and its `file` is exporter output; laid out differently, a one-setting
  change diffs as the whole file. The first fix compared them byte for byte.
- **Nothing teaches configuring a harness.** A harness is signed in and set
  up inside a discobox, and a box's change is saved with `admin harness save`
  or the console's harness-changes band (ADR 26-11-02-152). Help, a hint or a
  band that sends somebody to a setup out here is the model that was removed.
  Saying what signing in and saving expose — a login typed in a box is
  readable there, a save shares settings that can run commands — is not.
  `admin harness reset --include-overrides` is the one way back to a config
  with nothing but its bundle's files; a plain reset keeps a person's own.
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
  would take what was meant for the discobox whose ID begins that way: `push` the user's source, `exec` their keystrokes.
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
  everything that teaches it moves in the same change. One command is an
  exception to ADR 26-10-23-600 §1: `skills`, moved to `admin skills`, kept
  no root alias, by the owner's call on #490; a reference to `discobox skills`
  is a command that fails, and moves as a flag's does. Skills (`.agents/skills`, `sandbox-agent/features/*/skills`,
  `access/skills`, and the discobox-ai/skills repository), bats tests, harness hints (`harness/*/image.json`),
  design docs and the argv the console builds (`tui/options.go`) move with it. The `run` rename also
  missed, on a first pass:
  - format strings (`"%[1]s new"`, `"%s new %q"`);
  - the bare word in help prose ("By default new opens…").
  The `shell` → `exec` rename missed, on a first pass, the worked scripts in
  another command's help (`tools --help`'s host script, `db shell …`,
  `"$DISCOBOX_CLI" shell …`) and the comments that quote them (`pkg/tools`):
  grep for the old word after `$DISCOBOX_CLI`, after a shell function that
  wraps the CLI, and in every `Long` and `Example`, not only after `discobox`.

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
  (`routesText` escapes its own output; the console's copy is escaped word by
  word in `terminalSafeRoute`, hosts, methods, paths, fields and tools alike). The secret's name beside them was
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
- **A route goes through `secretroute.Parse` and `secretroute.Format`,
  never a parser or printer of its own.** A route converts to and from the
  API's shape only in `internal/apiroute`; a new route field is added there
  and nowhere else. Routes on one line are joined by `; ` (a comma is the
  route's own), and `--route` never splits on commas.
- **Help that describes a route says a host is exact** (`*.h` for beneath
  it) and uses `secretroute.Syntax`/`routeHelp`; `HOST[/PATH]` and "a host covers
  the hosts beneath it" are the routes before ADR 26-10-27-880 §4
  (`command_conventions_test.go` checks).
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
- **A child process that names a server sets `DISCOBOX_TOKEN` outright.**
  Start its environment from `App.childCLIEnv`, never bare `os.Environ()`, and
  say which token it carries: the App's own for the ProxyCommand, none for a
  host tool, whose CLI is authorized by its transport.
  An App aimed at a registered server carries no token (`forServer`), so an
  inherited `DISCOBOX_TOKEN` was given for the primary, and beside a
  `DISCOBOX_SERVER` naming another server it sends the primary's bearer token
  there. The ProxyCommand had this right; the host script's environment, which
  started handing `DISCOBOX_SERVER` (ADR 26-11-03-982), first did not.
- **What the CLI reads back goes to a script, never to an editor.**
  `DISCOBOX_SERVER` and `DISCOBOX_PROJECT` are `--server` and `--project` to
  every `discobox` run with them, and a `.yaml` host tool is usually an editor
  whose terminals inherit its environment for the life of the window, so
  `Remote.Environ` is a script's alone (ADR 0125 §4); a `.yaml` tool takes the
  values as placeholders in its own args.
- **A file this CLI puts into a pool-shared volume, or tells anyone to fetch
  into one, is checked before it runs.** `~/.cache` is shared by every box in
  the pool that runs as the same sandbox uid (ADR 0094 §2 keys it on the uid
  alone, so on a shared pool that can be another person's box), so any of them
  can plant an executable there first. Code here that would run a file from it
  delivers it elsewhere instead (`deliverToolScript`), and help, docs or a skill
  that tell someone to download into it pin the file's sha256, check it, and run
  a checked copy in the box's own home, never the cached file or a link to it;
  an `-x` test proves nothing. The first `tools --help` worked example missed
  this (#513). It governs what this package delivers and recommends; the
  image's own cache volumes that hold programs (`~/.vscode-server`,
  `~/.rustup`, ...) are the image's decision, not this rule's.
- **The console runs a host tool with no terminal.**
  `apiDataSource.RunHostTool` gives it empty stdin and discards its output, so
  anything that documents a host tool's console key (`tools --help`, a skill)
  says it suits a tool that opens its own window; a terminal program runs
  through `discobox tools <id>`.

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

## The managed ssh_config

- **A Host pattern derives from the discobox's ID, never its name.** A
  generated name is neither stable nor unique the way an ID is, and ssh takes
  the first matching block, so a name that spells another discobox's ID would
  quietly reach the wrong one. What a tool is handed as `{ssh.host}` is the
  server-and-project-qualified alias from `sshConfigHost`. Preserve that scope
  through host tools: independent servers can reuse both project and box IDs.
- **Retire route-matching legacy SSH stanzas even for an empty project.**
  Keep the expected proxy route independent of rendered box stanzas: the last
  box disappearing must remove old aliases too, while other servers' files stay.
- **Every write of a project's managed config or known_hosts is restricted
  and locked.** Each is put through `target.restrict` once written — over an
  existing file `os.WriteFile` keeps the ACL an older CLI left, and an
  Included config ssh refuses fails every host in the user's ssh_config — and
  made under `sshConfigLockName`, since a write also re-keys other projects'
  configs and a read-modify-write without it can put back stanzas another
  command just replaced. (The key and its Windows mirror are not locked, since
  nothing reads one back to rewrite it, and each is restricted where it is
  written: the key through `privatefs.Restrict`, the mirror through
  `restrictFile`, ADR 0102 §1.)

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

- Retiring a command must preserve tests for shared live UI behavior. Rewrite
  fixtures using the remaining Git states instead of deleting the whole test:
  removing Apply's state initially removed the workspace-header refresh check.

## A discobox owed its delivery

- **A client decision about delivery keys on `sandboxOwedSource`**, never on
  `awaiting_source` or an inline `SourceDeliveredAt` / `PendingSourcePushes`
  pair. A discobox is owed its delivery while parked, failed, or being
  repaired (ADR 26-11-03-618 §1), and every place that decided by state, or by
  its own copy of the rule, let an ordinary push or a start through to one.

- **Escape decoded API errors before printing them.** Preparation failures can
  carry feature logs from the sandbox. `responseError` escapes generated error
  messages at the CLI boundary; a JSON escape becomes an active terminal control
  after decoding unless the decoded string is escaped again.

## Public CLI reference

- Keep terminal help in a fenced text block when exporting Markdown: shell
  comments and heredocs otherwise become headings and HTML. Generate from the
  command tree without loading user tools or exposing machine paths. User tool
  declarations may contain private names and descriptions.

- File-copy preservation uses nanosecond timestamps on both stat responses and
  metadata requests. Set the canonical time-format extensions and exercise
  generated codecs: plain encoding/json time.Time fixtures hide ogen's default
  whole-second RFC3339 encoding and once let single-file -p lose precision.

- Check single-file copy streams against the stat size before publishing, even
  when HTTP reaches EOF without an error. The pool proxy can finish a chunked
  response cleanly after upstream revocation; runtime verification once showed
  curl succeeding with only 22 MiB of a 32 MiB file. Archive transfers already
  require their final checksum manifest; raw files need their extent checked.

- One-off pool console commands require an explicit exit status. Interactive
  attach EOF means detach, so its default success cannot prove command completion.
