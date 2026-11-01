# github-repo Review Notes

- **Save a marker of done work only once that work is done.** That means a
  probe's ETag, `Since`, `RunsSince`, and a baseline. Everything a change
  calls for must come before it, the watched query included. State is saved
  even when a run fails part way, so progress is kept. A marker saved before
  its work therefore hides the change for good: the next probe answers 304.
  The first draft saved the issues ETag before the watched query, and set
  `Since` at the start of the baseline. A run that must set its markers
  first — a `test --since` pass, `startFrom` — saves nothing until it has
  finished; its first draft saved them at once, so a pass cut short left a
  state that read as one done.
- **Don't trust a single search pass after a probe change.** The REST probe
  sees a change before the search index does. Passes go on until one that
  began `overlap` after the probe's last change (`LastChange`) has completed.
  Measure that by the pass (`Since`), never by when the run happens: runs
  can be minutes apart (a long `interval`, a rate limit, a box asleep). A
  test should hold any new path that reads the search after a probe.
- **Don't take GitHub's aggregate states as final.** `statusCheckRollup.state`
  is `FAILURE` while other checks still run. `mergeable` is `UNKNOWN` until
  computed. Report a conclusion only once it has settled.
