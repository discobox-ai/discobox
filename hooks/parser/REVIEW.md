# Parser Review Notes

- Keep this package focused on hook file discovery, parsing, normalization, and
  validation. Do not add daemon, runner, DB, or socket behavior here.
- Preserve Discobot-compatible front matter delimiter support: `---`, `#---`,
  and `//---`.
- Keep `.discobox/hooks` repository-root relative; parser APIs should accept an
  explicit root/path instead of discovering process-global state implicitly.
- Return precise validation errors that include the hook path and field when
  possible.
- Do not fail when the hooks directory is absent; return an empty hook list.
- Keep native AI execution out of the parser. Parse compatibility fields, but let
  execution packages decide how unsupported engines are handled.
- Preserve stable filename-derived IDs. Changing ID normalization is a data
  migration concern because status and run history reference hook IDs.
- Update `DESIGN.md` whenever the accepted hook file format changes.
