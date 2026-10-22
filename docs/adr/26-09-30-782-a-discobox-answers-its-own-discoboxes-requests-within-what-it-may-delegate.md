# 26-09-30-782 — A discobox answers its own discoboxes' requests, within what it may delegate

- **Status**: Accepted (§3's judging of the uses handed on against a delegation's superseded in part by [26-10-20-782](26-10-20-782-approved-access-is-a-policy-decided-at-the-network-boundary-fast-path-first.md) §11: a hand-off's policy is held to its delegation's)
- **Date**: 2026-09-30
- **Supersedes in part**: [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md)
  §4's grants pre-assigned or approved by a sandbox "with any project secret",
  and its secrets list in the role; §5's "the server reads no grant"; its
  rejections of "Authorize by who created a discobox" (for answering requests)
  and "The server evaluates grants and their uses"; and settles its Deferred
  "Holding a sandbox to what it was given".
- **Relates to**: [26-09-30-854](26-09-30-854-a-request-is-judged-against-the-command-its-sentinel-was-minted-for.md),
  which gives the request judge the command; this ADR takes delegation away
  from the request judge altogether.

## Context

ADR 0140 lets a lead discobox holding `ai.discobox.sandbox` create workers,
give them credentials, and answer their requests, and leaves every limit on
that to the request judge: "Whether a call fits the use it was made under is
the judge's question" (§5), with bounding a lead to its own delegation grants
deferred "with the judge" (Deferred). The judge exists now, and it cannot hold
that bound:

- **It does not see the facts.** Whether a request came from one of the lead's
  workers, which delegation grants the lead holds, and what they allow are
  rows in the control plane. The judge sees an HTTP request — `GET
  /secret-requests/<id>`, a create body with `"grants"` — and a sentence.
- **It reads delegation as prose, twice nested.** A create's grants are use
  sentences inside a request judged against a use sentence. On 2026-09-29 the
  judge refused a create that carried no grants because it took a clipped
  prompt for hidden ones, then refused another because the prompt told the new
  discobox not to push.
- **It varies.** The same create was allowed on one run and refused on the next.

So in practice a lead either cannot delegate, or can hand any project secret
to any discobox, bounded only by the person who approved its
`ai.discobox.sandbox` (0140 Consequences). Delegation is authority over
credentials; it has to be decided where the grants are.

## Decision

### 1. A discobox launches its discoboxes with none, and is held to the bounds if it does not

The way a discobox runs others is to create them with no credential uses and
answer what they ask for: a worker asks through `discobox-access` when it
knows what its work needs. The in-box skills teach that, and teach wording the
create use for any prompt, because a create is judged against the creator's
use and grants folded into it are read there as part of the prompt.

`discobox new --grant` stays, for people and discoboxes alike. A create from a
discobox that does carry grants is held to what §3 holds an approval to, grant
by grant, and a create with a grant outside those bounds creates nothing.

### 2. A discobox answers only the requests of discoboxes it created

The server scopes the sandbox role's secret-request routes — list, get,
approve, deny — to requests whose discobox the caller created
(`createdBySandboxId`), as it already scopes source delivery (26-09-24-630 §2).
The discobox that created the requester is the request's owner; other requests
are not visible to it, and a request no discobox filed, or whose discobox is
gone, is no discobox's to answer. A person answers every request, as today, and
a request for `ai.discobox.sandbox` is still only a person's to approve.

### 3. Approving is bounded by the approver's delegation grants

A discobox never approves a request to delegate (`purpose: delegate`): handing
on the power to hand on stays a person's. Any other request it may approve,
and the server mints the grant only when at least one live delegation grant
(`purpose: delegate`) the approving discobox holds bounds it — one is enough,
and which one is recorded:

- **the same credential** — the delegation grant's secret, or the well-known
  credential it names; the approver does not choose among project secrets, so
  the sandbox role's secrets list holds only the secrets of its live
  delegation grants — what it may hand on, and so may name;
- **a host within** the delegation grant's host;
- **an expiry no later** than the delegation grant's; with no lifetime given,
  the lifetime the request asked for, capped there;
- **uses within** the delegation grant's uses. The uses being approved are the
  ones the approver narrowed them to, when it did (`--use`), and otherwise the
  ones the request asked for; they are what is checked and what is minted.
  Whether one sentence is within another is a reading, not a comparison, so
  the server asks the project's judge (838 §1–2) as a delegation job: the
  delegation grant's uses and the uses being approved, both authorization, no
  request evidence. Anything but an explicit yes refuses the approval — and so
  does a project with no judge to ask, or a judge that cannot be reached: the
  request waits for a person.

The verdict is recorded with the delegation grant it was asked about, so every
grant a discobox mints can be traced to the delegation that allowed it.

A grant made this way stays independent once made (0140's rejection of
cascading lifetimes stands); its expiry already ends no later than the
delegation it came from.

### 4. Approving is one request

With the secret taken from the delegation grant and the lifetime defaulted on
the server, `discobox secret request approve <id>` sends one `POST` and reads
nothing first. The request judge (under 26-09-30-854) sees the approval the
command names; the server decides whether it is within what the approver may
delegate.

## Why 0140's rejections no longer hold

**"Authorize by who created a discobox."** 0140 rejected it as a tree built by
position rather than by whether a call fits the work, and deferred the bound to
the judge. The judge cannot see who created a discobox; the server records it
and already authorizes source delivery by it. This ADR uses it for one thing —
whose requests a discobox may answer — and cascades nothing: no archive, no
purge, no inherited grants.

**"The server evaluates grants and their uses."** Rejected for splitting the
judgement of what a use allows between the server and the judge. The split
here is along what each can know: the server checks identity, credential,
host, and expiry, which are facts; the judge still reads sentences, but two
authorization sentences on trusted ground rather than a sentence against an
HTTP request.

## Alternatives rejected

**Keep delegation in the request judge** (0140 as written). It lacks the facts
and reads delegation as nested prose; see Context.

**Refuse grants on a create from a discobox.** One path to the authority
instead of two, and no grants in a create body for the request judge to read.
But `--grant` is one flag for people and discoboxes, and refusing it to one of
them makes the create API mean different things by caller. With §3's bounds on
both paths, a discobox gains nothing by pre-assigning that it could not get by
approving, and the skills keep discoboxes on the path that judges well.

**Show the request judge the approver's delegation grants.** It moves the
server's facts into a model's reading of an HTTP request, and still leaves the
decision to vary.

**Check only credential, host, and expiry.** A delegation grant's uses would
be decoration: a lead delegated "read issues in org/repo" could approve
"push to main".

**A tree of discoboxes with inherited limits.** 0140's rejection stands. One
level — the creator answers its own discoboxes' requests — is all delegation
needs.

## Consequences

- The sandbox role changes: a create's grants are held to the creator's
  delegation grants; its secrets list holds only what it was delegated;
  secret-request routes are scoped to discoboxes the caller created.
- A lead needs a delegation grant for each credential it will hand on, which a
  person approves once. Without one, its workers' requests wait for a person.
- `discobox new --grant` and `--json` `"grants"` stay. The in-box skills teach
  a discobox to launch with none and approve instead.
- The approve API defaults an omitted lifetime to the one the request asked
  for, for every caller, within the secret's limit — which is what the window
  already opens on, so the CLI stops reading the request first.
- The judge gains a delegation job kind, and `PromptVersion` changes with it.
- Grants already pre-assigned or approved by discoboxes stay as they are.
