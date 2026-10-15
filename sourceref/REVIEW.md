# sourceref — review rules

- **A list of things to cut from dedupes by `CloneKey`, never by `Key`.**
  `Key` merges a repository's https, ssh, http and `git://` spellings; a list
  that merges by it offers one spelling and runs another, so choosing https
  can fetch over plaintext with the user's credential. Shorten a display only
  where the short form stands for the same transport.
- **A directory that is there wins over the notation.** `@` and `//` are
  legal in a directory's name (a synced folder named after an account), so a
  parse that splits one must give way to the existing directory in `Resolve`;
  refusing it was a regression once. The same value no longer survives
  `Parse` alone, so code that only parses — the console, the server — never
  splits a directory off a value: it takes the directory from whoever
  resolved it (`tui.Source.Directory`).
- **Nothing here reads the environment while a frame is drawn.** `Display`
  takes the home directory from its caller, so what the console draws, and
  its tests, do not depend on the process's `HOME`.
