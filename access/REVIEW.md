# access review notes

- The CLI never carries, prints or asks for a credential's value, and no
  protocol call it makes returns one. A command that would — a `get`, a
  wrapper that injects a value, a request for one raw — is a new protocol
  decision, not a flag. A raw value a person handed over (ADR 26-10-27-840)
  is the sandbox agent's to deliver; the CLI only says one is held (`list`, and a
  granted `request`/`wait`, which says how it was handed over) and where its
  file is (`env`), never what it is.
- A request's name, variable, and that it names a route or host are checked
  before anything is sent, and a refusal names the flag (and the JSON key)
  that fixes it. A route itself is the service's to read: no parser here,
  neither imported (the module depends only on `agentcreds`) nor copied; the
  service's `invalid` naming the route is printed whole.
- `routeSyntax` is a copy of `secretroute.Syntax`; change both together
  (cli's `command_conventions_test.go` fails when they differ).
- A skill asks for routes as narrow as its task, not bare hosts: a host is
  exact, and `*.h` (or `*`) is a widening someone has to read and approve.
- Change `skills/discobox-access/SKILL.md` and the help in the same commit as
  the flag or field they describe.
- Every request example in a skill — this module's and `.agents/skills`'
  alike — passes this CLI's own validation: run it against a stub or read it
  against `runRequest`. A delegation names no `--env-var` (`"envVar"`), and
  two skills shipped an example that refused itself, so a lead following them
  could never hold a delegation.
