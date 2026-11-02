# Credential policies

This package owns the file format and argument resolution for reusable
credential permissions (ADR 26-10-30-584). It has no API or storage ownership:
callers persist the routes it returns. Two callers resolve through it:

- **The CLI** reads a person's files (`References`) before writing a binding.
- **The server** resolves the documents a credential request carries
  (`ResolveDocument`, ADR 26-11-02-022 §2). Input from a box, so every bound
  below is a boundary, not a courtesy. `Resolved` also carries what an approver
  is shown beside a role: the document's description, a digest of its canonical
  JSON (so copies group whatever their spelling), and the arguments no route
  path names.

- `References` preserves policy order and scopes each argument to the preceding
  file. `Resolve` concatenates the independently resolved route lists. The
  existing specificity and blocked-route rules apply across that one merged list;
  secret and parent delegation restrictions remain separate lists.
- Files are JSON version 1, strictly checked for exact field names, duplicate
  keys, types, nulls, depth, UTF-8, and a size bound before decode. Empty route
  lists fail because an empty binding route list would inherit permissions.
- Parameters are required unless `optional: true` has an explicit `default`.
  `pathSegment` values are literal ASCII URI-unreserved characters, excluding
  dot traversal segments; percent encoding, separators and glob syntax fail.
  Empty values may fill part of a segment, but may not remove a whole segment.
  Only paths expand placeholders; no recursive substitution or expressions.
- Expanded paths have a cumulative 1 MiB budget per policy, checked before
  each append so repeated placeholders cannot amplify input into unbounded output.
- Final routes use `secretroute.Normalize`. API conversion remains with
  the CLI's `internal/apiroute`. `cli/credential-policy.schema.json` publishes the editor
  schema; runtime validation remains authoritative for resolved paths and routes.
