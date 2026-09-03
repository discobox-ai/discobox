# 0087 — The pool agent reaps only children nothing is waiting for

- **Status**: Accepted
- **Date**: 2026-09-03

## Context

The pool agent is PID 1 in its container, so it runs a child reaper: on every
`SIGCHLD` it loops `wait4(-1, WNOHANG)` until the kernel says there is nothing
left, logging each pid it collects. The same process also runs subprocesses
through `os/exec` — every `git` and `chown` in `sandboxruntime`, and the
`git http-backend` behind each client push.

Those two are the same kernel resource. `wait4(-1)` collects *any* exited
child, including one `os/exec` is about to wait for, and the loser of that race
does not get a partial answer — it gets none. Go's `cmd.Wait` calls `waitid`
first, the kernel has already released the child, and `Wait` returns
`waitid: no child processes`. The command ran, its output is in the buffer, and
its exit status is gone:

```
reaper stole pid 146980 status 0
run 1: err=waitid: no child processes out="git version 2.54.0\n"
```

It is a silent corruption of every subprocess result in the process, and it
lands wherever the caller treats "the command failed" as a fact about the
world. `ensureOriginRemote` did exactly that:

```go
current, err := runGitOutput(ctx, target, uid, gid, nil, "remote", "get-url", "origin")
if err != nil {
    // There is no such remote.
    return runGit(ctx, target, uid, gid, "remote", "add", "origin", want)
}
```

`git remote get-url origin` printed the URL, the reaper took its status, the
create concluded the remote was missing, and `git remote add` answered
truthfully — `error: remote origin already exists.`, exit 3 — which failed the
create. The sandbox settled into `error`, which by design ([0017](0017-desired-state-orchestration.md))
is converged until new intent, so it stayed there with one source's `origin`
still pointing at the pool-agent-local clone path that means nothing inside the
container. Three sandboxes created at once is enough `SIGCHLD` traffic to lose
the race; the observed failure had two creates in flight and three stolen
statuses inside two seconds.

## Decision

### 1. The reaper peeks before it claims

`waitid(P_ALL, WEXITED|WNOHANG|WNOWAIT)` names an exited child *without*
collecting it. The reaper asks who is waitable, and only then decides:

- a pid this process owns — a child some `os/exec` caller started and will wait
  for — is left exactly where it is, and the loop stops. Its owner reaps it in
  microseconds, and the peek would otherwise keep naming the same pid.
- anything else is an orphan or a managed child, and is collected with
  `wait4(pid, WNOHANG)` and logged.

### 2. Ownership is registered, and registration happens under the fork

`childproc` holds the registry and every subprocess start in the pool agent
goes through it: `childproc.Run`, `CombinedOutput`, or `Start`/`Wait`. The
registry lock is held across `cmd.Start()`, so a child cannot exist before it
is recorded, and the reaper takes the same lock across its "is this ours?"
check and the `wait4` that follows. There is no window in which a child is
alive and unregistered, which is what makes the peek in §1 conclusive rather
than probabilistic.

The record is released after `cmd.Wait` returns, keyed by a token rather than
the bare pid, because the kernel may hand the pid to the next child started
before the release runs.

A child nobody waits for is deliberately not registered: the systemd namespace
child is stopped with a signal and never waited, so the reaper collecting it is
the point.

### 3. The reaper also runs on a timer

The loop stops when it peeks a pid this process owns (§1), so an orphan behind
it waits for the next wakeup. `SIGCHLD` is that wakeup almost always; a 30
second tick is what stops "almost always" from meaning "until the pool does
something else".

### 4. A caller may not read "no remote" out of a failed command

`ensureOriginRemote` reads `remote.origin.url` with `git config --get`, whose
exit 1 *is* the answer "there is none", and writes the URL with
`git config remote.origin.url` — which creates or corrects it in one step —
then asserts the fetch refspec as it already did. There is no check-then-act
and no command whose failure is interpreted, so it converges from any state a
repository can be in: no remote, a remote with no URL, the wrong URL, or the
right one.

## Alternatives rejected

**Fix `ensureOriginRemote` and leave the reaper.** It is the one caller that
turned a stolen status into a wedged sandbox, and it is a real bug on its own
(§4 stands whatever the reaper does). But the reaper corrupts every subprocess
in the process at random, and the next victim is worse: `gitOriginHasRefs`
answers "nothing has been pushed yet" and parks a sandbox, `directoryEmpty`
answers "already materialized", a `chown` reports failure after succeeding.
Hardening call sites one at a time treats "any command may randomly report
failure" as an invariant to code against.

**Deliver the stolen status back to its owner.** The reaper knows the exit
status it took; it could hand it to the registered owner and have the wrapper
use it when `Wait` returns `ECHILD`. Rejected because the delivery races the
owner: `Wait` learns the child is gone as soon as the reaper's `wait4` returns,
which is before the reaper has recorded anything, so the owner would have to
wait a bounded interval for a status that may never come. Not taking the child
has no such window.

**Reap only known-managed pids.** One line, no registry: wait for the systemd
child by pid and nothing else. Rejected because the reaper has a real job —
`git` spawns a detached `gc --auto`, and anything else re-parented to PID 1
becomes a permanent zombie in a process that never exits.

**Find orphans in `/proc` instead of peeking.** Enumerating zombie children
whose parent is us needs no `unsafe` and no `waitid`. Rejected on cost and
honesty: it walks every process in the namespace on every `SIGCHLD` — one per
`git` command, and a create runs dozens — to answer a question the kernel
answers in one syscall.

**Run a real init as PID 1 and the agent beside it.** The container's process
model is the pool's provisioning contract, and a second process to supervise is
a heavier change than owning the registry. Revisit if the agent ever needs to
restart without the container.

## Consequences

- Every subprocess in the pool agent reports its own exit status, including
  under concurrent creates. The test that proves it is the race itself: the
  reaper loop spinning while commands run, asserting none loses its status.
- `git`/`chown`/`git http-backend` starts serialize on one mutex for the length
  of a `fork`+`exec`. Creates already serialize per sandbox on a heavier lock.
- Reaper log lines change meaning: an unmanaged pid in the log is now genuinely
  an orphan, not an `os/exec` child that lost a race. There were three of the
  latter and none of the former in the incident that produced this ADR.
- A sandbox already wedged by this bug is repaired rather than re-created: §4
  corrects the URL that the failed create left pointing at the pool-side clone
  path.
- `childproc` is the pool agent's only way to start a subprocess. A future
  `exec.Cmd` started directly is a child the reaper may take, and the reaper
  cannot tell that from an orphan.
