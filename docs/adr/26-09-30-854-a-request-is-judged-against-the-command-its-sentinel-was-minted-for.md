# 26-09-30-854 — A request is judged against the command its sentinel was minted for

- **Status**: Rejected by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
- **Date**: 2026-09-30
- **Supersedes in part**: [26-09-22-838](26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md)
  §5's "the declared command is optional context and explicitly untrusted";
  [26-09-25-428](26-09-25-428-the-judge-may-let-its-allow-stand-for-a-route.md)
  §3's key for a standing allow, which gains the command.
- **Depends on**: 26-09-22-838 §3 — the command judge on trusted ground — which
  is decided and not yet built.

## Context

A use is approved as a sentence, and a discobox spends it in two steps.
`discobox-access run --use <id> -- <argv>` has the argv judged against the
sentence, and the pool mints a fresh sentinel for that run, recording the argv
on the activation. Then the argv's process sends HTTP requests, and the proxy
has each one judged against the same sentence, alone.

The request judge is never told what the sentinel was minted for. The pool
holds the argv and drops it (`judge.Job.Command` is always empty on a request
job), and ADR 838 §5 calls the command untrusted context in any case. So the
judge sees a bare request and has to decide whether it supports a purpose
written about a command it cannot see. On 2026-09-29, orchestrating one epic
across ten discoboxes, that failed on nearly every step:

- `discobox new`, approved as that command with its prompt quoted, sent a
  create the judge refused because it took the quoted prompt as the purpose,
  then a `GET` of the new discobox it refused as "not part of reading GitHub
  issues", leaving a discobox created and never given its source.
- `discobox secret request approve <id>` sent `GET /secrets` to resolve a name
  and `GET /secret-requests/<id>` to read the asked lifetime before the
  approval. Both were refused: "a read-only retrieval … does not carry out the
  approval".
- A use naming `gh issue view` was refused for the GraphQL call `gh issue view`
  makes.

Each refusal is the judge failing to connect a request to the intent that
produced it. "Ordinary supporting operations" (838 §5) is only decidable by
someone who knows what the operation is supporting. Rewording uses does not
fix it: a use written as a command is matched literally by the command judge
and cannot be recognized in the requests it causes, and a use written as an
outcome gives the request judge nothing to anchor a read to.

## Decision

### 1. An activation carries the verdict that minted it

A sentinel is minted only after the trusted command judge (838 §3) allows the
argv. The activation records that command verdict — its ID, the argv, and the
digest of the stdin that was shown — and nothing a discobox reports can put
another there. An activation from a verdict the sandbox made itself cannot
exist once 838 §3 lands, and until it does this ADR's §2 does not apply: the
argv would be the sandbox's word.

### 2. The request judge is told the command, as a trusted fact

Every request job under an activation carries the command it was minted for,
labeled as what Discobox itself allowed for this sentinel, beside the untrusted
request. `judge.Job` carries it in place of the declared `Command`, which no
request job has ever carried; a command job keeps its argv as the thing being
judged.

### 3. The question has two parts, and the command only narrows

> Is this request something the allowed command does in carrying out the
> approved use, without materially expanding the use?

The approved sentence stays the ceiling: a command the judge allowed at mint
cannot carry a request the sentence refuses. The command narrows what else is
allowed — a request within the sentence that is no part of the command is
refused, because the sentinel was minted for that command and nothing else.
The supporting operations of a command are what it does on the way to its
effect: `discobox new` reads the discobox back and pushes its source,
`approve` reads the request it approves, `gh issue view` asks GraphQL.

### 4. An activation lasts as long as its run

`discobox-access run` reports its child's exit, and the pool ends the
activation then. The activation TTL stays as the ceiling for a run that never
reports. A request verdict records the activation's command verdict, so every
request can be traced to the argv and the sentence it was allowed under.

### 5. A standing allow is keyed by the command too

An allow stands for the same discobox, use, origin, route, **and argv**
(ADR 428 §3). A later run of the same command — a lead polling a worker with
the same `get` — is covered; a different command under the same use is asked
about, because the allow was decided with one command in view.

## Alternatives rejected

**Keep the command as untrusted context.** It is what we have, and the judge
cannot decide supporting operations without it. Every refusal above is this.

**Judge only at mint** (838, rejected there and still rejected). The request
is a different object from the argv and is the one that reaches the internet.
This ADR keeps both judgements and joins them; it does not drop the second.

**Declare, per CLI, the requests each command makes, and match them without
the model.** Exact for the few commands Discobox writes, useless for `gh`,
`git`, `npm` and everything else, and wrong the day a command changes what it
sends. The model reads a command and a request together; endpoint rules
(838 §7) can still add exact knowledge where Discobox has it.

**Bind a sentinel to the process that holds it.** The proxy sees the sandbox,
not a process. Attributing a connection to a process needs something in the
sandbox to say which, and everything in the sandbox belongs to the agent,
which has root. A sentinel read out of `/proc` by another process in the same
discobox remains usable during the run; §3 and §4 bound what it can do to what
the allowed command does, until the run ends.

**Word uses as outcomes with no command.** It gives the request judge no
anchor, and the command judge no argv to match; it trades one kind of refusal
for the other.

## Consequences

- 838 §3 becomes a precondition, not a plan: the command judge moves to
  trusted ground before request jobs carry the command.
- The system prompt, the job, and `PromptVersion` change. Verdicts made under
  the previous version stay readable and are not re-judged.
- Credential verdicts gain a reference to the command verdict they ran under,
  which is a migration of the verdict table; existing rows have none.
- `discobox-access run` gains an exit report; the portable credentials
  contract gains the call that carries it.
- Standing allows are narrower: one granted for one command covers only that
  command's requests.
- A request unrelated to its command is refused even when the use would allow
  it, which is stricter than today. A discobox that needs it runs the command
  that sends it.
