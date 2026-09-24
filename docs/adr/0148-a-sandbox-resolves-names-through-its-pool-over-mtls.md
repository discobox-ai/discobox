# 0148 — A sandbox resolves names through its pool, over mTLS

- **Status**: Accepted
- **Date**: 2026-09-24
- **Relates to**: [ADR 0013](0013-local-linux-pools-use-libkrun-microvms.md),
  the internal network that makes the pool proxy a sandbox's only way out;
  [ADR 0130](0130-an-audit-record-is-read-where-it-was-written-and-names-its-attestor.md),
  whose rules the DNS trail follows as one more pool-attested trail;
  [ADR 0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md) §2,
  the sandbox's client certificate as its identity to the pool.

## Context

A sandbox is attached to its pool's internal Docker network and nothing else,
so its only route off-box is the pool proxy. Docker's embedded resolver
answers container names there, but a container attached only to internal
networks has no external name forwarded for it: every public name is
`SERVFAIL`. Anything that resolves a name before dialing — an SSRF check, a
health probe, `getent`, `nslookup` — fails inside a sandbox even though the
proxy would reach the host.

Verified on Docker 26.1 (Debian's `docker.io`, which the VM image and
DigitalOcean pools run) and 29.8: a container on an internal network started
with `--dns <address>` has its embedded resolver forward every name it cannot
answer to that address, from inside the container's own network namespace,
and still answers container names itself.

Two facts rule out answering sandboxes with plain DNS from the pool. A
container's DNS server is fixed at create, while the pool's address on the
sandbox network is Docker's to reallocate. And every sandbox is root in a
privileged container on the same segment as the pool, so any of them can claim
the pool's address at any time; the pool proxy and the credentials endpoint are
safe from that only because both are mTLS.

## Decision

1. **The pool answers DNS over TLS.** The proxy unit serves RFC 7858 DNS on
   `0.0.0.0:17085` with the same server certificate and required per-sandbox
   client certificates as the proxy and the credentials endpoint, answering
   each message from the pool container's own resolver.
2. **Every sandbox container is created with the DNS server `169.254.53.53`**,
   and the sandbox claims that address on its own loopback, dropping anything
   sent to it that did not arrive there: a local address answers on every
   interface, and a neighbor routing it through this sandbox would otherwise
   be asking the pool under this sandbox's certificate. Docker's embedded
   resolver stays the sandbox's `nameserver`, so container names resolve as
   before; everything else it forwards to that address, which is local.
3. **A stub in the sandbox's proxy bridge carries those queries to the
   pool.** The bridge already reaches the pool with the sandbox's keypair, so
   DNS is one more thing it carries rather than a process of its own; a flag
   enables it, set for the sandbox's instance and not the nested-Docker one.
   It listens on the claimed address, verifies the pool's certificate for
   `discobox-pool-proxy`, presents the sandbox's, and holds a fixed number of
   connections under the pool's per-sandbox admission, queueing a burst rather
   than failing it. The pool stages both settings in `bridge.json`, beside the
   credentials endpoint that uses the same keypair.
4. **The pool bounds connections before it knows who is asking** — in total
   and per source address, at accept — and per sandbox once the handshake
   names it, because its listener shares a process with every sandbox's
   egress.
5. **Every query is audited, as a trail of its own beside the sandbox's HTTP.**
   The pool's DNS server decodes each exchange — name, type, rcode, the
   answers' addresses and targets, or why there was none — and records it
   through the proxy's recorder, into the same database, retention and client
   identity as the HTTP trail, but not the same queue: lookups have one of
   their own, written only when the HTTP and SOCKS queue is empty, with a
   budget per sandbox. It is read the way ADR 0130 reads that
   trail: where it was written, relayed by the pool agent under `audit:read`
   with the sandbox in the token, fanned out and merged by the server with a
   pool that cannot answer named, and followed by a write-ordered cursor per
   pool. Its records are `dns_<row>`, a prefix of their own so neither trail's
   cursor can be read as the other's. `discobox admin audit dns` lists it,
   `audit get` reads one by ID, and `audit list` includes it when `--source`
   names it; it is attested by the pool.

## Alternatives rejected

- **Plain DNS to the pool's address.** The first version of this decision. A
  stale address could land on a neighbor sandbox, and even a current one can
  be claimed by a neighbor, which would read and answer every query.
- **Plain DNS to a fixed address the sandbox routes and DNATs to the pool.**
  The second version. It follows a pool that moves, but the check of where the
  pool now is was itself a lookup that leaked to the old address while the pool
  was away, and a neighbor claiming the pool's address still wins.
- **DNS through the HTTP proxy (DoH to an intercepted host).** Also mTLS, but
  it puts every name lookup through the MITM stack and its audit writer for no
  gain over a dedicated listener like the credentials endpoint's.

## Consequences

- A lookup another sandbox can see or answer no longer exists; DNS has the
  proxy's trust model.
- DNS for external names needs both sides: a sandbox created by an older pool
  has no DNS server set, and a sandbox image without the stub sends to an
  address nothing listens on. Neither loses container names.
- The pool's own name reaches the stub only when Docker cannot answer it — the
  pool is off the network — and the stub answers it `SERVFAIL` rather than
  dialing, which would need that same name.
- The upstream is the pool's own resolver, so a sandbox can resolve the names
  of containers on the pool's egress network — on a provider that sets
  `network`, possibly the server's. It still cannot connect to them.
- DNS is a way out of the sandbox that bypasses the proxy's allowlist. The
  allowlist is not enabled on any pool today, so this matches the policy HTTP
  already has. It does not bypass the audit: every query is recorded, up to
  the sandbox's budget.
- A sandbox holds its own client key, so it can skip the stub and send the pool
  lookups as fast as it likes. The budget (50 a second, bursts of 500) makes
  that cost only its own trail: lookups past it are dropped from the audit and
  counted, never the HTTP or SOCKS rows, which are always written first, and
  never another sandbox's lookups. The count is the proxy's `/audit/dropped`
  (`dnsDropped`), which is pool-local and not relayed, so a trail thinned this
  way does not say so on the read.
- Requests made through the proxy do not appear in the DNS trail, because the
  proxy resolves those names itself; the trail is what software in the sandbox
  resolved on its own.
- Containers inside a sandbox on its nested Docker's default `bridge` still
  resolve nothing: that daemon drops the loopback `127.0.0.11` it inherits and
  falls back to public resolvers the sandbox cannot reach. Its user-defined
  networks forward through the sandbox's resolver and work.

## Deferred

- **Reporting dropped lookups where the trail is read.** Revisit if a DNS
  trail is relied on as complete: the pool agent would relay the proxy's drop
  counts, per sandbox, beside the rows.
- **Filtering DNS.** Revisit when a pool enables the proxy allowlist. The
  server already decodes each query and knows the sandbox from its
  certificate, so the check is the allowlist's, applied to the name.
- **The nested default bridge.** Pointing the nested daemon at the stub's
  address would cover it. Revisit when a default-bridge container in a sandbox
  needs to resolve a name.
