# CLI command review notes

- Approval history renders empty route lists by their source: an empty binding
  or parent list adds no restriction, while an empty secret list allows nothing.
  A common route formatter calls either "nowhere", which misrepresented an
  explicitly approved anywhere grant. Keep the original ask separate from the
  recorded consent lists, even after the binding is gone.

- Keep `sandboxEventData` in `events.go` aligned with the event schema in
  `server.yaml`: `discobox events -o json` decodes and re-encodes through it,
  so an omitted field is silently dropped, as the first `errorMessage` change was.

- A plain `404 page not found` from a route forwarded to a box can be the
  server's, the pool agent's or the box's agent's, whichever predates the
  route. Name every one that may, as `errNoFilesAPI` does, and the remedy that
  fits each; `admin box reprovision` first blamed only the box's agent and sent
  people to an upgrade that cannot help a server or pool that is behind.

- `indentedSize` must stay an upper bound on exactly what `json.Indent`
  writes, since every layout of a box's JSON (`diffText`, `indentAuditJSON`)
  is bounded by it: the whitespace indenting drops (before and between tokens)
  may be taken off, the whitespace it keeps (after the last token) may not.
  `TestIndentedSizeIsAnUpperBound` holds it; add a case for any change to the
  scan. Its first fix counted a pretty-printed document twice, and the next
  under-counted a trailing run of spaces.

- **Sanitize harness names before using them as inline picker labels.** A name
  can come from a bundle or a project configuration and contain terminal
  control sequences. Apply `terminalSafe` before handing it to the picker.

- **An elevated step writes nothing to a path its unelevated parent chose.**
  `lsp-exempt-elevated` runs as administrator from arguments a medium-integrity
  parent passed; a file the parent names (an error file in `%TEMP%`, say) can
  be swapped for a link by any program running as that user, which turns the
  write into an elevated overwrite of a file of its choosing. Report through the
  exit status, and have the parent read the result back from Windows. The first
  fix for the hidden window's lost error did exactly this.
