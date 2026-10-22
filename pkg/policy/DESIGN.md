# policy

What a credential grant authorizes: statements, the definitions of their words,
and what their variables resolved to at approval. See
[ADR 26-10-20-782](../../docs/adr/26-10-20-782-approved-access-is-a-policy-decided-at-the-network-boundary-fast-path-first.md)
§§1, 2, 4. This package is the contract and the code that needs no reading;
nothing here decides whether a request is one of a statement's actions.

## What is here

| File | What it holds |
| --- | --- |
| `policy.go` | `Policy`, `Statement`, `Resource`, `Scope`, and `Validate`: the shape a person approves, and every way one is refused; `ValidateDefinitions`, the part a reader of bound statements still asks. `Words` and `Variables` list the words and the variables the statements use. |
| `evaluate.go` | `Match`, `Parts`, `Atom`, and `Evaluate`: decided parts of each statement, and whether the atom is a supporting read, per atom of a request, combined in the two phases of §2. |
| `sentence.go` | `Parse` and `Format`, and `ParseStatements` and `FormatStatements` for a policy's list: a statement's sentence form, and its structure, converted without loss. `Tokens` is that sentence as its tokens, each named for what it is (`Join(Tokens(s))` is `Format(s)`), for a reader that highlights it; `FormatScope` and `ScopeTokens` are a scope as a sentence writes it, for a rendering that groups by scope. |
| `within.go` | `Within`: whether a policy is within the one it is held to — a policy handed on within the one delegated (ADR 26-10-20-782 §11), or the policy a request asks for within an approval rule's (ADR 26-10-22-689) — each side as its own values resolve it: every allow covered, every deny of the other carried, shared words defined alike, and the allows whose conditions only a judge can compare (`Unsettled`). |
| `written.go` | `Written`, a policy from its statements' sentences and its definitions, parsed and validated, and `Definitions`, the `--define WORD=DEFINITION` flag value: what discobox-access and the CLI take a policy in by, one way. |
| `facts.go` | `Facts`, the shape of the control plane's record of a discobox (§4; built by the server's `Sandbox.PolicyFacts`), the variables a policy may use, `Variables` listing each with its value for an agent to read, `Resolve` at approval, `Bind` at a decision, and `Bound`: the statements as a reader at that decision sees them. |

It imports only the standard library, so `agentcreds` can carry a policy and
stay portable.

## The rules it exists to keep

**Structure is fixed; words are open.** Actions and types are any words. Every
word but the defaults — `read`, `create`, `update`, `delete`, `manage`, and
`anything`/`anywhere` — has a definition in the policy, and the defaults cannot
be given one, so no definition quietly widens `read`. A definition no statement
uses is refused: a person reads every one.

**An allow names where it applies.** Only a deny may say `anywhere`. A
condition is present and non-empty, or absent. No word, definition or
condition holds a control character: all of it is shown to the person
approving it.

**Order never matters.** `Evaluate` allows when every atom matches some allow,
or is a supporting read, and no atom matches any deny. A known deny refuses
outright; an atom every allow is known not to cover, and known not to be a
supporting read, is refused; anything unknown that could change the answer
leaves it `Undecided`, for whatever decides next. Only a judge says an atom is
a supporting read (§2); a fast path says `No` for a write and leaves a read it
cannot place `Unknown`. Callers supply the
parts — a fast path, a judge — and never combine them themselves.

**Variables are a whole name, from the record, never the discobox.** `$this`,
`$primary.repo|host|branch`, `$sources.<slug>.repo`, and the observed
`$primary.upstream` and `$primary.default_branch` — which resolve to nothing
until observed facts exist. A variable that does not resolve matches nothing.

**Resolved at approval, bound at a decision.** `Resolve` records each
variable's value against the discobox the grant is for; the server does it when
it mints a grant, and ignores any `resolved` a client sends. `Bind` reads the
facts again: a variable matches only when it resolved then and resolves now to
the same value. A policy handed to another discobox resolves and binds against
that discobox.

**What a judge reads is bound.** `Bound` writes each variable that binds as
its value, so a judge reads `repository "acme/web"`, never a variable it would
have to resolve itself. An allow with a variable that does not bind covers
nothing and is left out; a deny with one is kept as written, since a reader
that cannot tell it matches nothing refuses more, never less. Definitions go
with the statements that use them.

## Sentences

A statement has a sentence form, for agents to write and people to read, and
its structure, which is what a policy holds. `Parse` and `Format` convert
between them without loss: `Parse(Format(s))` is `s` for every statement `Validate`
accepts, and `Format` is canonical.

```
statement := ("allow" | "deny") action ("," action)* "on" resource ("within" resource)* ["in" scope] ["when" quoted] ["unless" quoted]
resource  := type [name] ["created" "by" name]
scope     := type [name] ("of" resource)*
action    := word+
type      := word+
name      := quoted | variable
```

- **A word is unquoted; a name is quoted or a variable.** So a multi-word type
  is unambiguous: `read on pull request` is the type `pull request` with no
  name. `in repository acme/web` reads `acme/web` as part of the type, and
  `Validate` says the scope has no name.
- **Keywords end a word**: `on`, `within`, `in`, `of`, `when`, `unless`,
  `created`. An action or type is printable ASCII, one space between its
  parts; `Validate` refuses one that holds a keyword, a quote, a comma, a `$`
  or two spaces — anything a sentence could not read back, or would show as
  something else. Outside ASCII there is no list of lookalikes to refuse.
  `anything` and `anywhere` are ordinary words here: `deny delete on anything
  in anywhere`.
- **`within` and `of` read inside out**; the structure keeps parents outermost
  first. `within issue "43" within project "p"` is `Within: [project, issue]`.
- **Quoted text is a Go string literal**, escapes and all. `Format` writes it
  in ASCII (`strconv.QuoteToASCII`), so quoted text cannot seem to end early;
  `Parse` reads any rune, escaped or not. Conditions are always quoted; only a variable a
  policy may use is written bare.
- **Parsing does not validate.** `Parse` reads structure only, and a statement
  with no scope parses. A malformed sentence is a `SyntaxError` naming the
  token, its byte offset, what was expected, and `Grammar`, because an agent
  reads it and tries again.
