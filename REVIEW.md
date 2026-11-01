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
