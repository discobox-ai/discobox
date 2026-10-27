# 26-09-27-905 — The command judge is shown a bounded prefix of stdin

- **Status**: Superseded by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
- **Date**: 2026-09-27

## Context

`discobox-access run` asks a model whether the argv it is about to execute is
the use a human approved ([ADR 0079](0079-a-local-judge-gates-every-wrapped-credential-use.md)),
and the judge is handed that argv and a few facts
([ADR 0090](0090-the-judge-is-handed-facts-and-given-no-tools.md)). Nothing it
is told says what the command will read on stdin.

For a growing set of commands, stdin is the operation. The CLIs an agent drives
take a structured request on stdin precisely so free text never passes through
a shell: `discobox new --json`, `discobox-access request --json`,
`gh api --input -`, `gh pr create --body-file -`, `kubectl apply -f -`. The
discobox skill tells an agent to create a discobox exactly that way:

```
discobox-access run --use <id> -- discobox new --json <<'EOF'
{"prompt": "...", "grants": [...]}
EOF
```

The judge saw `[0] discobox [1] new [2] --json` and refused: it "lacks the
required arguments to scope each discobox to its own issue". It was right that
it could not tell; it was shown nothing to tell from. The documented pattern
cannot pass the judge that guards it.

## Decision

1. **`run` reads a bounded prefix of stdin before it judges, and shows it.**
   When fd 0 is a regular file or a pipe, `run` reads up to
   `maxJudgedStdin` (8 KiB) before asking the judge, and the prompt carries
   what it read as the input the command will read — labeled untrusted, like
   the argv, inside a fence drawn fresh for each run so the input cannot
   close it.
2. **What is not shown is said.** Input longer than the bound, input still
   arriving after `stdinArrivalWait` (5 s), and input that is not UTF-8 text
   are each named in the prompt, with the byte count read, rather than cut
   silently. The judge weighs what it was not shown, as the request judge does
   with a body's `missing` ([ADR 26-09-26-240](26-09-26-240-a-request-is-recognized-and-its-body-is-shown-in-one-shape.md) §2).
3. **The command reads exactly what was sent.** The child's stdin is the bytes
   `run` read, followed by the rest of fd 0, in order. Reading ahead changes
   when the bytes arrive, never which bytes. The child is handed an OS pipe
   that `run` feeds, not a reader for exec to copy: exec waits for its copy
   before it returns, so a writer that keeps stdin open would hold `run` open
   after its command had exited.
4. **Only a file or a pipe is read.** A terminal, a socket, a character device
   (`/dev/null` included) and anything else pass straight through, unread and
   unmentioned. An agent's shell tool commonly hands its commands an open
   socket that never ends; reading it would add the arrival wait to every
   `run` and take input meant for an interactive child.
5. **The verdict record carries it, as it carries the argv.** The input shown
   is part of the prompt, and the prompt is what
   [ADR 0091](0091-a-credential-is-not-issued-without-a-verdict-on-record.md)
   records, so the audit shows what the judge read. That record travels inside
   one protocol body (`agentcreds.MaxBodyBytes`, 64 KiB), and JSON can write a
   byte as six, which is what sets the bound at 8 KiB — the request judge's
   own (`judge.MaxBodyBytes`), for the same reason.
6. **Stdin is shown unredacted, like the argv.** In a discobox no credential
   value enters the sandbox (ADR 0031): what an agent can put on stdin it could
   equally put in the argv, which the judge has always been shown and the
   record has always kept verbatim. A command that reads a real secret on
   stdin — `docker login --password-stdin` with a password the agent holds —
   therefore records it, exactly as `docker login -p` always has.

The judge stays a guardrail and not a boundary (ADR 0079): a command whose
stdin shapes a request is still judged again where the request leaves, by the
pool's judge with the request itself in front of it.

## Alternatives rejected

- **Tell agents to pass everything as argv.** It moves free text back through
  the shell, the problem `--json` on stdin exists to solve (the interface table
  in `access/DESIGN.md`), and the argv form is not available for every tool: a
  PR body with backticks, a manifest, a JSON document with apostrophes.
- **Judge the argv and let stdin through unseen.** The judge cannot say what
  `discobox new --json` does from its argv; it refuses, as it did, or it allows
  what it has not read, which is worse.
- **Read all of stdin first.** Stdin can be a bundle or an archive of any size,
  or a pipe that never closes, and the prompt is recorded inside a 64 KiB
  protocol body. A bound is not optional; the question is only whether what
  lies past it is said, and it is.
- **Hand the judge a file to read.** The judge has no tools (ADR 0090), and
  giving it one to read stdin with would give it one to read anything.
- **Redact stdin before showing it.** The only redaction that would help is
  by name — credential-named JSON keys and form fields, as the pool does for a
  request body — and it would need that heuristic in a module that is
  deliberately stdlib-only, catch only the names it knows, and still miss a
  password piped bare. It would also make stdin safer to show than the argv
  beside it, which it is not. Redacting nothing, and saying so, is the honest
  line.
- **Read stdin whatever fd 0 is.** See §4: the common case in an agent's shell
  is a socket that never ends, and a terminal is a person typing.

## Consequences

- A command fed from a pipe that stays open without ending waits up to
  `stdinArrivalWait` before it is judged. A file, a here-document and a
  finished pipe are read as fast as they arrive.
- A child sees a pipe on stdin where it may have seen a file, so a command that
  seeks on its stdin no longer can. None of the CLIs above does.
- The recorded prompt grows by up to 8 KiB for a command that reads stdin.
- A secret an agent pipes into a command is recorded with its verdict, as one
  in its argv is. Outside a discobox, where a caller may hold real secrets,
  that is the cost of judging a command by what it does.
