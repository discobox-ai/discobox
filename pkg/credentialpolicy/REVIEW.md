# Credential policy review notes

- Bound expanded output before appending substitutions, across every path in a
  policy. The input-file bound alone does not bound output: a small template
  can repeat a large default many times and exhaust the CLI before validation.
- The server resolves documents a box sent (ADR 26-11-02-022 §2): a bound
  that was only a courtesy to a person's own CLI is now the boundary against
  a hostile one. Never loosen one, and keep `ResolveDocument` the only path
  a document reaches routes by, so what is stored and what is shown come
  from one parse.
