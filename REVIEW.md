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
- When a skill's credential ask is rewritten from a use into a policy, carry
  every limit the use said — each "Never …", each "my …" or "this …" — into a
  deny, a `created by $this`, or a condition: once a grant carries a policy,
  the use's words limit nothing (`judge.Job.UnderPolicy` drops them), so a
  limit left only in the use is gone. `internal/skillpolicies` checks the
  sentences parse, not that they say enough.
