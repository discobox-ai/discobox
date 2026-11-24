# Security policy

## Reporting a vulnerability

Report security problems privately, through GitHub:
[Report a vulnerability](https://github.com/discobox-ai/discobox/security/advisories/new)
(the repository's **Security** tab, then **Advisories**). Only the maintainers
can read the report.

Do not open a public issue, pull request or discussion for a vulnerability, and
do not post it on Discord. A public report shows the problem to everyone before
there is a fix.

## What to include

- The affected version (`discobox version`) or commit.
- The platform (macOS, Linux or Windows) and the pool's provider type, as the
  TYPE column of `discobox admin provider ls` shows it.
- Steps to reproduce, and a proof of concept if you have one.
- The impact: what an attacker can do, and from where (inside a box, another
  box, the local machine, the network).

## What happens next

1. We acknowledge your report within 3 business days.
2. We triage it within 10 business days: we confirm it or explain why it isn't
   a vulnerability, and tell you how severe we judge it.
3. We fix it, and keep you updated in the advisory until the fix is released.
4. We publish a GitHub Security Advisory, with a CVE where warranted, and
   credit you if you want to be credited.

Please keep the problem private until the advisory is published.

## Supported versions

Security fixes go into a new release; older releases are not patched. We
support the newest stable release and the newest beta release (see
[Release channels](README.md#release-channels)). Reproduce the problem on one
of them before reporting, if you can.

## Scope

In scope:

- The Discobox server, the `discobox` CLI and its console.
- The sandbox agent and the pool agent.
- The credential proxy: managed credentials, sentinels and their bindings, and
  the destination policy.
- Isolation boundaries: between a box and what runs it (the pool's VM, the pool
  agent and proxy, the user's machine), between pools, and between a box and
  the credentials it was not given. An escape from a box onto the kernel or
  machine that runs it is in scope on every provider.
- The install scripts and the release artifacts they download.

Out of scope:

- Third-party services Discobox talks to (model providers, GitHub, cloud
  providers) and the coding agents it runs; report those to their vendors.
- Problems that need an attacker to already control the host or the user's
  account.
- A user's own misconfiguration, such as binding a secret to more routes than
  it needs.
- What boxes in one pool share on purpose, without an escape: the pool's cache
  volume, its CPU and memory. Put mutually untrusted work in different pools.
- The Docker provider having no VM between a box and the host, which the README
  documents. A container escape on it is still in scope.
