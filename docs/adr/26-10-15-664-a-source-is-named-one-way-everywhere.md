# 26-10-15-664 — A source is named one way everywhere

- **Status**: Accepted (`new.skills` spelled `run.skills` by [26-10-24-254](26-10-24-254-a-discobox-is-one-positional-and-flags-take-one-type-each.md))
- **Date**: 2026-10-15
- **Amends**: [26-10-14-294](26-10-14-294-a-skills-repository-is-a-source-its-skills-are-linked-from.md)
  §1, whose `URL[@REF][//PATH]` becomes this ADR's general notation and whose
  `--skills` now takes GitHub shorthand.
- **Relates to**: [0056](0056-a-repository-declares-the-sources-it-is-worked-on-with.md),
  [0111](0111-the-origin-is-the-client-and-its-key-names-where-the-source-came-from.md),
  [26-10-14-585](26-10-14-585-a-console-folder-is-a-github-repository-and-servers-are-not-sections.md),
  [26-10-14-894](26-10-14-894-a-project-names-skills-repositories-every-discobox-is-created-with.md).

## Context

A person names a source in several places: `-C`, `-i`, `--skills`,
`client.yaml`'s `new.skills`, `discobox skills add|remove`, the console's
Source field and its skills screen, and `.discobox/sources.json`. Each place
grew its own reading of the value:

| Where | Directory | Git URL | `owner/repo` | `@REF` | `//PATH` |
|---|---|---|---|---|---|
| `-C`, console Source field | yes | yes | yes | yes | no |
| `-i` | yes | yes | no | yes | no |
| `--skills`, `new.skills`, `skills add`, console skills | yes | yes | no | yes | yes |
| `.discobox/sources.json` | no | yes | no (it is expanded, but refused first) | yes | no |

The parsing is spread across `splitRunSourceRef`, `IsRemoteGitSource`,
`ExpandGitHubShorthand`, `GitHubRepositoryURL`, `GitHubRepositoryKey` and
`remoteSourceName` in `cli/internal/sandboxcreate`, `parseSkillsRepository`
beside them, `sourcecache.NormalizeRemote`, and the form check in
`sandboxconfig.ValidateSkillsRepository`.

Listings compare sources differently. The console's folders are keyed by
`GitHubRepositoryKey`, so `https://github.com/foo/bar.git`,
`https://github.com/foo/bar` and `git@github.com:foo/bar` are one folder,
`foo/bar on github`. The console's Source row and `list.sources()` compare the
raw string, so the same repository is offered as two or three choices.
`discobox skills remove` matches an entry by its raw `URL@REF`, so
`skills remove foo/bar` does not find the entry `skills add
https://github.com/foo/bar.git` filed.

## Decision

### 1. One notation: `LOCATION[@REF][//PATH]`

A **source reference** is written `LOCATION[@REF][//PATH]`.

**LOCATION** is one of:

- **A directory**: absolute, relative, `~`-prefixed, or a Windows path.
- **A Git URL**: `https`, `http`, `ssh`, `git`, `git+ssh`, `ssh+git` or `file`
  URLs, or git's scp-like `[user@]host:path`.
- **GitHub shorthand**: `owner/repo` or `github.com/owner/repo`, which stands
  for `https://github.com/owner/repo.git`. The directory wins: a shorthand
  that is also an existing path on this machine names that directory, as `-C`
  already does. Shorthand is GitHub's alone. A `host/path` form for other
  hosts would claim every relative path whose first segment holds a dot.

The directory wins over `@REF` and `//PATH` too: an existing directory whose
own name holds `@` or `//` (a synced folder named after an account, such as
`GoogleDrive-me@example.com`) is that directory, not one at a ref or a path.

**@REF** is a branch, tag or commit, split as `splitRunSourceRef` splits it
now: at the last `@`, unless that `@` is the user part of an scp-like URL.

**//PATH** is a directory inside the source. It starts at the first `//`
after the URL's `://`, if the URL has one, as ADR 26-10-14-294 §1 settled for
skills. It is clean, relative, slash-separated, and has no `..` segment.

### 2. Every entry point reads it with one parser and says what it takes

Each entry point accepts the whole grammar and refuses, by name, the parts it
has no meaning for. Nothing has its own parser.

| Where | Directory | Git URL | Shorthand | `@REF` | `//PATH` means |
|---|---|---|---|---|---|
| `-C`, console Source field | yes | yes | yes | yes | where the harness starts (§3) |
| `-i` | yes | yes | yes | yes | refused |
| `--skills`, `new.skills`, `skills add\|remove`, console skills | yes | yes | yes | Git only | the skills directory (ADR 26-10-14-294 §1) |
| `.discobox/sources.json` | refused (ADR 0056) | yes | yes | yes | refused |

A skills directory is read as content, not as a checkout (ADR 26-10-14-294
§1), so `@REF` on it is refused rather than ignored.

### 3. `-C SOURCE//PATH` starts the harness in PATH

`//PATH` on the primary source is the directory inside it that the harness
starts in: `destination.workingDirectory`, at PATH beneath where the source
is placed. The create request already carries this field, so there is no API
change. Until now nothing inside the sandbox read it, so even a local
`-C repo/sub` started at the repository root. The pool agent passes it into
`sandbox.json` as the primary source's `workingDirectory`, but only when it is
inside the source. The sandbox agent starts execs there (terminals, the
harness, `discobox shell`). It still reads what the repository declares
(services, tools, `.discobox/skills`) from the source's own directory. A
sandbox or pool from before this has no `workingDirectory` and starts at the
source, as before.

- **A local source**: `-C DIR//PATH` is `-C DIR/PATH`, which already starts the
  harness in that subdirectory of the repository that holds it. PATH must
  exist, and the client checks.
- **A remote source**: the client has no checkout of it at create time. PATH
  is checked when the client delivers the source (ADR 26-10-10-545), and a
  commit that does not hold it fails the delivery with PATH named.

### 4. A reference has one identity and one display

A parsed reference has:

- **A key**, which says whether two references are the same source. A remote
  is keyed by its normalized remote (`sourcecache.NormalizeRemote`: host
  lower-cased, default port, scheme, user and `.git` dropped). GitHub's
  `owner/repo` is also lower-cased, since GitHub does not distinguish names by
  case. A directory is keyed by its absolute path. The key includes `@REF` and
  `//PATH`.
- **A repository key**: the key without `@REF` and `//PATH`. One repository
  at two refs is refused by it, on the client and on the server. The console's
  folders keep the key ADR 26-10-14-585 gave them, the lower-cased
  `owner/repo`, because saved views already hold it.
- **A clone URL**, kept as it was written. Transport is not identity: ssh and
  https name one repository but reach it with different credentials.
- **A clone key**: the key with the transport (https, http, ssh, git, file).
  Two references with one clone key are fetched the same way.
- **A display form**, which is itself a valid reference that parses back to
  the same clone key:
  - A GitHub repository reached over https displays as
    `owner/repo[@REF][//PATH]`, which is the URL that shorthand stands for.
  - Any other remote, including GitHub over ssh, http or `git://`, displays as
    its URL without `.git`, so its transport stays visible.
  - A directory displays as an absolute path, with the home directory as `~`.
    A directory's display is always rooted, so it cannot be read as GitHub
    shorthand.

  A directory whose own name holds `@` or `//` reads back as itself only
  through the resolve step (§6), since only the disk tells that name from a
  ref or a path. A reader that does not resolve takes such a directory from
  the one that did, and never splits it off a value itself.

A list of sources to cut from, such as the console's Source row and
`list.sources()`, dedupes by clone key. Deduping by key would offer the https
spelling and run another discobox's ssh, http or `git://` one, which is a
transport nobody chose. A list of stored entries, such as `discobox skills
list` and the console's skills screen, shows each entry in its display form
and dedupes nothing: each entry is a record of its own, removed by its own ID.
Matching a typed reference against stored ones, as `skills remove` and the
server's check for one repository at two refs do, compares keys.

`on github` is a header's word. The console's folder header names a GitHub
repository as `owner/repo on github` (ADR 26-10-14-585), and nothing else
appends it. Everywhere else the display form appears bare, so what is on screen
can be typed back.

### 5. Clients send the canonical spelling; stored values are read through the parser

A client sends a reference with shorthand expanded and a directory made
absolute. Values already on the server, such as sandboxes' sources and
project skills sources, are not migrated: they are parsed when read, so an
old `https://github.com/foo/bar.git` and a new one dedupe and display alike.
The server still holds a project skills source to
`sandboxconfig.ValidateSkillsRepository` (ADR 26-10-14-894 §1). The client
expands shorthand before sending, so the server never sees shorthand.

### 6. The parser is one root-module package

The notation lives in a root-module package, `sourceref`, because both the
CLI and the server's validation read it. It replaces the parsing functions
listed in Context and its key absorbs `sourcecache.NormalizeRemote`. Parsing
is pure syntax. The directory-wins check and `~` expansion are a separate
resolve step that reads this machine's disk, so the server can parse without
a filesystem.

## Alternatives rejected

- **Ref as a query or fragment** (`URL?ref=main`, Terraform/go-getter style;
  `URL#main`, npm style). `@REF` is already what `-C`, `-i` and
  `sources.json` take. A query or fragment is also part of a valid URL, so
  some remote's real URL could hold one.
- **Shorthand for any host** (`gitlab.com/group/project`). Any relative path
  whose first segment holds a dot would turn into a URL whenever the
  directory is missing, which turns a typo into a network fetch. Non-GitHub
  remotes are written as URLs and displayed as URLs.
- **`owner/repo on github` as the display everywhere.** It reads well, but it
  cannot be typed back in. It is kept for the folder header, which names a
  place rather than a value.
- **Rewriting stored values to the canonical spelling.** Normalizing on read
  gives the same listings without a migration. It also keeps the clone URL a
  person wrote, which matters because transport carries credentials.

## Consequences

- `-i foo/bar`, `--skills foo/bar@main//skills` and
  `discobox skills add foo/bar//skills` work, and the console's skills screen
  takes them.
- One repository is one choice in the console's Source row, however its
  discoboxes spelled it over one transport. The same repository over another
  transport is a second choice, shown by its URL.
- A `.discobox/sources.json` entry may be `owner/repo`. It resolves the same
  way on every machine, because a declared source is never a directory.
- `-C https://github.com/o/r//sub` is new. A remote PATH that is missing is
  only discovered at delivery, after the create has been accepted.
