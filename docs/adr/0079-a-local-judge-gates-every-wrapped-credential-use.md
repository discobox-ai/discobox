# 0079 — A local judge gates every wrapped credential use

- **Status**: Accepted
- **Date**: 2026-08-28

## Context

[ADR 0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md)
gave an agent a way to ask for a credential and use it: `discobox-credential run
--use <useId> -- <command>` declares a command, takes an ephemeral sentinel for
it, and injects that sentinel into one child process. The declared command is
recorded on the trusted side and the sentinel is scoped to a host and a
five-minute window, but nothing compares the command to the sentence a human
approved. A use granted for "open a PR against the current repo" is equally
usable for `curl api.github.com/user/repos -X DELETE`: same credential, same
host, same window.

ADR 0031 §6 deferred semantic judging and said it must never run in the sandbox,
placing it inside `ResolveSandboxSecret` on the trusted side. That is still the
stronger design, and it is still unbuilt. What has changed is that the coarse
controls are now visibly too coarse for the thing they gate: host plus TTL says
nothing about *what* the command does, and the declared command — the one piece
of evidence that would — is currently only written to an audit log nobody reads
before the value is handed over.

## Decision

**`run` asks a model whether the command it is about to execute is the use it
was granted, and executes nothing unless the answer is yes. The model is reached
through `discobox-prompt`, a one-shot prompting CLI every harness image
provides. The gate is the client's, it runs before the value is taken, and it
fails closed.**

### 1. The order is judge, then take

`run` reads the approved use from `list`, judges the argv against it, and only
then calls `get`. A refusal therefore mints no ephemeral sentinel and leaves no
activation behind — the credential is never issued for a command that will not
run. A use `list` does not carry cannot be judged at all (there is no approved
sentence to compare against) and is refused without asking.

### 2. `discobox-prompt` is the harness's contribution

```
discobox-prompt --model judge --system <text> --prompt <text> --output-schema <json schema>
```

- `--model judge` names a *role*, not a vendor's model id. The wrapper maps it
  onto whatever small, fast model its harness has. The CLI never learns a model
  name.
- stdout carries one JSON document conforming to `--output-schema`; exit 0 means
  the model answered. Anything else is a failure.
- The verdict schema is `{allow: boolean, reason: string}`.

Each harness image ships the wrapper as `/usr/local/bin/discobox-prompt`, a
shell script around the CLI it already installs and authenticates —
`claude -p` for claude-code, `codex exec` for codex-cli. It is a harness
concern for the same reason the launch and configure scripts are: the harness is
what knows how to talk to a model in that image, and the mapping is
version-coupled to a CLI the image pins.

### 3. Fail closed, in every direction

No wrapper on PATH, a non-zero exit, output that is not the verdict shape, or
`allow: false` all produce the same outcome: the command does not run, the CLI
exits non-zero, and the failure carries `denied` with the reason. There is no
flag to skip the judge. An escape hatch would be used by exactly the situation
the gate exists for — an agent that has been told to get something done.

### 4. This is a guardrail, not a trust boundary

It runs in the sandbox, which is untrusted by construction. An agent that does
not want to be judged calls `get`, or the protocol endpoint directly, or never
asks at all. **Nothing here is an authorization decision**, and no trusted-side
check is relaxed because of it: the pool agent still refuses an ephemeral
sentinel outside its activation window or against a host the use was not
approved for, and the control plane still checks grant liveness on every
resolve. Those remain the only things standing between a compromised sandbox and
a credential.

What the judge catches is the honest failure, which is also the common one: an
agent that drifted from the task it was granted a credential for, that reached
for a broader command than it needed, or that was steered by injected content in
a repository it is reading. For those it is the difference between a credential
that is scoped and a credential that is scoped *and* aimed.

### 5. The placement is provisional, and the pattern is the durable part

This is the first half of a two-step move, and it is being landed as the first
half deliberately rather than as the whole answer. The gate lives in the CLI
today because that is where the argv is; it will be reimplemented somewhere the
sandbox cannot reach, at which point it stops being advisory. What is being
established now is the *pattern* — a role-named model, a system prompt, a
verdict schema, judge-before-take, fail-closed — and that pattern is what
survives the move. Only the caller changes.

The obvious next step is **the pool agent judging inside `get`**. It already
receives `{useId, command}`, already holds the approved use, and already mints
the sentinel, so a verdict there is one the sandbox cannot skip: there is no
path to a value that does not pass it. That converts the property from "an
honest agent is kept on task" to "no credential is issued without a verdict",
which is a real barrier and not a guardrail.

It is not a complete one, and the gap is worth naming precisely: on the trusted
side the declared command is a *claim*. A sandbox can pass one command to `get`
and run another. What keeps that from being free is everything else already
enforced there — the value is a sentinel, scoped to the approved host and a
five-minute window — so a lie buys a differently-shaped command against the same
host, not a wider credential. Closing the gap entirely means binding the
declaration to the execution, which nothing outside the sandbox can observe;
that is the part that needs more work, and it is why this step is not being
claimed as the finished design.

Revisit when any of these is true: the pool-agent implementation is worth its
control-plane latency and consent question (whose model, whose tokens); a
credential whose misuse is unrecoverable is granted through this flow; or the
in-sandbox gate is observed being routed around in practice rather than in
theory.

### 6. `get` stays unjudged

`get` hands the value to a caller that may then do anything with it, so a judge
there would gate the taking and not the using — the declaration and the act are
no longer the same thing, which is the property `run` has and `get` does not.
`get` remains the documented weaker path, for scripts that cannot be wrapped.

### 7. The judge spends the sandbox's own harness credential

ADR 0031 §6 left "whose tokens" as an open consent question. For this half it is
answered by where the judge runs: the harness CLI in the sandbox, already
authenticated as the user, on the account that is running the agent. A trusted-
side judge would still have to answer it with a project-designated secret.

## Alternatives rejected

**Build a trusted-side judge instead of this one.** Not rejected, deferred:
§5 makes it the intended next step, and this is the half that can land now.
ADR 0031 §6's specific placement — inside `ResolveSandboxSecret`, at swap time —
is the part rejected outright, because at that point the trusted side sees an
HTTP request and not an argv, so it cannot ask the question this ADR is about.
The pool agent's `get` is where the argv reaches trusted ground.

**Make the judge optional when no wrapper is installed.** Rejected: the failure
mode is silent and permanent. A harness ships without the wrapper and every use
in every sandbox running it becomes unjudged, with nothing in the output saying
so.

**Run the judge in sandbox-agent instead of the CLI.** It sounds more trusted
and is not: sandbox-agent runs inside the same sandbox. It would move the code
without moving the trust, put a model dependency into the runtime rather than
the harness that already has one, and cost the CLI its stdlib-only
independence — `agentcred` is meant to be liftable into another repository.

**Let the judge see the credential value.** Never available to it by
construction: §1 judges before `get`, so at verdict time the value does not
exist. A judge that needed it would invert the ordering that makes a refusal
free.

## Consequences

- Every harness image must provide `discobox-prompt`. `claude-code` and
  `codex-cli` ship one; `shell` does not, so `run` refuses there until a wrapper
  is installed or `DISCOBOX_PROMPT` names one.
- `run` now costs one model call before the command starts — latency the agent
  pays, and tokens on the sandbox's own harness account.
- `agentcred` keeps its rule of depending on nothing but `agentcreds`: the judge
  is `os/exec` and `encoding/json`.
- ADR 0031 §6 is superseded for the client side, and its swap-time placement is
  superseded outright: the argv never reaches the proxy. Enforcement that must
  hold against a hostile sandbox moves to the pool agent's `get` (§5), which
  inherits the prompt contract, the schema, and the ordering unchanged — the
  work is the caller and its consent question, not the judging.
- The wrapper is a general one-shot prompting interface, not a judging one.
  Anything else in a sandbox that wants a model — a hook, a script — now has a
  harness-independent way to ask for one.
