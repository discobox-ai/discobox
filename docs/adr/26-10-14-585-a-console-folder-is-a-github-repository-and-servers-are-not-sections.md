# 26-10-14-585 — A console folder is a GitHub repository, and servers are not sections

- **Status**: Accepted
- **Date**: 2026-10-14
- **Supersedes**, in part:
  [0111](0111-the-origin-is-the-client-and-its-key-names-where-the-source-came-from.md)
  §3's "the launcher's header filters by the origin key", for a discobox whose
  source resolved to a GitHub repository; and
  [26-09-30-188](26-09-30-188-the-consoles-filters-open-on-everything-and-are-kept-per-directory.md)
  §1's sectioning "by folder and server (`<folder> on <server>`) while it shows
  every server". 0111's key, `discobox ls`, `apply` and the rest of §3 stand,
  and so do 26-09-30-188's opening view and per-directory filters.

## Context

ADR 0111 files a discobox under an origin key: the machine, and the directory
or URL its source came from. The console's header folder is that key, so one
repository is as many folders as there are places it was cut from:
`~/src/disco2`, `discobox-ai/discobox` (the shorthand, which is the URL),
`https://github.com/discobox-ai/discobox` and the same with `.git` are four
entries on the filter's card and four sections under "all folders", and the
same clone on two machines is two more. The person reads them as one project.

ADR 26-09-30-188 §1 sections "all folders" by folder and server at once
(`<folder> on <server>`) while every server is shown. With several servers that
splits one repository's discoboxes across a band per server, which is not the
grouping the list is read for.

## Decision

1. **A discobox whose source resolved to a GitHub repository is in that
   repository's folder.** The repository is the one already recorded for an
   `issue=`/`pr=` link: the URL a remote source clones, or the upstream a local
   checkout recorded when it was cut. Its key is `owner/repo` lower-cased, as
   GitHub compares names, and it is computed by the creation path
   (`sandboxcreate.GitHubRepositoryKey`), not the console. The folder is named
   `owner/repo on github`, is one entry on the filter's card and one section,
   across every machine and server.
2. **Anything else keeps its origin-key folder** as 0111 has it: a source on
   no GitHub repository, and a discobox with no source.
3. **The window's own folder is the repository its checkout resolves to now**,
   as a create from it would record it; it also holds the discoboxes filed
   under the window's key that resolved to nothing. A discobox stays in the
   repository it resolved to when it was cut.
4. **The list is sectioned by folder only.** No band names a server over its
   rows. Which server a discobox is on is answered by narrowing the header to
   that server; a server that did not answer, or is still listing, is still
   said where its rows would be.

## Alternatives rejected

- **Keep the origin key (0111 §3).** 0111 rejected resolving a checkout to its
  remote because it "groups different places … and `apply` goes into exactly
  one of them". That holds for `ls` and `apply`, which act on a place, and they
  keep the key. The header is a filter: merging clones in it sends nothing
  anywhere, and a discobox's own source, which is what `apply` uses, is
  unchanged.
- **Normalize the origin key itself.** 0111 rejected it because the origin key
  and the pool's source-data key would differ for one repository. Grouping
  only in the console leaves both keys as they are.
- **Keep `<folder> on <server>` sections, or put a server cell on the row.**
  The user chose filtering to a server over showing one on every row; a server
  column repeats one value down the list for anyone with a single server and
  spends a name's cells for everyone else.
- **List each spelling of the source on the card**, grouping only the
  sections. A card offering four entries for one repository is what this
  replaces. The run options' Source row does keep each source: a clone and the
  URL are one place to list and two things to cut from.

## Consequences

- The console's header no longer always agrees with `discobox ls` in the same
  directory: `ls` lists the origin key, the header the repository.
- The window's own folder reads `owner/repo on github` rather than
  `~/src/foo @ main` when the checkout is on GitHub; the Source row says what
  is cut.
- In a fork, discoboxes cut while the branch tracked the upstream are in the
  upstream's folder, not the window's.
- A view saved on an origin key whose discoboxes now resolve to a repository
  still lists them, under its saved label, until the header is moved; one
  saved on the window's own key opens on the window's repository.
- Rows from several servers interleave under one band; nothing on a row names
  its server.
