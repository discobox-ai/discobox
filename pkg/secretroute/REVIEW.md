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
  ellipsis by `safe`, which also escapes it. A request bounds those names only
  by the body limit, and a reason goes back to the sandbox, into the audit
  row, and onto every screen that shows it (t103). Never put request-derived
  text in a reason except through `safe`.
