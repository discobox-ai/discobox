# 0069. Staging a pool's images is a condition, not a state

Status: Accepted

## Context

A first run is slow in a way that is hard to explain. Creating the first sandbox
on a cold machine waits for a VM to boot, for the pool-agent image, and then for
one to two gigabytes of harness image — at whatever moment the user happened to
ask for a sandbox, with a status line as the only clue. Every later harness costs
its own wait the first time it is used. Narrating those waits (ADR 0060) made
them legible; it did not make them shorter, and it did not stop them arriving one
at a time.

Pulling the images up front is the right answer: one wait, understood as the
price of a first run, and everything after it fast.

The first implementation put that pull in server startup and withheld `/healthz`
until it finished. Three things were wrong with it, and they are the reason this
record exists.

It **started every pool the database had ever held**, at every server start, in
order to have somewhere to pull to — turning a machine with four projects into
four running pool agents that nobody had asked for.

It **called `EnsurePool` from outside the reconcile engine**, which claims and
leases a resource so exactly one worker converges it at a time. Startup held no
claim, so it could race the pool reconciler over the same pool's containers.

And it **made staging a precondition for the server answering at all**. The API
was unreachable for as long as the pull took, bounded at thirty minutes, while
the CLI's own patience was five — so on a cold machine every command failed
against a server that was working perfectly.

## Decision

**Staging is its own reconciled resource, and its result is a condition on the
pool rather than a state of it.**

`poolImages`, keyed by pool ID, registered on the same engine as everything else.
It is claimed and leased like any other resource, retries on its own cadence, and
its failures are its own.

**It creates nothing.** It pulls onto a pool that is already up. The pool's own
reconcile has converged the host before this runs, and marks it dirty on the way
out; a level-triggered scan picks up anything a lost mark drops. `StageImages`
replaces `PreloadImages` on the provider contract, and no longer calls
`EnsurePool`.

**A pool whose images are not staged is active, healthy and schedulable.**
`Pool.ImagesStaged` and `Pool.ImageStage` are display data. Nothing schedules on
them. A sandbox that wants an image its host does not have pulls it then, exactly
as it did before any of this existed — staging is a head start, and the failure
mode of a head start is that it did not happen.

**Server startup has nothing to do with it.** The server binds, migrates, builds
its services and reports ready. Whether images are staged is a property of a
pool, observed when that pool converges, on whatever schedule the engine runs.

**A client that launched the server waits on the condition.** That is where the
"first run is slow, then everything is fast" experience lives: the CLI autolaunch
path waits for the pool to be active and its images staged, and says what it is
waiting for. A client that did not launch the server waits for nothing.

## Consequences

Staging failures are invisible unless somebody looks at the pool. That is the
intended trade — a registry outage must not fail a reconcile or make a host
unschedulable — but it means the recorded condition carries the error, because
it is the only place the failure is written down.

A pool that is never brought up is never staged. This is correct and worth
stating: staging follows pools, and a pool nothing has asked for has no host to
pull onto.

## Alternatives rejected

**Staging as part of the pool's own reconcile.** It would inherit the engine's
failure backoff, which escalates a resource that must converge — and this one
must not. A pool would go into a failing reconcile because a registry was briefly
unreachable.

**Staging as a pool state (`staging` before `active`).** It would gate placement
on a registry. A host that can run containers can run them whether or not an
image has been pre-pulled, and saying otherwise makes a pull a dependency of
scheduling.

**A `warming` health state on the server.** Considered while staging still ran at
startup: the API serves, `/healthz` reports not-ready, and deployments wait while
clients do not. It solves the blocking problem without solving the other two, and
once staging leaves server startup there is nothing left for it to describe.
