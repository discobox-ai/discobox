# sandboxconfig — review rules

- **A key in `.discobox/settings.json` that one reader owns must not change
  what another decides.** The client owns `sources`, the pool the project
  layer; the sandbox agent answers the pool the project layer alone, and none
  for a file holding only the client's keys. Passing the whole file through
  made a sources-only `settings.json` a non-nil empty layer, whose digest
  differs from none, and the pool rebuilt the container on every create
  (ADR 26-10-16-423 §11 review).
- **Every reader decodes the settings file through `RepositorySettings.Settings`.**
  A second decode drifts: the pool's lenient one settled a spec from a file the
  client refused.
- **No reader refuses `settings.json` for a key it does not define.** The file
  is read by every client and sandbox agent version its repository's people
  run, and a sandbox keeps the agent it was created with (ADR 0114 §7): a
  strict decode makes a key added later a 422 from an older agent, and a box
  that is never ready. Warn on the client (`UnknownKeys`) instead; a check on
  the file's shape belongs to the reader whose keys it is about, and must not
  count `$schema`, which every file is told to carry.
