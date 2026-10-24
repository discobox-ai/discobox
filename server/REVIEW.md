# Server Review Notes

- The `github.com/Code-Hex/vz/v3` replace in `go.mod` is at or past the one
  disco-vm's own `go.mod` names, never behind it. Check it after any rebase
  that touched `go.mod`. Why: disco-vm's vz driver uses fork-only API
  (`WithMacGuestProvisioning`), so an older pin only fails CI's `darwin` job,
  which Linux cannot typecheck locally. A rebase taking main's side of
  `go.mod` already put the older pin back once.
