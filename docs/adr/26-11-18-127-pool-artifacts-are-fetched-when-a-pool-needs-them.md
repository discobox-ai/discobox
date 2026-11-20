# 26-11-18-127 — Pool artifacts are fetched when a pool needs them

- **Status**: Accepted
- **Date**: 2026-11-18
- Accepted by the owner by merging #1397, 2026-11-20
- **Issue**: [#1392](https://github.com/discobox-ai/discobox/issues/1392)
- **Amends**: [0113](0113-the-cli-stages-the-images-its-server-loads.md) §§1–2 for automatic staging. Its explicit staging, cache and provider rules still apply.
- **Preserves**: [0069](0069-staging-pool-images-is-a-condition.md)'s pool image condition, [0099](0099-the-cli-downloads-the-server-it-starts.md)'s verified server download, and [26-11-14-624](26-11-14-624-after-an-upgrade-the-server-is-not-ready-until-its-pools-are.md)'s bounded pool handover, as amended.

## Context

A clean Linux amd64 account installed the main CLI successfully but hit the
300-second deadline during its first `discobox ls`. One run reached the
listener at the deadline. Triage reproduced the timeout during the first
image download, before launch. The account lacked access to both KVM and
Docker. The two runs show different bottlenecks and do not quantify how
much of the delay removing image staging alone will recover.

At the issue's base, `cli/internal/cli/root.go` runs image staging inside
the autolaunch command resolver, before the server is launched or its
readiness can be checked. `server/internal/server/images.go` asks the release
manifest for platform-specific boot references and the sandbox base.
`server/internal/service/defaults.go` checks KVM only after launch, so the
CLI can download a libkrun guest and kernel for an account that will fall
back to Docker. Startup also inspects release harness bundles to seed their
configuration and check availability before router handover
(`server/internal/resources/harnessconfigs/service.go`,
`server/internal/server/server.go`). That registry work has a different purpose
from downloading pool images.

## Decision

1. **Automatic local launch stages the verified server distribution.**
   Autolaunch proceeds from resolving that distribution to starting the server.
   The VM guest, libkrun kernel, sandbox base and other pool-only payloads
   are outside the fresh API readiness path. Readiness still requires
   database and service initialization, including the bundle metadata
   and availability checks already needed to seed release harnesses.
   A provider's inability to start a new pool is reported through the existing
   provider and pool diagnostics. Health on an existing installation still
   observes the bounded upgrade wait for previously registered pools.

2. **The pool's actual consumer fetches the payload.** Provider selection and
   prerequisite checks determine what can be used. A VM provider fetches its
   pinned boot artifacts through the existing shared image cache when it
   needs to boot a pool. A Docker-backed pool or sandbox uses the existing
   daemon/cache/registry resolution path for its base image. Registry errors
   and progress belong to the requesting pool operation or its staging
   condition. A listing or health request does not start a pool to warm the
   cache. Explicit `discobox admin server stage` keeps the release's
   platform-based image inventory and stages it into the existing cache for
   users who want to pay that cost ahead of time.

3. **A launch waiting for an artifact shows its download.** The provider or
   sandbox operation doing the fetch publishes progress through the existing
   pool or sandbox provisioning status. Interactive CLI and console launch
   waits display the artifact being fetched, bytes downloaded, and the
   total when known. When byte counts are unavailable, show the artifact
   and current phase. Keep that phase visible during a slow transfer and
   show retries and terminal failures where the user is waiting. A launch
   that joins a fetch already in flight shows its latest progress on the
   first status read. Once the fetch finishes, show the next provisioning
   phase, including loading from cache or booting as applicable. General
   pool readiness text is for waits with no more specific active phase.

4. **Measure five minutes on a defined cold path.** On a dedicated Linux amd64
   validation VM with at least four vCPUs, begin with one-minute load no
   higher than 1.0 and keep it at or below 2.0 for the run. Use a fresh account
   with no local server, server distribution, or image cache and without
   Docker or KVM access. Install the published main CLI before starting the
   clock. Use reachable release and registry origins. Qualify the network
   with a separate, uncached probe of each origin on the same route, with
   at least 20 Mbit/s sustained payload throughput on a large object and
   HTTPS request time to first byte at or under two seconds. Record the
   release, asset sizes, probe results and host load alongside phase timings.
   From the start of first use, allow 300 seconds total for
   `discobox ls`, ready health, and an authenticated local status check
   confirming the installed CLI's server version. Include the cold server
   download and harness metadata reads in that clock. A miss inside the
   envelope is a regression signal. Record misses outside the envelope with
   the phase and environmental measurements so they can be rerun under
   qualifying conditions. These are proposed validation conditions, not
   measurements obtained from the original busy host.

## Consequences

The first pool operation can now incur the download that autolaunch previously
paid. Explicit staging still amortizes those reads ahead of use, and a later
pool can reuse artifacts the cache already holds. The validation should
also observe a real first pool start on a host with a working provider.
Throttle a cold artifact download long enough to observe the artifact name
and advancing byte counts in the launch wait. Join the wait while the fetch
is in flight, confirm both waits see the same progress, and check that the
status advances after download and reflects cache reuse on a subsequent
start. That exercise has its own timing and does not gate the clean-account
health check.

## Alternatives rejected

- **Filter the pre-launch list by probing the default provider.** This avoids
  downloading a guest on a KVM-ineligible host, but still charges every first
  API user on a VM-capable host for downloads needed only at pool boot. It
  also duplicates provider selection outside the server's persisted
  configuration.
- **Start a background image prefetch at launch.** It still speculates about
  which provider and images will be used and competes with startup for CPU,
  disk and bandwidth. Explicit staging already provides a way to opt in.
- **Extend the timeout.** Slow mirrors and large images still consume the
  first-use budget before the API can start. The extra allowance also masks
  regressions in server initialization.

## Deferred

The proposal leaves the harness metadata path in startup. If qualified
cold runs still miss the budget and timings implicate bundle inspection,
revisit how verified release metadata is delivered. Phase instrumentation
and the mechanics of the validation runner are implementation choices.
