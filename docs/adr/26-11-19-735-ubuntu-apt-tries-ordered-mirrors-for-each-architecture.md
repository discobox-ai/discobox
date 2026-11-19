# 26-11-19-735 — Ubuntu apt tries ordered mirrors for each architecture

- **Status**: Accepted
- **Date**: 2026-11-19
- Accepted by the owner by merging #1433, 2026-11-19
- **Supersedes**: [26-11-16-365](26-11-16-365-every-linux-base-is-the-newest-ubuntu-lts-read-from-a-fast-mirror.md) §3's mirror
  order and the requirement to use one mirror family for both architectures.
- **Issue**: [#1420](https://github.com/discobox-ai/discobox/issues/1420)

## Context

Every Ubuntu stage writes an apt mirror list with Azure first and a Canonical
endpoint second. The same setup is copied across the sandbox base, the two agent
Dockerfiles, and the VM guest and kernel Dockerfiles. `scripts/ubuntu-pin.sh`
checks that the copies agree. The sandbox base and pool VM guest carry the
sources into boxes.

The sandbox base and the pool bundle both failed to fetch packages during
`ux-smoke` in #1420. Azure was slow or unavailable and `archive.ubuntu.com`
also refused connections. The base build hit the smoke harness's timeout, and
the CI task action retried the whole smoke target under its Go module download
heuristic.

## Decision

Use apt's existing `mirror+file:` source and give each endpoint a distinct
priority. Apt tries lower-priority entries in turn when a fetch fails. Apply the
same list for a given architecture to all Ubuntu stages and retain it in the
sandbox base and guest.

| Priority | amd64 | arm64 |
| --- | --- | --- |
| 1 | `http://mirrors.edge.kernel.org/ubuntu` | `http://mirrors.mit.edu/ubuntu-ports` |
| 2 | `http://ftp.osuosl.org/pub/ubuntu` | `http://azure.ports.ubuntu.com/ubuntu-ports` |
| 3 | `http://azure.archive.ubuntu.com/ubuntu` | `http://ports.ubuntu.com/ubuntu-ports` |

The amd64 list omits `archive.ubuntu.com`. The arm64 list uses the
`ubuntu-ports` archive and keeps Canonical's ports host last as a third source.
The candidate ports paths on kernel.org and OSU OSL returned HTTP 404 for
`resolute-security/InRelease`. MIT provides a separate ports mirror.

On 2026-11-19, HTTP HEAD requests from a discobox to all six listed endpoints
returned 200 without redirection for `InRelease` and the architecture's
`main` and `universe` package indexes in `resolute`, `resolute-updates`
and `resolute-security`. These are bootstrap endpoints usable before
`ca-certificates` is installed. Apt verifies the indexes against Ubuntu's
archive key and the packages against the signed index's checksums. Retain the
one-week maximum age for updates and security indexes.

A mirror that times out consumes time per fetch, so keep connection attempts
and retries bounded. If the smoke build still fails or times out, surface the
mirror hostnames and the apt or build timeout in the job output. Reserve the
Go module retry for failures with Go module download evidence.

## Alternatives rejected

- **Use the same mirror family for both architectures.** This would exclude
  the preferred amd64 mirrors even though apt already chooses an archive by
  architecture.
- **Keep `archive.ubuntu.com` as the amd64 fallback.** The outage showed that
  it can fail alongside Azure. Two independent mirrors ahead of Azure provide
  an alternative without that host.
- **Require HTTPS during bootstrap.** The pinned base starts without CA
  certificates. Fetching the first packages over HTTPS would require a
  separate trust bootstrap.

## Open for implementation

Verify package downloads and failover on both architectures with an unavailable
first mirror. Measure a stalled mirror on CI to choose any timeout or retry
adjustments, and check that exhausted mirrors produce a useful failure before
the smoke job's outer deadline.
