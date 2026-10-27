# proxyagent review notes

- A field removed from a persisted on-disk policy document needs a read-side
  conversion, since the file outlives the binary that wrote it. SecretsFile is
  read by the next pool agent before anything rewrites it: when a route's `useId`
  went (ADR 26-10-27-880), decoding the old file into the new `secretroute.Route`
  silently dropped it and turned a route a judge read into an allow on every
  running sandbox. `readSecretsDoc` reads such a route as blocked
  (`persistedRoute`). Convert to what the old file's writer enforced, never to
  anything wider, and toward refusing where the old meaning is unclear; test
  the conversion with a file of the old shape.
- A field whose shape changes in SecretsFile bumps `secretsDocVersion`, and
  `readSecretsDoc` reads each version as its writer meant it. When sentinel
  routes became route lists, "no routes" turned from "held to none" into "sent
  nowhere": without a version an old file's unrouted sentinel would have read
  as refused on every running sandbox until the control plane pushed again,
  and a new file's could not be told from it. An old file's reads as `*`, as
  it was enforced (ADR 26-10-27-939).
