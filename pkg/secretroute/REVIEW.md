# Secret Route Review Notes

- **`**` matches zero or more segments and never makes a pattern more specific
  than the same pattern without it — a blocked exact path must outrank an open
  `p/**`.** `p/**` matches `p` itself, so on a request to `p` both are
  candidates, and if `**` counted as a segment the open `p/**` would decide
  and the block would never refuse anything. `DELETE
  api.github.com/repos/acme/web=blocked` beside `api.github.com/repos/acme/web/**`
  is exactly how a person stops a repository delete. Count a trailing `**`
  only as the tiebreak it loses (`pathRank.rest`), never in `segments` or
  `literal`.
- **A request's method is what the upstream executes, override headers and
  `_method` included; open method routes need every one, blocked ones any
  one.** Servers run a `POST` as the method it names elsewhere: Google APIs
  read `X-HTTP-Method-Override`, and OData services read `X-HTTP-Method`.
  `Rack::MethodOverride` (Rails, Sinatra) reads `X-HTTP-Method-Override` and
  `_method` from a form body, including a POST with no Content-Type, which
  Rack parses as a form. It does not read `_method` from the query. Laravel
  (Symfony) reads `_method` from the body, a form or JSON, and then from the
  query. Reading only the request line lets a box step around a
  `DELETE …=blocked` carve-out with a real credential.

  So a request names its request-line method plus every method in
  `X-HTTP-Method-Override`, `X-HTTP-Method` and `X-Method-Override`, plus
  `_method` in the URL query, for every method. On a POST only, it also names
  `_method` in a body that may carry one: a URL-encoded form, a body with no
  Content-Type, or the top-level object of a JSON (`/json`, `+json`) body.
  Symfony and Rack apply a body override only to a POST, so a PUT is judged
  as a PUT whatever its body says. It names them whether or not the upstream
  reads them, because the proxy cannot tell which framework is behind a host.

  An open route allows only when it lists every method the request names. A
  blocked route refuses when it lists any one. Every `_method` value read
  counts, even from a body a parser refuses: `_method=DELETE&a=b;c` names
  DELETE, and a JSON body with two `_method` keys names both. Form pairs are
  split on `&` and `;`.

  **Unreadable, never unparsable.** An open method route may ignore a POST
  body only when it could not be read at all, and only for two kinds:
  - **JSON** whose `Body` returned false: past the limit, undeclared in
    length, or in another encoding.
  - **Multipart** of any type (form-data, mixed, related — Rack parses them all), which is not parsed.

  Those are large API requests (LLM calls) and uploads, and refusing them
  would refuse legitimate traffic. A `_method` hidden there is caught only by
  a blocked method route, which matches them as unknown. That is the
  deliberate cost.

  Everything else is unknown to every route. An open route refuses it, and a
  blocked route matches it:
  - a URL-encoded or untyped POST body that could not be read. Real forms are
    small, and a chunked form is the cheapest way around an open POST route
    on a Rack app. The cost: `curl -d @file` and `--data-binary @file` send
    `application/x-www-form-urlencoded` unless told otherwise, so such an
    upload past the proxy's body limit (`routeBodyLimit`, 1 MiB), or
    chunked, is refused under an open method route; it goes through with its
    real Content-Type (`-H 'Content-Type: …'`);
  - a body that was read but does not parse or is ambiguous: invalid JSON or
    UTF-8, a duplicate or case-variant JSON `_method` (Laravel keeps the last
    value), a non-string `_method`, or a form pair `ParseQuery` refuses.

  Such a body is a few bytes, so refusing it costs nothing legitimate. Every
  new place a framework reads a method from goes on the list above, with a
  test in `TestDecideMethodOverrides`.

- **A reason carries at most `reasonNameLimit` runes of anything a request
  names** (a tool, a root field, a method, a host and path), cut with an
  ellipsis by `Safe`, which also escapes it. A request bounds those names only
  by the body limit, and a reason goes back to the sandbox, into the audit
  row, and onto every screen that shows it (t103). Never put request-derived
  text in a reason except through `Safe` (exported, so the proxy uses the same one).

- Keep containment input bounds aligned with credential-request route and byte
  bounds. A delegation combines resolved policies, including repeated shared
  routes: budget pairwise work after normalization and deduplication, and test
  handing on every member of a realistic policy union. A proof limited to a
  single role's size rejected existing manager delegations after approval.

- Charge pattern bytes as well as counts against the containment work budget.
  `pathContained` splits paths per candidate pair, so one long segmented grant
  against hundreds of parent routes otherwise allocates nearly a gigabyte while
  the handoff transaction holds its locks.

- **A route grants one kind of use: a body block its body places, any other
  route the headers and query.** A credential is decided per use
  (`Request.At`), and an open route is a candidate only for its own kind, so
  `*` grants no body place and a body route no header. A blocked route
  without a body block refuses every use, so a carve-out cannot be stepped
  around by moving the credential into the body. Every use of a sentinel in
  one request must pass in the same grant (`DecideGrants` over `At`): two
  grants, each allowing one use, allow neither together. Any new kind of
  place follows the same split.

- **No WebSocket block is no message place, whatever else the route says.**
  Every other block's absence allows any body; reusing that for messages
  would let a host route put a credential into any field a service publishes
  (ADR 26-11-06-442 §2). A blocked route without a protocol block still
  refuses a message on its host and path, and a WebSocket route grants no
  header or body use.
- **A message's string keeps one pointer step, never a copy of its whole
  pointer.** The sandbox writes the message, and a copy per string is name
  length times string count: one long member name over an array of `""`
  costs hundreds of megabytes in one 64 KiB WebSocket message. Build a
  pointer (`MessageString.At`) only for a string being matched, and keep
  `ParseDocument`'s allocation tests covering long names over many strings.
- Read a message strictly and compare conditions on the original, before any
  value is swapped in. A lenient decoder takes the last of two members and
  replaces an unpaired surrogate, either of which lets the proxy and the
  origin read a different message.
- Every view that shows a route by its hosts alone asks `Route.HostWide`,
  never its own list of fields: the hand-written lists missed the body block
  once and the WebSocket block once (`routesSection`), showing a narrowed
  route as a bare host at approval.
- A place shown for approval (`MessagePlace.Describe`) quotes its pointer and
  every condition name: a name may read "op is 2 and nonce", and shown bare
  an approver reads a condition the place does not have.
- **A request is held to every way an upstream may read its path**
  (`RequestPaths`): decoded, with an encoded `/` kept in its segment, and
  with each segment's `;` parameters removed before dot segments resolve,
  at a `;` as sent and at one decoded from `%3B`. Tomcat routes
  `/admin;x/a` as `/admin/a` and `/public/..;/admin` as `/admin`; reading
  only the literal path let a sandbox carry a credential past a blocked
  path or out of an open one (#1537). A new way servers normalize a path
  becomes another reading, with a case in `TestRequestPathsResolveDotSegments`.
  A reading that keeps one escape encoded (`%2F`) still decodes every other
  escape in that segment: keeping the whole segment as sent hid a `%3B`
  beside a `%2F` from the parameter readings.
