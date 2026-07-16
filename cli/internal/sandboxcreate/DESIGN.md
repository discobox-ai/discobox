# Sandbox Creation

This package owns UI-independent client-side preparation and submission of
sandbox create requests.

- Frontends provide typed options; this package resolves source refs, snapshots
  dirty local workspaces, captures local user identity, classifies environment
  and secret inputs, builds the API body, and submits prompt sandbox creates.
- Do not depend on `internal/cli` or `internal/tui`. Both frontends consume this
  package through their adapters.
- Keep terminal waiting, attach, progress output, and rendering in the frontend
  packages; those behaviors begin after sandbox creation.
