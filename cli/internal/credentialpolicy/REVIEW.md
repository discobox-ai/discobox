# Credential policy review notes

- Bound expanded output before appending substitutions, across every path in a
  policy. The input-file bound alone does not bound output: a small template
  can repeat a large default many times and exhaust the CLI before validation.
