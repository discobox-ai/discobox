# 26-11-12-355 — A machine sandbox's resolver is the DNS stub, and its bridges resolve the pool against the provider's

- **Status**: Proposed (would extend [0148](0148-a-sandbox-resolves-names-through-its-pool-over-mtls.md)
  §2, which gives the stub to every sandbox container as its DNS server, to a
  machine sandbox, which has no container runtime to configure it; would narrow
  [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §9's "the DNS stub sits beside the provider's resolver" for a sandbox whose
  pool asks for it; would narrow
  [26-10-29-481](26-10-29-481-a-machine-sandbox-and-its-pool-reach-each-other-at-their-private-urls-over-tls-the-pools-ca-issues.md)
  §2's "resolve through the provider at each dial" to the provider's resolver
  the sandbox recorded, still at each dial)
- **Date**: 2026-11-12
- **Relates to**:
  [26-10-15-760](26-10-15-760-nested-docker-containers-resolve-through-the-sandboxs-stub.md)
  (the nested daemon names the same stub);
  [26-10-31-206](26-10-31-206-a-local-vm-pool-runs-its-agent-in-a-linux-vm-the-server-runs-and-its-sandboxes-reach-it-only-over-vsock.md)
  §4 (the stub is the resolver of a box with no network device, which has no
  provider's resolver to keep);
  [26-10-23-719](26-10-23-719-a-sandbox-may-reach-its-pool-through-a-websocket-tunnel-with-its-mtls-inside.md)
  (the tunnel through an edge, whose host a bridge also resolves);
  [26-10-27-345](26-10-27-345-a-machine-sandbox-is-made-again-in-place-and-its-bootstrap-names-where-its-agent-listens.md)
  (a machine's bootstrap is placed again at every start). Issue #456.

## Context

ADR 0148 §2 makes the DNS stub (`169.254.53.53`) every sandbox's resolver by
creating its container with `--dns` (`pool-agent/sandboxruntime/runtime.go`,
`DNS: []netip.Addr{proxyagent.SandboxDNSAddress}`); Docker's embedded resolver
forwards what it cannot answer to the stub, which carries it to the pool over
mTLS. A machine sandbox (boxd) has no container runtime to do that. Its stub
runs (`sandbox-agent/dnsstub`, in `discobox-proxy-bridge.service`'s
`proxy-bridge --dns`) and answers when asked, but `/etc/resolv.conf` names the
provider's resolver, and 423 §9 says provisioning never displaces it.

On boxd that resolver (`10.40.0.1`) answers `REFUSED` for every name
(#456, `sbx_yhg7pcs4by9bmsq5`, 2026-11-02). So everything that resolves a name
before it dials fails: `getent`, `ssh`, `ping`, a server with no proxy
settings, an SSRF check. HTTP(S) through the proxy works only because the
proxy resolves on the pool. Pointing `resolv.conf` at the stub by hand made
external names resolve, and something rewrote the file back seven minutes
later with no restart.

Two facts constrain the fix:

- **The bridges reach the pool by a name the provider answers.** A machine's
  bootstrap carries the pool's provider host (`<pool>.boxd`,
  `pool-agent/poolhop.go` `resolvePoolHop`, `endpoints.Address`), and
  `proxy/bridge.Dialer` dials it with a plain `net.Dialer` (`pkg/wire`
  `Endpoint.DialContext`), so through libc's view of `resolv.conf` (481 §2).
  A pool behind an edge is reached at the edge's host the same way. The stub
  answers `SERVFAIL` for exactly those names (`dnsstub.New`, `ownNames`:
  the dialer's server name and transport host), because asking the pool for
  the name that reaches the pool cannot work. With `resolv.conf` naming the
  stub alone, every bridge — egress, credentials relay, nested Docker,
  BuildKit — would lose the pool.
- **Something in the guest rewrites the file**, and what is not yet known.
  It is found in the one live boxd session (#1050); an Archil guest's
  behaviour is found separately.

The owner's decision on #456 (2026-11-12): make a best effort to point a
machine sandbox's `resolv.conf` at the stub; if the provider makes that
impossible, nothing breaks, so document the limit and stop there.

## Decision

### 1. The pool asks a sandbox to make the stub its resolver

`sandboxconfig.PoolEndpoints` gains a flag, set by the pool in the sandbox's
bootstrap: **the stub is the system resolver**. A machine pool sets it on
both of 481 §2's forms, direct and through an edge; a Docker pool never does,
since `--dns` already does this inside the container and its
`resolv.conf` is Docker's. The intake carries it into `bridge.json` beside
`dnsServer`, and it is meaningless without one.

A pool's setting rather than a guess in the guest: a machine sandbox and a
container look alike from inside, and the pool is the party that knows which
runtime made the box. A machine takes it at its next start, which places its
bootstrap again (345), so a running sandbox is not touched by an upgrade.

### 2. The bridge records the provider's resolver, then rewrites the file

The proxy bridge running the stub (`--dns`) owns the change, since the stub
it points at lives in that process. Once the stub is listening:

1. It reads `/etc/resolv.conf` (following a symlink) and, if the file names
   any nameserver other than the stub, **records it on disk** — the file's
   content and whether `/etc/resolv.conf` was a symlink and to what —
   atomically and synced, outside the directory the intake manages
   (`/etc/discobox/proxy/`, whose unknown files the intake removes). Not in
   `/run`: a machine whose `resolv.conf` still names the stub after a reboot
   must still find the provider's resolver.
2. Only then it **replaces `/etc/resolv.conf` with a regular file** naming the
   stub as the only `nameserver`, keeping the provider's `search`, `domain`
   and `options` lines. A symlink is replaced, not written through: the file
   it points at belongs to the provider's resolver manager.

If `/etc/resolv.conf` already names only the stub, an earlier run of the
bridge wrote it, and the record it made stands.

### 3. A bridge resolves the pool's host against the recorded resolver

`bridge.Dialer` resolves its transport host — the provider's host an https
URL's name stands for, or an edge's host for a tunnel — **against the
recorded nameservers directly**, with Go's resolver dialing them, never
through libc or `/etc/resolv.conf`. It still does so at every dial, so an
address that moves is still followed (481 §2). With no record (a Docker
sandbox, a pool that does not set §1's flag) it resolves as today.

This lives in the dialer because every bridge process uses it, and none of
them may depend on the stub's process being up to reach the pool. The stub
keeps answering `SERVFAIL` for the pool's own names, to every other program.

### 4. A rewrite by the provider is taken back, and teaches the new resolver

The bridge watches `/etc/resolv.conf` (its directory, since a rename replaces
the inode). When the file stops naming the stub alone, something in the guest
has written its own resolver: the bridge **records the nameservers it now
names** as the provider's resolver, as §2 does, then **writes the stub back**.
The provider's rewrite is how a resolver that changes (a DHCP renewal, say)
reaches the record, so the bridge follows it rather than fighting it blind.

Reclaims are rate-limited and logged. Retrying without a limit against a
manager that rewrites at once would be a busy loop. The limit's numbers are
left to implementation.

### 5. When the provider wins, the box keeps the provider's resolver

If the file cannot be written (a read-only mount, an immutable file, a
permission the provider withholds), or the rate limit trips, the bridge
**logs it once, leaves the provider's file as it is, and keeps serving the
stub** at `169.254.53.53` for a program that asks it by address. Nothing else
changes: egress through the proxy is unaffected and the bridges still reach
the pool, because §3 reads the record. This is the limit the owner accepted,
and the provider's documentation says it.

### 6. Turned off, the file is given back

A bridge whose `bridge.json` no longer sets the flag, but which finds a
record, restores `/etc/resolv.conf` from it (the symlink if it was one) and
removes the record. A downgrade, or a pool that stops asking, leaves the
machine as the provider made it.

## Alternatives rejected

- **Stub first, provider's resolver second in `resolv.conf`.** No bridge
  change: libc and Go both move to the next server on the stub's `SERVFAIL`
  for the pool's names. But they also move on for every other name the pool
  answers `SERVFAIL` or is slow on, so those lookups go to the provider's
  resolver, outside the pool's audit and the trust model 0148 gave DNS. Where
  a provider's resolver answers, it is a second way out for names.
- **Redirect port 53 to the stub with a DNAT rule, leaving `resolv.conf`
  alone.** It survives any rewrite, but it does not do what the owner asked:
  the file still names a server that is not the one answering, which is the
  confusion #456 reports. Every query to any server, including an explicit
  `dig @<server>`, is taken over. And the bridge's own lookups need an
  exemption by owner or mark, under a root that shares both.
- **The stub answers the pool's own names from the recorded resolver.** One
  place resolves everything, but every bridge's dial then depends on the
  stub's process being up and answering, and the provider's answer for the
  pool host is served to every program in the box.
- **Make the file immutable (`chattr +i`) or bind-mount it read-only.** It
  holds against a rewrite, but a resolver that changes never reaches the
  record, and a provider's own tooling that fails to write it may fail in ways
  that are not ours to know.
- **Configure the provider's resolver manager** (a `systemd-resolved`
  drop-in, `dhclient` `supersede`, cloud-init's `manage_resolv_conf`). That is
  the fix that does not fight the guest, but it is specific to a distro and a
  provider, and discobox does not own the guest's init (423 §9). Deferred
  below, once it is known what rewrites the file.
- **Pin the pool host's address in `bridge.json` or `/etc/hosts`.** Rejected
  by 481 already: an address fixed at create is wrong once the pool's machine
  moves.
- **Leave it to the provider, and document it.** The owner's fallback, not the
  first choice: a sandbox whose names do not resolve breaks ordinary tools.

## Consequences

- On a machine sandbox, names resolve through the pool, as in a container:
  `getent`, `ssh` and a program that resolves before it dials work, and every
  lookup is audited by the pool (0148 §5).
- `discobox-pool-proxy`, the pool's provider host and an edge's host still
  resolve to `SERVFAIL` for a program in the sandbox; the bridges reach them
  without asking the stub. Names the pool's proxy answers itself (for example
  `assets.discobox.internal`) still do not resolve through the stub. Neither
  is new; both are as the pool's DNS answers them.
- While the bridge is down, nothing in the sandbox resolves. Nothing reaches
  the network either, since the bridge is the only way out, so no working
  path is lost.
- The sandbox's root can undo any of this, as it can everything in the box.
  The resolver is not a fence: the egress fence a driver sets (481 §4) is.
- Losing the record (a person deletes it) while the file names the stub
  leaves the bridges unable to resolve the pool until the provider rewrites
  the file, which §4 then records.

## Open, for implementation

- Where the record is kept, and its format.
- The reclaim rate limit's numbers (§4).
- Whether a box whose provider won (§5) reports it anywhere but its log.

## Deferred

- **Configure the provider's resolver manager instead of fighting it.**
  Revisit when the live boxd session (#1050) or Archil's check finds what
  rewrites the file and §4 reclaims it often enough to matter.
- **A box with no network device** (206 §4) makes the stub its resolver with
  no provider's resolver to record. If it is built on §1's flag, §§2–3 find
  nothing to record and resolve nothing; that runtime needs no more than the
  rewrite.
