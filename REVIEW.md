# Review Notes

When changing code, first read the closest `DESIGN.md` and `REVIEW.md` files in
this directory and parent directories.

Global review expectations:

- Keep changes scoped to the package responsibility.
- A new shared root-module package goes under `pkg/`, not at the repository
  root; the root holds only components and `internal` (`DESIGN.md`, root module
  package map). A Go package may sit outside `pkg/` only beside a non-Go file
  it must `go:embed` and that has its one source there (`assets/brand` reads
  `tokens.json`), because `go:embed` cannot reach a parent directory; it holds
  only that file's reader, and anything shared beyond reading it goes in
  `pkg/`.
- Moving a directory: check backticked paths as well as markdown links, in docs
  and comments alike, and check that a backticked `a/b` is a path at all before
  rewriting it. The move to `pkg/` missed `api/model/DESIGN.md`'s backticked
  `../../sandboxconfig/DESIGN.md`, and rewrote the GitHub labels
  `platform/windows` and `platform/macos` because they looked like paths.
- A doc, README or skill names only commands, task targets, shells and
  variables that exist in the commit it lands in; one that does not yet says
  where it comes from and what to use until then. The desktop app's README,
  then its verify recipe, both told readers to `nix develop .#desktop` before
  the flake had that shell, and an agent follows a recipe literally.
  The change that lands what such a doc waits on rewrites it too: the
  recipe kept its workaround after the shell landed until review caught it.
- Changing what a desktop task makes or where (`desktop:snap`'s file names, a
  shell, a flag) rewrites `.agents/skills/verify-recipes` in the same change:
  that recipe has lagged the desktop's build twice, the second time still
  naming `build/desktop/<scene>.png` after the snapshots became
  `<scene>-<dark|light>.png`.
- Terminal content in the desktop app never springs (ADR 26-11-07-363 §1): its
  element sets both `.instant_changes()` and `.motion(Spring::NONE)`.
  `instant_changes` lands layout, opacity and transforms at once but leaves
  colours on the spring they inherit, so without `Spring::NONE` every cell
  background a program repaints fades in.
- A commit with `Tx::color_motion` (the desktop's change of palette) springs
  every colour, terminal content's `Spring::NONE` included: Rune applies it
  over each element's own. So it never shares a commit with anything a
  program wrote — the poll that settles a theme takes no events (`poll`,
  `theme::settle`) — or that output fades in.
- In the desktop app, a session whose result lands late goes to its own box's
  workspace, parked or not, and takes the screen only if the box on screen is
  the one it was asked from (`Pending::Session` carries it) or its own. With
  no workspace for its box, its stream is let go and the session opens with
  that workspace later: never make a workspace for it, which is the screen
  grab this rule prevents. A shell's result once switched to its box wherever
  the person had gone, and parked or released the box they had moved to; the
  TUI learned the same rule as `heldPane` (`cli/internal/tui/REVIEW.md`).
  A session the sessions topic never lists (a pull request's files changed)
  has no later to open with: letting its stream go ends it, so the status line
  says so. The first desktop files changed vanished silently that way.
- In the desktop app, a tab given the window (`Workspace::give`) gives back
  the layout it took only while the layout is still the one it was given:
  every tab put in front (`Workspace::show`, whoever calls it) and every
  `maximize` lets go of it, and a layout change of the person's that does
  not go through either must let go of it too. The first version restored
  on `maximize` alone, so a tool ending after the person had switched tabs
  undid their switch, and the second checked only that the tool was in
  front, so a switch away and back still lost the other column's tab.
- A file the desktop app opens for the person goes to a text editor, never to a
  shell or to the system's file association by its extension alone
  (`sheet::open_in_editor`): a harness file's path is its bundle's choice, and
  a tool file's name its declaration's (a discobox's image or source can
  declare one), so the desktop is only ever handed a copy under a text
  format's extension (`console.EditExtension`, the bridge's `Server.edit`),
  `.txt` for any other. The TUI runs `$EDITOR` itself and edits a tool's file
  in place, whatever its name.
  The first version opened it with `cmd /C start`, so a bundle could name a
  script, or put `&` in the extension, and run it on a Windows machine.
- In the desktop app, whatever draws in front of the workspace takes the keys
  and the commands' context: a page (the harnesses or the secrets,
  `State::page_shown`) sets the list's focus, keeps `pane_focused` and
  `workspace_shown` false, and keys it does not answer never reach `encode`. Its first version left the focus on the panes, so a
  letter typed on the page went to the terminal it covered, and a box command
  acted on that hidden box; the TUI learned it as "what draws in front takes
  the keys" (`cli/internal/tui/REVIEW.md`). Every input path is held to it,
  not only keys: the input method's commits asked less than the keys did and
  typed into a pane behind help, a menu or the page, so the key path and
  `State::ime` ask one question (`State::pane_has_keys`). What is drawn in
  front of the window is one list, `State::overlaid`, which anything asking
  "is something in front" calls: the audit view's reads first kept a copy of
  it, which missed the secrets page's panel added beside the original — the
  third time such a check was copied and missed a surface.
- In the desktop app, a page beside the list (the harnesses page, the
  secrets page, a box's audit view) or a box taking the screen first takes down whatever page is
  up, through `State::close_pages`, never by clearing its own neighbour by
  hand. The audit view's first version cleared the harnesses page as it
  opened, but the harnesses page did not close it: the audit view kept its
  follow and its keys under the page, and Esc on the page brought it back.
  The TUI made the same mistake (`cli/internal/tui/REVIEW.md`, "the audit
  screen is the trap a third way").
- The desktop app's mock (`desktop/src/backend.rs`) sends a stream's bytes as
  the bridge sends them, never reshaped. A service runs on pipes, so its output
  and transcript end lines with a bare line feed; the mock first turned them
  into CRLF, and service panes that stair-stepped on every real box passed
  every test. The pane does the turning (`Screen::newline_mode`), since
  alacritty_terminal applies LNM to NEL but not to a line feed.
- Each console drops a clipboard write (OSC 52) from a box that is not on
  screen — parked, or covered by a page such as the desktop's harnesses page
  (`State::workspace_shown`) — then and when it is back: the TUI's window
  (`cli/internal/tui`, `tree.go`), and the desktop app's
  (`desktop/src/main.rs`, `take_copies`). A
  box switched away from keeps running, and its copy would replace what the
  user just copied in the box they are in. Both consoles' first versions of
  the feature took it from every box.
- While an image pasted or dropped on a pane is carried into its box, each
  console holds only what is typed or pasted into the pane, and sends it after
  the image's path: never the mouse or the wheel (the TUI's `imageInput`, the
  desktop's `State::type_into` beside `send_to`). The desktop's first version
  held everything `send_to` sends, so a wheel turned while a screenshot copied
  in replayed as arrows after the path, walking a full-screen agent's prompt
  history over it.
- A paste's bracketed-paste markers are stripped until none is left, not in
  one pass, and in one linear scan, not a pass per layer: taking one out of
  `\x1b[20\x1b[201~1~` joins its two sides into another, which ends the paste
  early and runs the rest as typed, and a pass per layer is quadratic in a
  clipboard a box can fill (OSC 52). The desktop's `Screen::paste` keeps
  both; termpane's `stripPasteMarkers` is still one pass (#927).
- Code added just above a function goes above its doc comment, never between
  the two: the comment then documents the new item, and the function has
  none. One change did it twice, to `nonNil` in the desktop bridge's
  `convert.go` and to the app's `debug` in `desktop/bridge/src/lib.rs`.
- Renaming an identifier a doc cites (a function, a type, a field): grep for
  the old name across docs, `REVIEW.md` notes, Dockerfile comments and code
  comments, not only the Go callers the compiler finds. Renaming
  `agentlayer.AgentLayer` to `ExtractAgent` left five citations of the old
  name, a review note among them, pointing at code that was gone.
- Changing how a sandbox is named on screen (`sandboxmeta.Title`,
  `services.SandboxDisplayName`) also updates every place that tells a person
  or an agent the rule, most of them off the change's path: the `SandboxMeta.name`
  and `displayName` descriptions in `api/openapi/server.yaml`, the `Meta.Name`
  comment, `writeSandboxName` in `cli/internal/cli/rename.go`, the comments on
  the console's `windowTitle` and `paneName`, the rename dialog in
  `cli/internal/tui/DESIGN.md`, the in-box `discobox` skill
  (`sandbox-agent/features/core/skills/discobox/SKILL.md`), and the status
  line of every ADR the rule supersedes. The rule changed three times in two
  days, and each change left some of these describing the rule before it.
- The console's rename dialog prefills what the box is called (`askRename`),
  so it must offer the resolved title without the msg, never `Sandbox.Name`:
  the listing's name can end with the program's msg, and a word added to it
  writes that msg into the box's name for good. A change to the naming rule
  left this prefill behind twice.
- Retiring a concept: grep every `DESIGN.md`, `REVIEW.md`, `AGENTS.md` and
  skill under `.agents/skills` for it, and the shared skills in the
  discobox-ai/skills repository, not only the docs on the change's path.
  A doc off that path goes on describing what no longer exists, and an agent
  follows it: retiring harness images left `AGENTS.md`, the pools design and
  several verify recipes naming targets and images that were gone.
- Preserve desired-state reconciliation semantics for orchestrated resources.
- Persist accepted intent and resource changes in the same transaction as the
  reconcile dirty mark that drives them (`MarkDirtyTx`); there is no separate
  job queue.
- Do not let provider/runtime code depend on the public control-plane API DTOs
  in `api/gen` and `api/model`; use provider/domain-owned types at that
  boundary. Pool-local generated client/DTO packages (`pool-agent/api`) are
  not public control-plane API DTOs and may be used where pool-agent HTTP API
  calls or pool-local runtime contracts are the package responsibility.
- Prefer short-lived tokens and explicit key ownership for auth flows.
- A CI job that builds what a tag publishes never reads a build cache that a
  pull request can write. Go trusts what `GOCACHEPROG` returns for a key, and a
  pull request's test code runs with that cache's credentials, so it can plant
  compiled packages a release would link. `.github/actions/task` keeps Depot
  Cache only for `pull_request` events and pushes to a branch, by event rather
  than by ref, since a dispatched release runs off a branch; a release path
  that skips that action has to turn it off itself.
- A Taskfile target the macOS or Windows CI job runs (`ci:darwin`, `ci:test`,
  `release:build`, and what they call) uses only Go, `go tool` dependencies,
  git, and what the runner image ships. Those jobs have no Nix devShell (ADR
  26-11-02-173), so a target reaching for jq, shellcheck, bats or node works in
  a developer's shell and fails, or finds the image's own version, in CI.
- A constructor that starts a goroutine reading into an object takes its own
  claim on that object first. `wstunnel.Client.DialNew` started a tunnel's
  reader before claiming its stream, so a server closing at once left nothing
  to claim, and it returned a nil stream with no error.
- An ADR names, in its `Status` and its index row, every section of an
  accepted ADR whose decision it reverses or narrows, and its "what stays"
  claims are checked against those sections. Read the ADRs that the touched
  code and design docs cite before writing the header: a proposal that
  reversed several accepted decisions without naming them was the most
  common finding in its review (ADR 26-10-16-423).
- The image watcher never rewrites what a published development manifest
  names. It publishes the manifest and `.env` only once a whole pass has
  built, so whatever a pass rewrites before a later build fails stays named
  by the manifest still in force: a bundle layout overwritten in place left
  the server refusing its own platform, the layout holding another digest
  than the manifest named, until a pass succeeded. Name the output by its content and remove a
  superseded one only after the manifest replacing it is out.
- A test server that counts what reaches it must ignore the sandbox-agent's
  port probe (`User-Agent: discobox-sandbox-agent (port probe)`). Every port
  that starts listening is asked `HEAD /` once, and this repository is worked
  on inside a sandbox, so an unguarded counter is a flake that only fires
  there. See `sandbox-agent/ports/probe.go`.
- No file may carry a build constraint that excludes darwin, linux, and
  windows together (`!darwin && !linux && !windows`). gopls opens such a file
  in an `aix/ppc64` view, the first port its list has left, and type-checks
  the whole workspace for aix; the `go-lsp` hook then fails on `syscall.Flock`
  and `modernc.org/libc`. Nothing here builds for another OS, so write the
  darwin, linux, and windows halves and no fallback.
- A `date-time` query parameter that a reader pages by needs
  `x-ogen-time-format: 2006-01-02T15:04:05.999999999Z07:00`. ogen's default
  encodes it with `time.RFC3339`, whole seconds. An upper bound truncated below
  a page's oldest record skips the rest of that second; a lower bound truncated
  below a page's newest re-reads the page, and when one second holds more than
  a page the reader never gets past it (the audit lists' `until` and `since`).
- An ordering a client recomputes from what the API returns uses only the
  precision the API carries. A `date-time` in a body is whole seconds (ogen's
  RFC 3339); `pkg/bindingplan` once ordered bindings by the stored
  nanoseconds on the server and by those whole seconds in the CLI's preview,
  so two bindings made within one second were kept one way and previewed the
  other.
- A binary embedded from a gitignored `artifacts/` directory (the server's
  guest relay, the Linux CLI's clipboard helper) builds without it by design,
  so a fresh checkout compiles. That makes a release without it silent:
  every such artifact gets a check in `release:verify` that the release
  binary's own architecture has one.
- Update package-local design docs when changing architecture or data model.
- A machine a hosted provider creates for discobox is created with what the
  provider gives a machine by default switched off: a hosted VM may come with
  the provider's CLI and a token that acts on the account or org, credential
  helpers, and network peers it can exec or copy into, and some of it can be
  set only at create. On boxd a machine that is not `isolated` has all of
  these. A sandbox machine — created by the pool agent — is always isolated,
  on a network shared only with its pool's machine, and its forks and
  restores keep that; the pool's machine is the one exception, because boxd
  peers isolated machines only with non-isolated ones, and it is on that
  network alone. A sandbox machine that kept them could reach its pool's
  machine and the provisioning key there (ADR 26-10-23-551 §4).
- An index row in `docs/adr/README.md` that gains a later ADR's note gets it
  inside its Status parenthesis, before the `)` that closes `Accepted (`:
  appending after that `)` leaves the note outside it and a stray `)` at the
  end, and one change made that mistake on three rows (ADR 26-10-23-551).
- Import `pool-agent/vmtest` only from `_test.go` files, in any module.
  `RunFakeGuestIfInvoked` turns the running binary into disco-vm's fake guest
  agent when its first argument is `guest`, and that agent runs whatever exec
  requests reach its listen address as host processes. In a shipped binary
  (the pool agent, the server) that is a host-exec backdoor one argument
  opens, and nothing in the build refuses the import, so a review has to.
- When the dead-code check flags a function whose last caller went with a
  retired concept, remove everything only that function fed — a type, a field,
  a branch that reads it, a helper only its tests still call — not just the
  function it flagged. The check counts tests and a branch that can never fire
  as callers: deleting only `NewStatusErrorOfKind` left the whole problem-kind
  mechanism and `secrets.Redact` in place, unreachable from anything real
  (ADR 26-10-27-880).
- Retiring a field of a persisted model checks `pkg/` for what only that field
  used: a shared type, its constructor, its comparison and its error stay
  compiling because a test or a discarded return still names them, and the
  dead-code check counts a discarded return as a caller. Retiring a harness's
  platforms left `platform.Set` alive only through `publishedImage`'s return
  its one caller threw away, and the harness's kind left `platform.PlaceKind`
  and `KindMismatchError` comparing a hard-coded `OCI` (ADR 26-11-01-122).
- A skill's (or doc's) claim about what a credential's routes allow or stop is
  checked against what each route actually matches, not against what the
  skill means to do with it: a REST route reads no body, so `PATCH …/pulls/*`
  can close a PR as well as edit its body; a GraphQL mutation's arguments are
  not routed, so `updatePullRequest` closes PRs and every node-ID mutation
  acts on any repository the token can write; `graphql=query` reads every
  repository the token can see; and a git push route is held to a repository,
  never to a ref. A skill once told approvers "nothing that closes is routed"
  over `updatePullRequest`. Say what the routes stop and what they do not,
  and keep repository-scoped writes on REST where REST has them.
- No ADR number or reference in user-facing text: OpenAPI descriptions
  (`api/openapi/server.yaml`, `pool-agent/api/openapi/pool.yaml`) and the code
  generated from them, model `doc:` tags, CLI help, error messages, and console
  text. The spec is the API's documentation and is user facing (owner, PR #549);
  an ADR is history for this repository's developers. Say what the thing does,
  in plain words. ADRs are cited in code comments, `DESIGN.md` and `REVIEW.md`.

- Required real-box smoke tests retain each command's output even on timeout,
  and collect sandbox container logs before purging boxes. Cleanup must not
  overwrite the failure evidence it is meant to preserve. Adoption tests use a
  sandbox-created child and assert a nonempty creator: comparing absent
  creator IDs cannot detect a regression that clears one.
- A restart regression proving a running terminal program survived checks its
  process state, such as a nonce kept only in its shell's memory. Terminal IDs
  survive an in-place relaunch, so an unchanged ID and running status cannot
  distinguish recovery from a replacement program.

- A watcher build deadline stops the whole process tree before cleanup and
  retry. Docker starts buildx as a child, and builder setup is go → task →
  docker: killing only the parent leaves an exporter able to overwrite the
  next attempt's `.partial.tar`. `WaitDelay` only bounds pipe draining; it
  does not kill descendants. Keep a non-exec child exporter in deadline tests.

- A real-box restart regression keeps its fixtures and progress markers off
  `/tmp`, which Docker sandboxes mount as tmpfs and clear on restart. Keep
  markers on the persistent root filesystem, and give a marker written by a
  declared service a directory writable by the sandbox user: the agent's
  `/var/lib/discobox` directory is root-owned.

- Render filesystem paths as escaped URLs when a development shell pins a
  socket address. Raw concatenation truncates paths containing `#` or `?`
  and rejects `%`, disagreeing with the platform endpoint resolver.
- Tests that override the default local socket through XDG or TMPDIR must clear
  `DISCOBOX_SERVER_ADDRESS` before contacting a listener. The Nix shell exports
  it, and a shutdown test can otherwise stop the developer's real server.
- Empty local IPC shorthand must not recursively resolve an address override
  that is itself shorthand. Parse that default once and reject a missing path.
- Text a box wrote that the desktop app hands a platform surface is escaped
  for that surface's own syntax, not only a terminal's. A Linux
  notification's body is markup (`<a href>`, `<b>`), and its first version
  sent a request's name and a box's title raw: a box could put a link in a
  notification signed "discobox" (`desktop/src/notify.rs`).
- A desktop sheet that hands something out is never answered by a stray
  Enter. A request's card opens with the keys on no action, Enter on a field
  moves nowhere, and only an action the keys were moved to (or pressed) is
  taken; its first version opened on Accept, and a doubled Enter ran a part's
  card straight through the review (`desktop/src/sheet.rs`).
- Nor is a question answered, in either console, by a key typed for what was
  in front a moment before. A question's own keys (y, n, a choice's number or
  letter) are taken only once it has been up, and the keys still, the core's
  `AnswerQuiet` (`console.AnswerSettled`, in the spec as `answerQuiet`): the
  git credential question a create asks follows Return in the composer, and
  its first choice, a digit, lends this machine's credentials to the pool.
  Time it from when the question is first drawn; count every key that types,
  before it and under it (a field's typing included: someone who has not
  looked up yet types on into it), but no key that only moves (arrows, Tab,
  Home, End, page keys); once it has settled keep it so; and say on the card
  when a key was held (`console.AnswerHeld`), since the person presses it
  again after a pause. Counting only the typing the question came up under
  was tried and let typing begun under it answer; counting the arrows shut
  out an arrow before the answer. The owner chose this rule
  (`desktop/src/sheet.rs`, `cli/internal/tui/model.go`). A click on an answer
  is not held.
