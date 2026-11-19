# Review Notes

When changing code, first read the closest `DESIGN.md` and `REVIEW.md` files in
this directory and parent directories.

Global review expectations:

- Keep changes scoped to the package responsibility.
- A new shared root-module package goes under `pkg/`, not at the repository
  root; the root holds only components and `internal` (`DESIGN.md`, root module
  package map).
- Moving a directory: check backticked paths as well as markdown links, in docs
  and comments alike, and check that a backticked `a/b` is a path at all before
  rewriting it. The move to `pkg/` missed `api/model/DESIGN.md`'s backticked
  `../../sandboxconfig/DESIGN.md`, and rewrote the GitHub labels
  `platform/windows` and `platform/macos` because they looked like paths.
- A paste's bracketed-paste markers are stripped until none is left, not in
  one pass, and in one linear scan, not a pass per layer: taking one out of
  `\x1b[20\x1b[201~1~` joins its two sides into another, which ends the paste
  early and runs the rest as typed, and a pass per layer is quadratic in a
  clipboard a box can fill (OSC 52). termpane's `stripPasteMarkers` cuts a
  marker off the output's tail as it forms; sandbox-agent's
  `shimruntime.Runtime.EncodeInput` still strips in one pass (#1292).
- Renaming an identifier a doc cites (a function, a type, a field): grep for
  the old name across docs, `REVIEW.md` notes, Dockerfile comments and code
  comments, not only the Go callers the compiler finds. Renaming
  `agentlayer.AgentLayer` to `ExtractAgent` left five citations of the old
  name, a review note among them, pointing at code that was gone.
- A generator `go tool task generate` runs writes its output through
  `pkg/genfile` (`WriteFile`, or `SyncDir` for a tool that writes a whole
  directory, as `api/internal/genogen` does for ogen), never `os.WriteFile`
  or a tool's own clean-then-write: a rewrite of unchanged content still shows
  the hooks a modified or deleted file and sets off every hook over it. A
  generator not written in Go cannot import it and keeps the same rules
  itself, as `scripts/build-harness-settings.mjs` does (`writeIfChanged`); a
  change to one is made to the other.
- A websocket keepalive must not call its peer dead for a ping left
  unanswered while this end was not reading. coder/websocket reads a pong only
  inside a `Read`, so a reader stopped behind a paused consumer (`| less`) holds
  the pong behind its data. Count a missing pong only while a read was
  waiting, as `websocketAttachFrames.keepalive` in `cli/internal/cli` does, or
  never, as `pkg/wstunnel` does. Check both ends of the socket: the exec
  attach's CLI end got this wrong (#1268) and its sandbox agent end still does
  (#1408).
- A file or tree something trusts after a hard stop (a record, a marker, a
  key, a cache entry, a harness's settings) is staged and renamed through
  `pkg/fsdurable`, in every module, and so is any staged write that syncs at
  all: `fsdurable.WriteFile` for bytes in hand, `fsdurable.Create` (or
  `CreateIn` through an `os.Root`) and `Commit`/`CommitNew` for one streamed,
  chmodded, chowned or checked first, `fsdurable.Symlink` for a link; never
  `os.CreateTemp`, a hand-written `Sync` and `os.Rename`. A staged tree is
  synced before its rename (`SyncTree`, or `SyncDirs` when its files are
  durable already), as is whatever a record or marker vouches for before the
  record. A listing or cleanup of a directory with staged writes in it knows
  `fsdurable.IsStaged` names. A sync that failed is never retried as proof:
  a second fsync after a failed one can succeed over data the kernel already
  dropped, so `File.Commit` keeps a sync failure final and a caller stages
  the write again. The same write-then-rename was hand-written, and
  half of the copies synced nothing, in the sandbox agent, the pool agent, the
  libkrun driver and four `pkg` packages (#1113), and a hard stop of a pool's
  host keeps an unsynced rename and loses its data. A rename that only keeps
  a reader from seeing half a file, of something nothing trusts across a hard
  stop, needs neither. Two staged writes that would otherwise qualify are
  exempt: a directory on memory (`rawsecrets`, which refuses any other), and
  a user's own output under an in-progress name the program reports (the
  CLI's export to `<file>.partial`).
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
  `release:build`, and what they call) uses only Go, `go tool`
  dependencies, git, and what the runner image ships (on Windows, Git Bash).
  Those jobs have no Nix devShell (ADR 26-11-02-173), so a target reaching for
  jq, shellcheck, bats or node works in a developer's shell and fails, or finds
  the image's own version, in CI.
- A check added to CI that reads anything under `docs/` (a Markdown lint, a
  link check, a test of `docs/public`) is added to `ci:docs` too. A pull
  request that changes only `docs/` runs `ci:docs` and skips every other job,
  so a check reachable only from `verify`, `ci:test` or `ci:check` lets such a
  PR pass and fails the next push to `main`. Nothing enforces this.
- A CI job gated on `changes` uses
  `!cancelled() && needs.changes.outputs.code != 'false'` so a failed
  `changes` job still runs the code checks. A skipped `changes` job allows
  main pushes through and skips all jobs on an ignored pull request label
  event.
- A workflow's concurrency group keys eligible pull request runs on the
  pull request number. An event on a merged or closed PR can have the base
  branch as `github.ref`, so grouping by ref alone let a label event cancel
  main's push run (#1271). Ignored label events use a per-run group so they
  cannot cancel active or pending CI.
- A run that publishes is never cancelled: a cancelled one leaves a release
  half made. That takes two things. `cancel-in-progress` is `false` for it
  (or `${{ !startsWith(github.ref, 'refs/tags/') }}` when the workflow's
  other runs should cancel), which stops a newer run cancelling it in
  progress. And it has a group of its own, one per release: GitHub keeps one
  pending run per group and a newer run replaces it whatever
  `cancel-in-progress` says. A tag push is grouped by its ref; `release.yml`'s
  dispatch runs off a branch, so it is grouped by the tag it names.
  `promote.yml` (`release` events) shares one group, which is safe only
  because each run recomputes from the newest blessed release.
  `main-build.yml` shares one group on purpose (ADR 26-11-16-249 §3): a
  newer green commit replacing a pending main build is the coalescing it
  wants, and a pending run has published nothing. Only a run that will build
  may join it; any other (a cancelled or failed CI run, a dispatch off
  another branch) gets a group of its own, or it would replace the pending
  build and then skip, leaving main unbuilt. Its `if` and its group's
  expression state the same condition.
  `test/cichanges` checks the tag-triggered workflows (`on.push.tags`),
  `ci.yml` and `main-build.yml`, not the rest.
- A Taskfile target that must force an environment variable sets it on the
  command (`VAR=value go tool task <target>`), never as a task's `env:`. Task
  lets a variable the caller already exports win over `env:`, so a forced
  `GOPROXY=off` there was silently ignored on any machine that exports one.
- An environment variable a CI step or job exports around `go tool task`
  either is a Taskfile input, named for the variable it sets (`RELEASE_TAG`),
  or is named so no Taskfile variable could share it (`DISCOBOX_CI_…`), or is
  unset before Task runs. Task hands every environment variable to its
  templates, so one named like a task's variable fills it wherever the task
  leaves it unset: the task action's `TARGET` became `release:image`'s
  Dockerfile stage and failed the release's images job.
- A client's patience with something the server bounds comes from the
  server's bound plus a margin (`health.PoolUpgradeWait`), never from a
  constant that happens to equal it: with no margin the client times out on
  the eve of the server's own answer (ADR 0069 recorded the same mismatch the
  other way round).
- A constructor that starts a goroutine reading into an object takes its own
  claim on that object first. `wstunnel.Client.DialNew` started a tunnel's
  reader before claiming its stream, so a server closing at once left nothing
  to claim, and it returned a nil stream with no error.
- The moby client in every module stays at v0.6.3 or later. Before it, a
  hijacked call (`ExecAttach`, `ContainerAttach`, `DialHijack`) used its
  context only to dial, and then waited for the upgrade with nothing tied to
  it, so a daemon that accepted the call and never answered held it for
  ever (#1361). Never work around such a call by racing it against its
  context in a goroutine: whatever is left blocked keeps its goroutine and
  socket for good. The call has to end by itself when its context does.
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
- A test server on a fixed port that counts what reaches it must ignore the
  sandbox-agent's port probe (`User-Agent: discobox-sandbox-agent (port probe)`).
  It is asked `HEAD /` once, or twice if it sends no bytes within a second
  and still listens three seconds later. Undeclared listeners in the ephemeral
  range are listed without a probe. This repository is worked on inside a
  sandbox, so an unguarded fixed-port counter is a flake that only fires there. See `sandbox-agent/ports/probe.go`.
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
  these. A sandbox machine — created by the pool agent — is always isolated
  as its driver carries a pool (`sandboxruntime.PoolFencing`): on a driver
  that fences on networks, stripped and on a network shared only with its
  pool's machine; on one that fences at its provider's edge, holding nothing
  of the account, on no network, its fence closing DNS and its URL answering
  only the edge's credential (ADR 26-11-16-619 §1). Its forks and restores
  keep that. The pool's machine is the one exception, because boxd peers
  isolated machines only with non-isolated ones, and it is on that network
  alone. A sandbox machine that kept them could reach its pool's machine and
  the provisioning key there (ADR 26-10-23-551 §4).
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
- A route a skill tells the agent to ask for names its repository in the
  path (`api.github.com/repos/OWNER/REPO/...`), never only in a query the
  agent writes: a route matches no query, so `GET api.github.com/search/issues`
  searches every private repository the approver's token can read, whatever
  `q=repo:` the skill meant. The issue-reporting section of the `discobox`
  skill once asked for exactly that as a fallback.
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
- A live check of a boundary (`scripts/live-run.sh`) passes only on what its
  probe observed at that boundary. A probe that failed on the checking side
  (the checking box's own egress or DNS refusing it, a timeout there, no curl
  in the guest) proves nothing, and is a failure or a check for a person,
  never a pass: otherwise the run meant to show the boundary holds reports it
  holding untested. No connection passes only where the probe runs inside
  the boundary under test, as the guest's probes around its fence do, and
  then only when the probe shows no connection was made, or that the fence
  refused it: boxd's fence accepts TCP and closes TLS at the ClientHello
  (curl 35), so that refusal passes, and only while an address no server
  answers from (192.0.2.1) is refused the same way. A baseline that can be an
  origin (the allowed host) cannot tell the fence from a fence that forwards
  every connection; and any other failure after a connection (a certificate,
  an empty reply, a reset, a timeout) may be an origin's, never a pass.
- A live check of an inbound path probes where the path is, not only the
  URL the driver reports. A driver stops reporting a URL it has closed, but
  what it closed can still answer there: boxd keeps a sandbox machine's
  default proxy, locked to port 0, at its name in its zone. Probing only what
  `disco-vm url` printed never reached it, and the step could not tell a
  closed proxy from an open one. A proxy's refusal (boxd's 502) passes only
  while the guest listens where an open proxy would forward: an open proxy
  forwarding to a port nothing listens on answers the same 502.
- A test's stand-in for a real component (a stub CLI, a canned probe answer)
  answers what that component sends, read from its code, never what the code
  under test expects. The live run's first stand-ins had the proxy answer a
  denied CONNECT with 403, which it never does (it closes the connection),
  and so passed a check the real proxy would fail.
- A live-run branch keyed on a response header (`x-boxd-proxy`, `server`)
  has a case whose curl stand-in writes that header, CRLF-ended as curl's
  `-D` file is. The stand-in once wrote no headers, so the header branch for
  boxd's bot-protection challenge was never reached in a test, and `server`
  carried curl's trailing CR in every real report unseen.

- A watcher build deadline stops the whole process tree before cleanup and
  retry. Docker starts buildx as a child, and builder setup is go → task →
  docker: killing only the parent leaves an exporter able to overwrite the
  next attempt's `.partial.tar`. `WaitDelay` only bounds pipe draining; it
  does not kill descendants. Keep a non-exec child exporter in deadline tests.

- A watcher test that lowers the Docker deadline (`dockerCommandTimeout`)
  lowers it for every inspection and tag command, not only the one meant to
  stall; builds have their own `buildCommandTimeout`. Set it to seconds a
  loaded runner meets, never milliseconds, and wait on what the watcher
  published rather than reading after one tick. The stall itself outlasts the
  test's own time limit, or a command the deadline never cut off ends on its
  own and passes as a recovery.

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

- Watcher rebuilds use a fresh output on the same filesystem as the live
  executable, then rename after signing. Do not build directly over the live
  path: Go may copy in place, and a truncated file retaining its build ID can
  be accepted unchanged on retry.
- An exec attach (`pkg/execstream/host`) never puts a deadline on a pipe
  stream's output, its closing drain included, and nothing that owns a stream
  closes `Options.Done` under an attach still taking it (the shim waits out
  its attachers after its exit linger): a pipe exec has no repaint, so a
  viewer cut off loses the rest of the command's output and its exit status.
  #1261's first cut did both, with a 30s drain budget and the 60s linger.
  Only a terminal stream's viewer may be detached for being slow, and it
  recovers by reconnecting with a replay. Keep the protocol `Error` frame
  behind the output it ends, too: a client stops reading at it.
- Anything that waits for a stream's attaches to end (the shim's
  `lingerThenFinish`) relies on every attach being told to end: list the
  attachers to send `Exit` and `Close` only after `MarkExited`, so an attach
  that joins meanwhile is either on that list or finds the retained exit frame
  itself. Listing them before the output drained left a late joiner with
  neither, and the shim waiting on it for good.
- `check:ubuntu` (`scripts/ubuntu-pin.sh`) reads Dockerfiles as text, so it
  holds only what it is taught: the mirror list and Ubuntu's sources are
  written by the one shared setup block it compares across every copy, and a
  new way of writing either (another file, a heredoc, a `COPY`) needs the check
  taught to refuse it in the same change. Its first version accepted a stage
  that wrote an https list with `Trusted: yes` outside the block, because any
  mention of the list's path counted as the setup.
