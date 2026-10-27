# proxyagent review notes

- A field removed from a persisted on-disk policy document needs a read-side
  conversion, since the file outlives the binary that wrote it. SecretsFile is
  read by the next pool agent before anything rewrites it: when a route's `useId`
  went (ADR 26-10-27-880), decoding the old file into the new `secretroute.Route`
  silently dropped it and turned a route a judge read into an allow on every
  running sandbox. `readSecretsDoc` reads such a route as blocked
  (`persistedRoute`); convert toward refusing, never toward allowing, and test
  the conversion with a file of the old shape.
