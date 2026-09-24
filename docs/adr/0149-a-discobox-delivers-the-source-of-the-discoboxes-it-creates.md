# 0149 — A discobox delivers the source of the discoboxes it creates

- **Status**: Accepted
- **Date**: 2026-09-24
- **Supersedes**: [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md)'s
  rejection of "Authorize by who created a discobox", for source delivery
  only. Everything else 0140 decides stands, including that creating a
  discobox confers nothing else on its creator.

## Context

ADR 0140 §1 has a discobox call the API as
`discobox-access run --use <id> -- discobox new …`. Run from inside a
discobox, `new` fails after the create:

- **The source is never delivered.** The new discobox cannot reach the
  creating discobox's filesystem, so its local source is push-delivered, and
  the push (`sandboxes/{id}/git-origins/…/git-receive-pack`) and the report
  that ends the wait (`complete-source-push`) are not in the sandbox role. The
  new discobox waits for a source that never comes.
- **The SSH sync after the create is refused.** It reads the project and lists
  and *enrolls* an SSH key, and an enrolled key reaches every discobox in the
  project over SSH.
- **`new` cannot give grants.** Only `admin box create --grant` can, and that
  command infers nothing — no user, so the new discobox runs as root, where
  Claude Code refuses to start in its bypass mode.

And one thing works that should not: a create's `origin` is the client's word,
and when its `hostId` is the server's own, a local source is cloned from the
server's filesystem instead of pushed. Every discobox's record carries its
creator's `hostId`, and `discobox get` is in the sandbox role, so a discobox
can claim the user's machine and have a new discobox materialize any
repository on it.

## Decision

### 1. The creating discobox is recorded

A discobox created by a sandbox records that sandbox (`createdBySandboxId`),
fixed at create. One a person created records none. It is what §2 checks and
nothing else reads it for authority.

### 2. The role delivers source to a discobox the caller created

The sandbox role gains source delivery — the Git push into a discobox's
`git-origins` (`info/refs` for `git-receive-pack`, and `git-receive-pack`) and
`POST sandboxes/{id}/complete-source-push` — allowed only when the target
discobox's recorded creator is the calling sandbox. The authorizer loads the
target to decide, which is authorization by resource ownership rather than by
the request's body.

Fetching from an origin (`git-upload-pack`) is not added: delivery only
pushes.

### 3. A create from a sandbox is always push-delivered

The server ignores a sandbox caller's `origin` when deciding delivery: every
local source in its create is push-delivered. A sandbox's claim about which
machine it is on is not evidence of anything, and the server's filesystem is
the user's.

### 4. `new` is how a discobox makes one

`discobox new` takes `--grant` in `admin box create`'s form, and `--json`,
which reads the whole request as one JSON object on stdin — for an agent, which
would otherwise quote a prompt and a use's sentence through a shell — creates
the discobox without attaching, and prints it as JSON. It infers the user,
group and Git identity as it does on a person's machine.

The SSH sync after a create is skipped when the server refuses it (403): it
exists so a person can `ssh` to the discobox, and a discobox cannot.

## Alternatives rejected

**Allow any sandbox to push to any discobox's origin.** A push-delivered
discobox's agent fetches its origin as "the user's newest commits"; any
discobox holding the discobox credential could put commits there.

**Allow the push only while the target awaits its first delivery.** Closes the
same hole for running discoboxes, but a discobox could still race a person's
own create in the window before they push. The creator is a fact the server
records; a state is a moment.

**Add the SSH sync's routes to the role.** Enrolling a key is SSH into every
discobox in the project — far beyond creating discoboxes and giving them
credentials, which is all 0140 grants.

**Keep teaching `admin box create`.** It is the flag-driven administrative
path and infers nothing; every omission it leaves is a way for an agent to
make a discobox that does not work.

## Consequences

- A discobox can hand its own repository, uncommitted work included, to a
  discobox it creates, and push to that discobox's origin again later. It
  cannot push to any other.
- A discobox created by a person is never pushable by a sandbox: its creator
  field is empty.
- Sources in a create from a sandbox are never cloned from a host path, on any
  provider.
- The in-box skills teach `discobox new --json`.
