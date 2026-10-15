# 26-10-15-760 — Nested Docker containers resolve through the sandbox's stub

- **Status**: Accepted
- **Date**: 2026-10-15
- **Supersedes**: [0148](0148-a-sandbox-resolves-names-through-its-pool-over-mtls.md)
  §2's "dropping anything sent to it that did not arrive there", its
  consequence that default-bridge containers resolve nothing, and its deferred
  "The nested default bridge"; the rest of 0148 stands.

## Context

ADR 0148 gives every sandbox container the DNS server `169.254.53.53`, which
the sandbox's stub claims on `lo` and carries to the pool over mTLS. It drops
everything for that address that does not arrive on `lo`, because every
sandbox of a pool shares one internal bridge with inter-container traffic on:
a neighbor could route the address through this sandbox and have its queries
asked of the pool under this sandbox's certificate, in its trail and budget.

The sandbox's nested Docker was left out. Its default `bridge` network copies
the sandbox's `resolv.conf`, drops the loopback `127.0.0.11` it finds there,
and falls back to `8.8.8.8` and `8.8.4.4`, which the sandbox cannot reach, so
its containers resolve nothing. 0148 deferred this "until a default-bridge
container in a sandbox needs to resolve a name". That has happened: software
run in a discobox that starts its own containers (issue #236) had to carry its
own systemd networking bootstrap to get names inside them. Proxy settings and
trust already reach those containers (the runc wrapper, ADR 0020); DNS was the
missing part.

## Decision

1. **The nested Docker names the stub's address.** The image's `daemon.json`
   sets `"dns": ["169.254.53.53"]` (`sandboxconfig.SandboxDNSAddress`; a test
   holds the two together). Default-bridge containers ask it directly;
   user-defined networks' embedded resolvers forward to it. Their queries
   arrive across the nested Docker's bridges.
2. **The stub drops only what arrives on the pool's network.** The rule is
   `-d 169.254.53.53/32 -i eth+ -j DROP`. `eth+` is every interface Docker
   gives the sandbox container (`eth0` on the pool's network, and any further
   network it is attached to). The rest of the sandbox's namespace is `lo`,
   the bridges its own nested Docker made, which only this sandbox's
   containers reach and which may carry any name
   (`com.docker.network.bridge.name`), and whatever the sandbox's root adds
   itself — a VPN's tun, a macvlan. Its peers are the sandbox's own choice,
   and a sandbox could as well forward their queries itself, so the rule does
   not try to refuse them.

An image upgrade replaces the sandbox's container (ADR 0016), and with it the
network namespace that holds 0148's `! -i lo` rule, so no stub ever finds that
rule beside its own and nothing removes it.

The setting is static. Every bootstrap a pool writes names its DNS server
(`proxyagent.PoolEndpoints`), and every sandbox container is given
`169.254.53.53` (`sandboxruntime`), so the nested daemon uses the server the
sandbox itself uses; a sandbox with no stub listening there resolves nothing
either way.

## Alternatives

- **A DNS listener on the nested bridge, beside the nested-Docker proxy
  forwarder.** That forwarder discovers `docker0`'s address after dockerd
  creates it (ADR 0020 §7, no pinned `bip`), but dockerd needs its DNS setting
  when it starts. Pinning `bip` to break the cycle claims the same RFC 1918
  slice in every sandbox at every nesting level, which 0020 already refused.
- **Allowing only `docker0` and `br-+`.** Default-deny, but a user network
  with a custom bridge name, which resolves today through the embedded
  resolver forwarding over `lo`, would lose DNS once dockerd names a server.
- **Rewriting each container's `resolv.conf` from the runc wrapper.** It
  patches a file Docker manages, per container, to undo a setting the daemon
  could have been given once.
- **Rendering the setting only when the bootstrap names DNS.** dockerd takes
  DNS only from `daemon.json` or its command line, so this means replacing
  the packaged unit's `ExecStart` or rendering `daemon.json`, for a case no
  pool produces.

## Consequences

- Nested containers on any network resolve external names through the pool,
  recorded in the sandbox's DNS trail. So do builds the sandbox's own dockerd
  runs (a direct `docker buildx`); `docker build` goes to the pool's BuildKit
  (ADR 0044), which resolves for itself and is unchanged.
- The rule is no longer default-deny: it relies on Docker naming the sandbox
  container's interfaces `eth*`, and on a pool writing a DNS server only into
  the bootstrap of a container sandbox, which today's one writer
  (`sandboxruntime`) does. A sandbox that reaches its pool's network over
  another name — a boxd machine, whose NIC is `enp*` or `ens*`, once a pool
  serves one DNS — must have that name in `dnsstub.pooledInterfaces` first.
- Containers in a user-defined network now forward from their own namespace
  across the bridge rather than from the sandbox's over `lo`. Container names
  on that network still resolve through its embedded resolver; single-label
  names it does not know (`discobox-pool-proxy`, the outer network's
  containers) it never forwarded before either, so none resolved then and
  none do now.
