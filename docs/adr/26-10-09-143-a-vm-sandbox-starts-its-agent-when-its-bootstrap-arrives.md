# 26-10-09-143 — A VM sandbox starts its agent when its bootstrap arrives, and its backend gives the pool one address to it

- **Status**: Accepted (narrows [26-10-08-127](26-10-08-127-a-sandboxs-bootstrap-is-static-and-the-intake-carries-the-rest.md)
  §2 for backends with no PID-1 flow: the bootstrap is placed after boot,
  before the agent starts; picks [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
  §5's "address the pool can dial" for provider-hosted VMs)
- **Date**: 2026-10-09
- **Relates to**: [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md),
  [0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md),
  [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md),
  [26-10-08-127](26-10-08-127-a-sandboxs-bootstrap-is-static-and-the-intake-carries-the-rest.md).

## Context

Every sandbox today is a Linux container whose PID 1 is
`discobox-sandbox-agent init` (ADR 0007): it reads `sandbox.json` from the
config volume Docker mounted, wires the declared volumes and sources, and
execs systemd. The agent's unit then starts only if `/etc/discobox/sandbox.json`
exists (`ConditionPathExists`).

The VM sandboxes now in reach do not boot that way. disco-vm
(`github.com/discobox-ai/vm`) builds and runs machine images on boxd, a cloud
microVM service, and on Virtualization.framework and Hyper-V for macOS and
Windows guests. The `boxd.yaml` twins of the sandbox images already build the
same toolchain on boxd. On all of them:

- **The guest boots its own init.** boxd owns the kernel and its command line;
  a macOS guest boots launchd and a Windows guest the SCM. Making
  `discobox-sandbox-agent init` PID 1 would need the image to replace
  `/sbin/init` on Linux and has no counterpart on the other two.
- **Nothing can be written to the disk before boot.** disco-vm places files
  through the running guest (boxd's API, or disco-vm's own guest agent on a
  local VM). 26-10-08-127 §2's "place one file before boot" would mean booting,
  copying, and rebooting so that a PID-1 flow could read it.
- **There are no volumes to wire.** ADR 0126 §2 already says that where a
  runtime has no PID-1 flow, the declared-volume mechanism does not apply.

And the pool needs exactly one thing from the backend once the agent runs: a
connection to the agent's API. Everything else the pool does in a sandbox —
exec, terminals, files, the runtime-config intake, and user ports through
`tcp/attach` and `udp/attach` — is already a route on that API. A backend that
exposes arbitrary guest ports, or runs a second guest agent of its own for
exec and files, duplicates the sandbox agent.

boxd (and exe.dev, ADR 0126's other provider candidate) puts every machine
behind a public HTTPS proxy with a managed certificate, websockets included,
at no cost. boxd's docs offer no access control on it beyond a tailnet-only
mode an organization must have enabled, and a browser check on requests that
would wake a sleeping machine.

## Decision

### 1. The sandbox agent does not assume it is PID 1

It is an ordinary service of the guest's own init. When it starts and its
bootstrap is not there yet, it **waits for it** — on every OS, built into the
agent — rather than exiting or being skipped. It then provisions what applies
outside a container — the sandbox user and its groups, the home skeleton,
`~/.gitconfig` and direnv config — and serves. Declared volumes are not wired
(0126 §2); sources and everything dynamic arrive through the intake as they do
on every backend (26-10-08-127 §1).

On a systemd image the wait is also expressed natively: a `.path` unit
(`PathExists=` on the bootstrap) activates the agent's service, so a machine
with no sandbox in it runs no agent. launchd and the SCM start the agent at
boot, and it waits.

A container keeps its PID-1 `init` flow; this decision does not change it.

### 2. A VM backend's contract is start, place the bootstrap, and one address

The pool asks a VM backend for three things, in order:

1. **Start the machine** from an image that has the sandbox agent installed.
2. **Place `sandbox.json`** at the agent's fixed path, through the running
   guest. This is 0126 §3's bootstrap exception, moved from before boot to
   before the agent: the agent is not running yet, so the pool still changes
   nothing inside a running sandbox except by asking it.
3. **Return one address that reaches the agent's API.** It exists from create,
   answers once the agent serves, and stays the same across stop and start.

The backend exposes no other guest port and offers no exec or file operations
to the pool. Which guest port is the agent's is the image's to declare, not
the backend's to know.

### 3. A provider's public HTTPS is an acceptable address

For a provider-hosted VM (boxd, exe.dev), the address is the provider's HTTPS
URL for the agent's port. It is 0126 §5's "address the pool can dial", and it
is public: **the sandbox agent's own token check is the only gate**, which §5
already makes the authorization decision on every transport. Two things follow:

- A route the agent answers without a token is answered to the internet, so
  the agent's unauthenticated surface must be audited and kept minimal before a
  provider-hosted backend is enabled.
- The provider terminates TLS and sees the traffic. It runs the machine, so it
  is already inside the sandbox's trust boundary; but transport-level client
  authentication (mTLS between pool and agent) cannot ride this hop.

A provider feature that restricts who can reach the URL — boxd's tailnet-only
mode, or anything like it — is welcome hardening, never a substitute for the
token check.

A local VM's address is the host-to-guest socket of ADR 0144: vsock on
Virtualization.framework, hvsocket on Hyper-V.

## Alternatives rejected

- **Make `discobox-sandbox-agent init` PID 1 in a VM** by pointing `/sbin/init`
  at it. Linux only, depends on boxd booting the disk's init, and needs the
  bootstrap on disk before boot — a boot, a copy and a reboot per sandbox.
- **Place the bootstrap before boot** (a seed disk, provider user-data), as
  26-10-08-127 §2 assumed. disco-vm has no pre-boot file injection; adding it
  is cheap on boxd (whose cold prepare already runs the guest) and real work on
  an offline macOS or Windows disk image, to serve a PID-1 flow §1 removes.
- **Reach the agent through disco-vm's own guest agent**, or through a relay
  over boxd's authenticated Exec. Both keep a second agent or a per-connection
  provider credential in the pool's path, for a byte stream the provider
  already serves over HTTPS.
- **boxd's `ExposePort`.** A raw public TCP port with no TLS and no access
  control: everything the HTTPS proxy gives up, and nothing it lacks.
- **The outbound multiplexed session of 0126 §5** for provider VMs. It is
  required only where the sandbox cannot be dialed; a provider that serves
  HTTPS can be.
- **A backend that exposes any guest port the pool asks for.** The agent's
  `tcp/attach` and `udp/attach` already reach every port from inside the
  sandbox, behind its tokens.

## Deferred

- **Containers on the same model** — systemd as PID 1, the agent waiting for
  its bootstrap, provisioning moved out of `init`. Revisit when the PID-1 flow
  is the only thing a container still needs that a VM does not: declared
  volumes are the reason it exists today.
- **Egress confinement on provider VMs** (0126 §6) is not decided here. boxd
  documents a per-machine egress allowlist enforced outside the machine, which
  a restored machine starts without; a boxd backend is not enabled until it is
  set on every create and restore, and verified.
- **Reaching the pool from a provider VM** (0126 §§6–7): a pool the provider's
  machines cannot reach cannot serve them. Where the pool runs is the operator's
  choice and outside this decision.

## Consequences

- sandbox-agent waits for its bootstrap instead of being skipped without it,
  and gains a provisioning path that runs as a service rather than as PID 1.
- The sandbox image replaces the agent unit's `ConditionPathExists` with a
  `discobox-sandbox-agent.path` unit.
- disco-vm's driver contract narrows to lifecycle, guest file and exec
  operations for its own builds and the bootstrap, and one service endpoint per
  instance, from a port the image declares.
- The pool gains a VM backend that is the three steps of §2, plus the intake
  over the returned address.
