# Resources review notes

- A URL a person writes that the server will fetch — an exchange recipe's
  endpoint, a target document — goes through `internal/publicnet`. Call
  `RefuseHost` when the URL is written and again before each fetch, and fetch
  with `publicnet.Client` on a client that follows no redirect. A redirect is a
  second URL nobody checked. Without these, a project member can use the
  server's network position: loopback, the private network, cloud metadata.
  This has been done twice, the second time without the guard at first.
