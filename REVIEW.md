# Review Notes

When changing code, first read the closest `DESIGN.md` and `REVIEW.md` files in
this directory and parent directories.

Global review expectations:

- Keep changes scoped to the package responsibility.
- Preserve desired-state reconciliation semantics for orchestrated resources.
- Persist accepted intent, resource changes, and durable jobs transactionally.
- Do not let provider/runtime code depend on API DTOs.
- Prefer short-lived tokens and explicit key ownership for auth flows.
- Update package-local design docs when changing architecture or data model.
