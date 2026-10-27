# 26-09-26-240 — A request is recognized, and its body is shown in one shape

- **Status**: Superseded by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
- **Date**: 2026-09-26
- **Supersedes**: [26-09-22-838](26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md)
  §6's body forms (the judge asking for a body "as text or as JSON") and its
  rule that the first ask describes a body by media type and length alone.
  §7's endpoint rules are unchanged in what they may do; this gives them the
  place they plug into. Every other section is unchanged.

## Context

A `git push` through a use-scoped GitHub credential was refused every time it
was tried, on two repositories, across two grants. The verdicts show why:

- `GET …/info/refs?service=git-receive-pack` was allowed every time.
- `POST …/git-receive-pack` was refused. In round 1 the judge asked for the
  body; in round 2 it was told `"missing": "the body is not text"`, and
  refused because it could not see which ref the push updates.

A receive-pack body is a few hundred bytes of text pkt-lines naming each ref
update — old id, new id, ref — followed by a binary packfile. The proxy shows a
body as text or as JSON or not at all, the packfile makes it neither, and so
the part that says exactly what the push does was never shown. One of eight
identical round-2 asks was allowed anyway: the judge was guessing.

This is not a git problem so much as the first of a kind. The proxy knows a
body only as a media type and a length, the judge knows it only as bytes, and
nothing in between knows what a protocol or an API means by it. GraphQL puts
the operation in the body, a fork's destination organization is in the body,
and `--force` is on no wire at all. §7 foresaw endpoint rules and none exists,
because there is nowhere for one to go.

## Decision

### 1. A request is recognized before it is judged

Trusted code in the proxy recognizes what a request is, on two independent
axes:

- **Protocol** — the wire protocol, recognized from the method, the path, the
  declared media type, and the body's leading bytes: git smart HTTP, GraphQL,
  JSON, form, multipart. A protocol is not tied to a host; git's is the same
  on every forge.
- **Endpoint** — an operation on one API, recognized by host, method, and path
  pattern: GitHub's repository fork, the discobox API's create.

Recognizers are a built-in, ordered registry that ships with the pool. It is
structured to be added to, not configured: nothing a request, a sandbox, or a
use says can add or change one (§7's rule, extended to protocols).

**Recognition only adds information.** The sandbox writes every byte, so a
request can claim to be anything. A request whose declared protocol does not
parse as that protocol is shown as having failed to — which is evidence the
judge weighs — never silently described as something weaker.

### 2. A body is always shown in one shape

The judge never chooses a form. Every body, in every round, is described in the
same structure:

```json
"body": {
  "mediaType": "application/x-git-receive-pack-request",
  "length": 1435,
  "parser": "git-receive-pack",
  "metadata": {
    "commands": [{"op": "create", "ref": "refs/heads/topic", "new": "eff2d8a…"}],
    "capabilities": ["report-status", "side-band-64k"],
    "packfile": {"bytes": 1247}
  },
  "content": "…",
  "missing": "the packfile is binary and is described, not shown"
}
```

- `parser` names what read the body, and `metadata` is what it found worth
  knowing. **Metadata is in the first ask** whenever a parser recognizes the
  body: it is small, bounded, and it is what the operation is. A push is
  decided in round 1 from its commands.
- `content` is the body itself, redacted and cut to the budget, rendered the
  way its parser renders it — JSON written back compact, a form with its
  credential-named fields replaced, multipart part by part, binary described
  by length. It is absent until asked for.
- `missing` keeps its meaning: what is not shown, and why.

The judge may still ask to be shown the body. The ask loses its form —
`{"need": {"body": true, "bytes": N}}` — and the answer is the same structure
with `content` filled in. Bounds, rounds, and the rule that an ask repeating
what was already shown refuses are unchanged.

A body no parser recognizes is shown as today's text-or-binary description,
with no metadata: nothing gets worse for want of a parser.

### 3. Parsers are a series, enhanced one type at a time

Parsers are chosen by media type and refined by the recognized protocol, and
every one returns the same three things: metadata, a content rendering, and
what it could not show. Today's text, JSON, form, and multipart handling become
the first parsers, unchanged in what they redact. Redaction applies to metadata
exactly as to content: sentinels are removed and credential-named values
replaced before anything is recorded or sent. Metadata has its own byte cap,
separate from `MaxBodyBytes`, and says so when it is cut.

### 4. What is recognized brings guidance

A recognized protocol or endpoint contributes **guidance**: trusted sentences
about what the operation means and what to weigh, added to the job beside the
evidence and marked as Discobox's own knowledge, not the request's. For git
receive-pack: an old id of all zeros creates a ref and a new one of zeros
deletes it; `--force` is not on the wire, it only lets a client send an update
that is not a fast-forward; and whether an update is one is not in the request.

The system prompt stays one fixed text. Guidance travels only with the jobs it
applies to, so a GitHub push is told about git and a GraphQL call is told about
GraphQL, and neither pays for the other.

An endpoint recognized under §7 may still decide without the model (only ever
narrowing), add trusted facts from the control plane, or name an upgrade. This
ADR changes where those rules attach, not what they may do.

### 5. A protocol may refuse in its own terms

A refusal is a `403` with a sentence in a `text/plain` body. A client that
speaks a protocol often never shows that body: `git push` prints
`RPC failed; HTTP 403` and nothing else, which reads as a credential without
permission rather than a judge that said no. A recognized protocol may answer a
refusal the way its own server would — git's report-status `ng <ref> <reason>`,
which `git push` prints as `! [remote rejected]` — so the reason reaches whoever
is reading the client's output. The refusal is the same refusal, audited the
same way.

### 6. A verdict records what was recognized

The request a verdict stores gains the protocol, endpoint, and parser that
described it, each with a version, beside the system prompt's version. What a
judge was told is then traceable to the code that told it. The stored request
is JSON, so earlier verdict rows are read as they were written and need no
migration.

### 7. The first entries

- **git smart HTTP**, receive-pack: commands, capabilities, and push options as
  metadata; the packfile described by size; the guidance of §4; refusal as
  report-status.
- **JSON**, **form**, **multipart**, **text**: today's handling, as parsers.
- **GitHub fork** (`POST /repos/{owner}/{repo}/forks`): the destination
  organization lifted into metadata, since it is the one field that says where
  the fork lands.

## Alternatives rejected

- **A `git` form beside `text` and `json`.** It fixes this request and repeats
  the problem: the judge has to know a form exists to ask for it, and every
  protocol adds one. What reads a body is the proxy's knowledge, not the
  judge's choice.
- **Every body in full in round 1.** Simplest, and every request pays for its
  body in tokens, including the GETs whose URL settled them. Metadata carries
  what decides most requests at a fraction of the size.
- **Protocol knowledge in the system prompt.** One prompt for every protocol
  grows without bound, and every job pays for all of it.
- **Rules as configuration or scripts, loaded at run time.** Deferred. Rules
  are trusted code, and the only thing that may write them is the pool's
  release. Revisit when an operator needs rules for a private API that cannot
  ship with Discobox.
- **Sniffing the body alone, or the media type alone.** The media type is the
  sandbox's claim and the body's first bytes are too; recognition reads both,
  with the method and path, and a parse that fails is evidence rather than a
  fallback.

## Consequences

- The prompt version changes, and `Need`'s schema changes: `Decode` accepts the
  new ask and refuses the old `"text"`/`"json"` one. Harness wrappers pass the
  answer through and are unaffected.
- A git push is decided in one round, from what it does, instead of two rounds
  that end in a refusal.
- The proxy grows a registry of recognizers and parsers, and each new protocol
  or API is one entry and its tests. Parsing bodies is attack surface: parsers
  are bounded in bytes and time like the capture, and never alter what is sent
  upstream.
- Guidance is trusted text Discobox writes about third-party APIs, and can be
  wrong about them. It is versioned and recorded with each verdict so a wrong
  sentence can be found and corrected.
