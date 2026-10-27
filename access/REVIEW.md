# access review notes

- The CLI never carries, prints or asks for a credential's value, and no
  protocol call it makes returns one. A command that would — a `get`, a
  wrapper that injects a value — is a new protocol decision, not a flag.
- A request's name, variable and hosts are checked before anything is sent,
  and a refusal names the flag (and the JSON key) that fixes it.
- Change `skills/discobox-access/SKILL.md` and the help in the same commit as
  the flag or field they describe.
- Every request example in a skill — this module's and `.agents/skills`'
  alike — passes this CLI's own validation: run it against a stub or read it
  against `runRequest`. A delegation names no `--env-var` (`"envVar"`), and
  two skills shipped an example that refused itself, so a lead following them
  could never hold a delegation.
