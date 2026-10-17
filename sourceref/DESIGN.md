# sourceref

How a source is named, and when two names are one source
([ADR 26-10-15-664](../docs/adr/26-10-15-664-a-source-is-named-one-way-everywhere.md)).

- **One notation**: `LOCATION[@REF][//PATH]`. LOCATION is a directory, a Git
  URL (scheme and host, `file://`, or scp-like `user@host:path`), or GitHub
  shorthand (`owner/repo`, `github.com/owner/repo`). `//PATH` starts at the
  first `//` after `://`; `@REF` is the last `@` past the URL's host (or the
  scp colon), so a user is never a ref.
- **Parse is syntax; Resolve reads the disk.** `Parse` touches no
  filesystem, so the server can use it; a shorthand stays `Shorthand` until
  `Resolve(base)` makes it the directory there or the GitHub URL, and a
  directory that is there wins whatever its name holds (`@`, `//`). `Expand`
  makes a shorthand the URL unconditionally, for places that never name a
  directory.
- **Identity is not transport.** `Key` compares sources: GitHub as
  lower-cased `github.com/owner/repo`, another remote by `NormalizeRemote`,
  a directory by its path, each with `@REF` and `//PATH`; `RepositoryKey`
  drops those two. `CloneKey` adds the transport, and is what a list of
  things to cut from dedupes by: ssh vs https decides the credential, and
  http or `git://` carry no protection, so merging across them runs a URL
  nobody chose. The URL as written is kept.
- **Display round-trips.** `Display(home)` is itself a reference with the
  same `CloneKey`: `owner/repo` for GitHub over https (what that shorthand
  stands for), any other remote's URL without `.git`, a directory under the
  home the caller names from `~`. A directory whose own name holds `@` or
  `//` reads back as itself only through `Resolve`; a reader that only parses
  takes such a directory from the one that resolved it. `on github` is the
  console's folder header's word, not this package's.
- What each entry point accepts of the grammar (`-C`, `-i`, `--skills`,
  a `settings.json` source) is the caller's to say; see
  [`cli/internal/sandboxcreate/DESIGN.md`](../cli/internal/sandboxcreate/DESIGN.md).
