# sandboxcreate — review rules

- **What a repository's own files name is confined before it touches this
  machine.** `.discobox/settings.json` is repository content: a box can push
  the commit a create reads, and a repository can come from anyone. A declared
  source's name is joined onto the primary's parent to find the checkout beside
  it, so it is one path element and nothing else (`checkDeclaredSources`); a
  name of `..` reached the home directory, and once copying a directory in no
  repository became the default answer, it would have carried `~/.ssh` into the
  box. What stands in locally for a declared repository is a checkout of one,
  never a plain directory that would be copied whole.
- **A default that carries more into a box is checked against every way a
  source arrives.** `run`'s SOURCE, `-i`, and a declared source all resolve
  through `resolveRunSource`. A rule written for the one a person names (copy a
  directory in no repository) reaches the ones a repository names too, unless
  they are kept out of it on purpose.
- **A lookup started ahead of its turn must not be able to ask anybody
  anything.** A create runs one step ahead of a terminal, and two questions on
  it at once cannot be answered. Turning git's prompt off is not enough: git
  runs an askpass program before it checks that setting (an IDE's terminal
  sets `GIT_ASKPASS` to a dialog), which the prefetch empties (`prefetchEnv`)
  — and, separately, a noninteractive operation's shared execution policy.
  Interactive lookups in their turn preserve IDE helpers: for a
  person's own remote an askpass is often how the credential arrives, and
  emptying it in `sandboxgit.NoPromptEnv` broke cache delivery of a private
  https repository from an IDE's terminal; and
  ssh asks for a passphrase or a new host key on `/dev/tty` itself and waits
  rather than failing. So a remote is prefetched only when git, its
  `insteadOf` rewrites applied, reaches it over a transport whose questions all
  go through git (`prefetchable`) — judging the URL as written let an https
  GitHub URL rewritten to ssh through — and only a failure to ask is looked up
  again in its turn (`lookInTurn`): retrying every failure waited out a dead
  host twice. Noninteractive cache delivery uses the same no-question executor, including
  its best-effort fetch from the pool cache; an expired pool API token can
  trigger Git askpass there too.
