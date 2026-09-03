# 0092 — The CLI has no unjudged way to take a value

- **Status**: Accepted
- **Date**: 2026-09-03

## Context

[ADR 0079](0079-a-local-judge-gates-every-wrapped-credential-use.md) put a judge
in front of `run` and named its own limits plainly: "an agent that does not want
to be judged calls `get`, or the protocol endpoint directly" (§"It is a
guardrail, not a boundary"), and §6 called `get` "the documented weaker path,
for scripts that cannot be wrapped."

That was a reasonable line to draw at the time — `get` had an honest use, a
script that cannot `exec` its own credentialed step needs a raw value, and the
alternative was building the judge before shipping anything. But it is a line
that gets harder to hold, not easier, the more the judge improves.
[ADR 0090](0090-the-judge-is-handed-facts-and-given-no-tools.md) and
[ADR 0091](0091-a-credential-is-not-issued-without-a-verdict-on-record.md) both
narrow what a *judged* use can do wrong — bounded facts instead of tool access,
a verdict that cannot be issued without a record — and neither of them touches
`get`, because `get` was never judged in the first place. Every improvement to
`run`'s safety widens the gap between what `run` guarantees and what `get`
skips entirely, and `get`'s own justification — "a script that cannot be
wrapped" — does not get weaker just because `run` got stronger.

`get`'s value is also not what the name suggests. It does not fetch or copy a
secret; `secretformat.MintSentinel` generates a fresh, cryptographically random,
byte-shape-identical decoy, and the pool agent's egress proxy is the only thing
that ever turns it into something real, by swapping it into a request that
transits the sandbox's own path to the approved host within the activation's
five-minute window. Printed to a terminal or piped into a file, it is inert.
That does not make `get` safe — a value printed into that same egress path by a
script is exactly as usable as one `run` would have injected — it means `get`'s
danger was never about leaking the string on sight. It was always about the
missing judge.

Keeping `get` as a documented, supported capability means every future
improvement to the judge has to be re-litigated against it: does this new check
also need to run before `get`? If not, why does `run` deserve it and `get`
not? Answering that question forever, for a command whose only reason to exist
is that wrapping is sometimes inconvenient, is not a cost worth paying to keep
one flag.

## Decision

**`discobox-access` drops `get`. There is no CLI command that returns a
credential value without a command for the judge to have ruled on. The
underlying protocol call, `POST /v1/credentials/use`, is unchanged — `run`
still calls it, after judging — because it is infrastructure the pool agent
needs, not a capability this CLI chooses to expose.**

This is a policy decision about what the CLI supports, not a new technical
control. Nothing stops a script from reaching `/v1/credentials/use` directly
with its own HTTP client, the way ADR 0079 already noted it could reach the
judge's `run` around by going to "the protocol endpoint directly." Removing
`get` closes the CLI's own contribution to that gap — the one this project
ships and documents — and leaves the harder half, an unmediated caller
speaking the protocol itself, where ADR 0079 §5 already pointed: a trusted-side
judge inside the pool agent's handling of that call, which is unaffected by
whether this CLI still offers a shortcut to it.

## Alternatives rejected

**Keep `get`, and make it as safe as `run`.** Rejected: there is no version of
"print a value with no attached command" that a model can judge, because
judging is comparing a command against an approved sentence and there is no
command. The facts ADR 0090 adds and the verdict ADR 0091 requires both attach
to an argv; `get`'s call has none. Any safety `get` could gain would have to be
some other mechanism entirely, not an extension of the judge.

**Keep `get`, documented as unjudged, and let a caller choose.** Rejected: it
is choosing to keep publishing an escape hatch under our own name, and "well
`run` is stronger if you use it" is the same argument that was true before this
ADR and did not stop `get` from being reached for. A supported capability gets
used for what it is capable of, not for what its documentation recommends
instead.

**Wrap `get`'s script use case some other way — a second, restricted `run`
variant.** Not rejected, deferred without a specific trigger: no caller of
`get` inside this repository was found at the time of writing to justify
designing its replacement now. If one turns up, the shape to reach for is
`run` with a shell around the uninterpretable step, not a new unjudged
primitive.

## Consequences

- `discobox-access get` is gone. A script depending on it fails at `unknown
  command "get"` from an image built after this lands, and needs to move to
  `run --use ID -- sh -c '...'`.
- The protocol's `POST /v1/credentials/use` and `agentcreds.Client.Get` are
  unchanged. `run` still calls the same endpoint after judging; only the CLI
  path that called it with nothing to judge is gone.
- `docs/agent-credentials-protocol.md` still documents `get` as a protocol
  operation available to any caller of the protocol directly — that is accurate
  and stays. Its CLI-specific framing of `get` as one of `discobox-access`'s
  supported operations does not, and was updated in the same change as this
  ADR.
- This does not close the gap ADR 0079 named. An agent willing to speak the
  protocol itself, rather than call this CLI, still reaches an unjudged
  `/v1/credentials/use`. That is unchanged by this ADR and remains ADR 0079
  §5's open work.
