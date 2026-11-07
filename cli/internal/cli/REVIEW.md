# CLI command review notes

- Approval history renders empty route lists by their source: an empty binding
  or parent list adds no restriction, while an empty secret list allows nothing.
  A common route formatter calls either "nowhere", which misrepresented an
  explicitly approved anywhere grant. Keep the original ask separate from the
  recorded consent lists, even after the binding is gone.

- Keep `sandboxEventData` in `events.go` aligned with the event schema in
  `server.yaml`: `discobox events -o json` decodes and re-encodes through it,
  so an omitted field is silently dropped, as the first `errorMessage` change was.
