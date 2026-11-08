# Public documentation import contract

Product prose is authoritative in this repository. Site owns presentation,
navigation, version selection and committed snapshots. Never publish `docs/`
recursively: ADRs, development notes and credentials protocol documents are
outside this allowlist.

## Public input allowlist

The complete initial import is these exact files, with no assets:

| Source path | Suggested snapshot page |
| --- | --- |
| `docs/public/index.md` | `index` |
| `docs/public/getting-started.md` | `getting-started` |
| `docs/public/concepts.md` | `concepts` |
| `docs/public/console.md` | `console` |
| `docs/public/cli.md` | `cli` |

All relative manual links target another allowlisted page. The importer owns
rewriting them to its snapshot routes and must reject a missing input or an
unresolved internal link. Edit links point back to the source path in this
repository. Preserve source attribution. This contract itself is maintainer
material, not a public page to import.

Pages state no release status and hard-code no release line: the site's
snapshot manifest shows both, and an import cannot edit product prose, so a
page that called its line a prerelease would read wrong once that line is
stable. The exporter's tests (`TestPublicPagesNameNoReleaseStatus`) fail when
a page under `docs/public/` says "prerelease", other than describing the edge
installer's channel.

## Generator v1

Use the Nix toolchain and the pinned checkout, from the repository root:

```sh
nix develop --command go tool task docs:generate
nix develop --command go tool task docs:check
```

The targets invoke `go run ./cli/internal/cmd/discobox-docs` and the same command with
`-check`. `-output PATH` selects a different output for comparisons. Check
fails on missing or different bytes and does not rewrite the file. Generation
also runs through `generate`, so the ordinary CI generated-file check detects
stale CLI docs. The exporter format version is **discobox-docs v1**; changes
to its output contract (the page's structure: headings, flag tables, fenced
help) require updating this version and the site import. Changing the prose of
its fixed header is not a contract change.

It constructs the actual CLI tree without executing commands or talking to a
server, removes `DISCOBOX_*` environment overrides, fixes the name to
`discobox`, sorts commands and flags, and emits no timestamps. Hidden spellings, hidden flags and user-declared tools are intentionally
excluded. Only embedded tool definitions are read, and the tools config
directory is rendered symbolically. Public aliases, help/completion,
local/persistent/inherited flags, types, defaults, descriptions and implicit
flag values are included. Provider-specific flags are runtime catalog fields,
not compiled CLI definitions; the public concepts page explicitly defines
that coverage boundary and teaches the matching server's `--help=TYPE`.

## Pinning a release snapshot

Import from a release tag. Record **that tag and its full resolved commit
SHA**, not a moving branch, a triage revision or a SHA guessed before merge. Resolve annotated tags to their
commit (`git rev-parse TAG^{commit}`). Run generation and check at that exact
revision in a trusted maintainer environment, then import the committed output.
Do not fetch or execute product code during normal site builds or fork PR CI.

A site's committed import manifest records at least:

- Product repository, release line (`v0.19`), exact tag and full commit SHA.
- Release status (`prerelease` until explicitly promoted).
- Every source-to-destination file mapping from the allowlist above.
- Generator version (`discobox-docs v1`) and exact invocation
  (`go run ./cli/internal/cmd/discobox-docs`).
- Review evidence: commands/flags checked, observed console workflows and
  keybindings, and any platform behavior not exercised.

The site's import PR must be reviewed before the release's docs become
current. Keep v0.19 snapshots labeled prerelease while the product release is
a prerelease, even for a dot tag. Promoting the product is a separate human
decision; the site's stable target changes only in an explicit reviewed
versions-manifest change after stable availability is confirmed. A prerelease
tag alone never changes it. Patch updates replace the minor line's snapshot
through another pinned import PR, retaining provenance in Git. Old snapshots
are never regenerated from current main.
