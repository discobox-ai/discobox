# Hooks Review Notes

- Keep this module standalone. Do not import server internals, control-plane DTOs,
  or Discobot `agent-go` packages.
- Preserve session scoping: one daemon/socket/database namespace per session.
- Keep daemon-owned DB writes authoritative. CLI commands should use the Unix
  socket API for mutations.
- Use `gormdb` for DB opening and keep migrations/models compatible with SQLite
  first.
- Preserve daemon-configured bounded parallel hook execution. A failed hook must
  block future queued hook launches; hooks already in flight may finish.
- Treat `.discobox/hooks` as repository-root relative. Resolve and validate the
  Git root before discovery or watching.
- Respect Git ignored files before matching hooks.
- Keep native AI execution out of the initial implementation. AI behavior should
  be represented as script hooks calling external tools.
- Avoid shell-side database mutation for generated Git hooks; route through the
  CLI/daemon.
- Keep audit event docs current. `api.KnownEventTypes` is the user-facing event
  catalog for `discobox-hooks events --list-types`; add every new production
  `recordEvent`/`hook_events` type there with a description. The
  `TestKnownEventTypesDocumentProductionEvents` CLI test guards daemon/store
  event literals.
- Update `DESIGN.md` for architecture or data model changes; update `PLAN.md`
  for phased implementation decisions and open questions.
