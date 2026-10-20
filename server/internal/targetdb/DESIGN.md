# targetdb

The target database's contract: what is known about a host. See
[ADR 26-10-20-782](../../../docs/adr/26-10-20-782-approved-access-is-a-policy-decided-at-the-network-boundary-fast-path-first.md)
§6. The server serves and stores entries in
[`resources/targets`](../resources/targets/DESIGN.md).

## What is here

| File | What it holds |
| --- | --- |
| `entry.go` | `Entry`, `Document`, `Term`, `Shape`; `Validate`, every way one is refused; `Version`. |
| `lookup.go` | `Entry.Lookup`, a statement's known shapes per action, and `For`, the narrowest entry that covers a host. |

## The rules it exists to keep

**An entry is keyed by a host it covers.** `github.com` is also
`api.github.com`, the rule a credential's host follows (`hostscope`). Of the
entries that cover a host, the one with the longest host answers. An empty host
would cover everything, and is refused.

**Documents are text, named by digest.** Nothing here parses one. A document
with a URL pins a version, and its digest is what the URL must answer.

**Terms are the host's words.** Each has a definition. A default action or a
reserved word cannot be a term, so no definition widens `read`. Terms no shape
uses are allowed: they are what the host offers.

**A shape is a statement with its names as parameters.** It is an action on a
resource type, within parent types, in a scope type, plus the documented
operations it covers. The effect, names, conditions and creator are not part of
it, since the rules that recognize a request are the same for each of them.
Every word a shape uses is a default action or one of its entry's terms of the
right kind. Its action is never `manage`: that is every action, which a
statement may say and Lookup widens, but a shape is one action's.

**Lookup is code.** An action fits a shape when its action, resource type,
parent types, scope type and scope parent types are equal. `manage` fits every
shape of the type, `anything` every type, `anywhere` every scope. An action with
no shape is `Missing`. That is the caller's cue to read the documents. It is
never a refusal.

**The version names what an entry says about its host.** It covers the host,
each document's name, kind and digest, and the terms, in no particular order. A
new URL for the same text keeps the version. The shapes are left out: with the
version, a shape whole — its operations included — is what generated rules are
kept under (§7), and adding or changing one must not orphan the rules every
other shape already has.
