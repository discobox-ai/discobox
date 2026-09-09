# 0099 — The CLI downloads the server it starts, staged by version and checked by digest

- **Status**: Accepted
- **Amends**: [0066](0066-the-build-is-nix-plus-taskfile-and-github-actions-only-triggers-it.md) §5 — the entitlement belongs on `discobox-server`, which is once again the only process that creates a VM. The rule §5 states, that signing is part of the build and never of packaging, is unchanged.
- **Date**: 2026-09-09

## Context

`discobox` links the control plane. `cli/internal/cli/root.go` imports
`github.com/discobox-ai/discobox/server`, `discobox admin server` calls
`server.Run` in-process, and the autolaunch starts a server by re-invoking the
CLI's own executable with the argv of that command.

Three things follow from that import, and all three are paid by every user:

- **Size.** The CLI is 104 MB on linux/amd64; the server alone is 94 MB. Almost
  everything in a `discobox` download is the server, and most machines that
  install the CLI never start one — they talk to a server somewhere else.
- **The module graph.** `cli/go.mod` carries Docker, buildkit,
  go-containerregistry, gorm with the Postgres and SQLite drivers, and
  `Code-Hex/vz`. None of it is reachable from a CLI command, and all of it is a
  supply-chain surface, a build-time cost, and a reason `cli` cannot compile
  without `server` compiling first.
- **The release line.** The two cannot be versioned apart. A fix in either one
  is a new copy of both, and a CLI cannot be shipped against a server it was not
  cut with.

The separate binary already exists — `server/cmd/discobox-server`, built by
`task build:server`, and what a developer actually runs under `task dev`. The
embedding is a second way to start the same program, kept because there was no
way to *get* the first one onto a user's machine on demand.

That is the gap this closes: the server becomes something the CLI fetches when
it needs it, rather than something it carries in case it does.

## Decision

### 1. The CLI does not link the server

`github.com/discobox-ai/discobox/server` leaves `cli/go.mod`. `discobox admin
server` resolves a `discobox-server` binary and runs it as a child process; the
guest relay, the VM drivers, the database and the registry client are the
server binary's, and no longer reach the CLI's dependency graph at all.

`discobox-server` gains `--version`, which the release build's verification
already assumed every artifact answers, and which is the cheap way to ask a
staged binary what it is.

### 2. `stage` is a command, because it is the half that can be slow

`discobox admin server stage` downloads and verifies, and prints where it put
what it staged. `discobox admin server` stages if it must and then runs. The
split exists so the download can be done deliberately — before a flight, in an
image build, on a machine being provisioned — rather than only as a surprise in
front of the first command that wanted a server.

Everything that reaches a server on this machine goes through one resolve,
including the autolaunch, so `stage` is not a step anybody is *required* to run.

### 3. A release CLI carries the manifest for its own platform, and only that

The release build links a base64-encoded JSON manifest into the binary:

```json
{
  "version": "v1.2.3",
  "os": "linux",
  "arch": "amd64",
  "command": "discobox-server",
  "assets": [
    {
      "name": "discobox-server",
      "url": "https://github.com/discobox-ai/discobox/releases/download/v1.2.3/discobox-server-linux-amd64",
      "sha256": "…",
      "executable": true
    }
  ]
}
```

One platform's, not every platform's, because no single link step can see every
platform's digest: ADR 0066 §4 fans the release out natively, so darwin's server
is built on the macOS runner and linux's and windows' on the Linux one. Within a
runner the ordering is available and free — that runner builds the server for a
target immediately before it links the CLI for the same target — and a CLI only
ever needs a server for the machine it is running on. A cross-platform stage is
`--manifest`, which is a file, and a file has room for whatever a caller wants.

Base64 because the payload travels as a `-ldflags -X` value through a Taskfile
through a shell, and JSON does not survive that trip in a form anybody wants to
read in a diff.

**Rejected: an OCI image pulled by reference**, the way
`server/providers/guestimage` resolves VM boot artifacts. It is a good fit on
paper — an index selects by os/arch, an image holds many files, and content
addressing is the verification. But `release:images` runs in a job *parallel*
to the one that links the binaries, so at link time no image digest exists yet.
That is exactly why `DefaultPoolImage` is pinned to a tag and not a digest, and
a tag is a mutable name: pinning one would mean the binary carries a location
and trusts the registry, which is not the same as carrying a digest.

**Rejected: a base URL plus a name convention**, deriving
`discobox-server-<os>-<arch>` at runtime. It makes the URL for another platform
derivable, which is worth nothing on its own, because the digest for that
platform still is not. The manifest states both or neither.

**Rejected: a manifest published as its own release asset**, fetched at stage
time. Its digest is not knowable at link time either — it is assembled after
both matrix legs finish — so the binary would carry a URL and no digest, and the
verification would rest on TLS. The whole point is that the digest is in the
binary.

### 4. A manifest lists assets; one of them is the command

The server is one file today. Nothing about the design says it stays one — a
helper binary, a firmware blob, a bundled frontend — and discovering that later
is how a downloader grows a special case for "the other file". So the unit is a
list from the start, each entry with its own name, URL and digest, and
`command` names the entry to execute. An asset says whether it is executable;
the rest are staged 0600 beside it.

### 5. Staging is by version, verified while it is written, installed by rename

A staged set lands in `<state>/discobox/server/<version>/`, a sibling of the
CLI's own `<state>/discobox/cli`. By version, so an upgrade stages beside what
it replaces rather than over it: a rollback is a directory that is still there,
and a server that is running out of its directory is not being overwritten
underneath itself.

Each asset is hashed as it is written and compared against the manifest before
anything is kept, and the whole set is downloaded into a temporary sibling
directory that is renamed into place once every digest matches. So the directory
either does not exist or is complete and verified; an interrupted or corrupted
download can never be mistaken for a staged version, which is the failure that
matters — it would be executed.

The manifest is written into the staged directory as `manifest.json`. It is what
makes the directory complete, and it is the record of where the contents came
from, which is otherwise unanswerable once the bytes are on disk.

Re-verification on every use was rejected: it is a hash of ~100 MB in front of
every command that autolaunches, to defend a directory under the user's own
state root against the user. `stage --force` restages and re-verifies for
anybody who wants it.

### 6. Resolution order: an explicit override, a sibling, then the staged set

1. `--binary` / `DISCOBOX_SERVER_BINARY`, used as-is.
2. `--manifest` / `DISCOBOX_SERVER_MANIFEST`, staged. An explicit manifest is
   an instruction as much as an explicit binary is, so it outranks step 3.
3. `discobox-server` in the same directory as the running `discobox`.
4. The staged set for the embedded manifest, staging it if it is not there.

Step 3, the sibling, is what keeps a development build working with nothing
configured:
`task build` writes `build/discobox` and `build/discobox-server` side by side,
and a build that has no embedded manifest — every build that is not a release —
would otherwise have nothing to run. It is also how a distribution that ships
both binaries in one package avoids a download it has no use for.

**`PATH` is deliberately not searched.** A directory mate of the executable is
no more attacker-controlled than the executable itself; a `PATH` entry is a
different claim entirely, and "the server got replaced by something earlier in
`PATH`" is not a failure mode worth having in exchange for a convenience nobody
asked for.

A build with no embedded manifest, no override and no sibling fails saying so.
It does not fall back to a network guess.

### 7. The autolaunched process is the server binary

`endpoint.EnsureRunning` is given the resolved server path directly, with no
arguments. The CLI no longer re-invokes itself, so `App.serverLaunchArgs` and
the command-tree walk it does are deleted — that trick existed only to keep a
hand-written `[]string{"server"}` from going stale, and there is no argv to keep
in step any more.

What follows from it: the systemd user unit, the process the launcher watches
for an early exit, and the log in `endpoint.ServerLogPath` all describe the
server itself rather than a wrapper standing in front of it.

### 8. Signatures are deferred, and the condition is a signed release

The digest is the trust root, and it lives in the binary that checks it — which
is worth exactly as much as the binary is, and the binary is what the user
already chose to run. That is a real ceiling: it authenticates the download
against the CLI, not against Discobox.

Revisit when releases are signed at all. Today nothing is: macOS gets an ad-hoc
signature, which asserts integrity and no identity, and no other platform is
signed. When there is a Developer ID or a Sigstore identity to verify *against*,
the manifest gains a signature over it and staging verifies that instead — at
which point a manifest fetched at stage time becomes possible too, and §3's
rejections are worth reopening.

## Consequences

- The CLI is 37.9 MB on linux/amd64, down from 104 MB. Docker, buildkit,
  go-containerregistry, gorm and its drivers, and `Code-Hex/vz` leave
  `cli/go.mod` entirely.
- A first `discobox run` on a machine with no server now downloads one. It is
  reported on the same status line that already narrates the launch, and it
  happens once per version. Resolution is therefore lazy:
  `endpoint.LaunchOptions.Command` became a func called only when a server
  actually has to be started, so a machine whose server is already running — or
  a development build with no server to download — is not made to pay for, or
  fail on, finding that out.
- An air-gapped or offline install needs `discobox admin server stage` on a
  machine that has a network, or the two binaries installed side by side (§6),
  or `--binary`. There is no third mode where the CLI silently runs an old
  server it happens to have.
- The macOS entitlement moves with the code that needs it. `discobox-server` is
  signed on the macOS runner as before; `discobox` no longer creates VMs and is
  no longer signed with `vz.entitlements`. The signature is inside the Mach-O
  and survives the download, and nothing marks the file quarantined, because
  quarantine is applied by the frameworks that download files on a user's
  behalf and not by a program writing a file it fetched itself.
- The release publishes five more assets, one server per platform the CLI is
  published for, and `release:verify`'s guest-relay check moves onto them:
  the relay is embedded in the server, so checking the CLI for it was already
  checking the wrong binary and would have started passing vacuously.
