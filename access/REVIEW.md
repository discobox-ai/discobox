# access review notes

- The help shows a statement's grammar as `policy.Grammar`, never a copy of
  its own, and the skill's copy matches `pkg/policy/DESIGN.md`'s. A hand-written
  grammar in the help dropped `created by` and `of` and made the scope's name
  required, so an agent read one grammar in the help and another in the
  refusal of its sentence.
- A policy is parsed and validated before `request` sends anything, and a
  refusal is a usage error carrying the parser's or validator's own errors
  and the grammar. Parsing and validation are the `policy` package's; this
  client never decides validity itself, so the two cannot disagree.
