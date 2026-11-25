# 26-11-25-484 — The CLI serves a page of the person's boxes on loopback, behind a per-run token

- **Status**: Proposed (would amend [26-11-17-264](26-11-17-264-the-cli-serves-the-api-to-a-local-program.md)
  §2's "Nothing on it authenticates a caller" for a TCP listener: every TCP
  listener the CLI serves the API on takes a token, and only the unix socket is
  authenticated by its permissions alone. 264 is itself Proposed; the rest of it
  stands)
- **Date**: 2026-11-25
- **Issue**: [#1779](https://github.com/discobox-ai/discobox/issues/1779)
- **Relates to**: [26-11-17-264](26-11-17-264-the-cli-serves-the-api-to-a-local-program.md),
  the handler this page is served by;
  [26-10-29-316](26-10-29-316-sandbox-state-changes-are-a-resumable-event-stream.md)
  and [26-11-01-560](26-11-01-560-discobox-box-watches-other-boxes-through-the-sandbox-event-stream.md),
  the event stream and its `displayState`;
  [26-10-16-424](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md),
  the program status a box's state is read from;
  [0131](0131-the-launcher-answers-every-servers-credential-requests-and-names-the-server-its-config-screens-edit.md),
  every server's requests answered on their own server.

## Context

The list of a person's boxes exists only in a terminal: `discobox console`
and `discobox ls`. A terminal console is usually not on screen, and a browser
tab usually is. In #1779 a person reports three boxes that finished and sat
unnoticed, one of them overnight. They also report that answering a box's
credential request meant going back to the terminal. They ask for a local
browser page that lists their boxes with live state, notifies them when one
finishes or is blocked, and later lets them answer requests.

What is there today (at `fe8a69986`):

- **`discobox admin api serve`** (`cli/internal/cli/api_serve.go`, ADR 264)
  proxies every registered server's API (`/v1/servers`,
  `/v1/servers/NAME/api/PATH`). By default it listens on a 0600 unix socket.
  With `--listen http://127.0.0.1:PORT` it listens on loopback TCP with no
  authentication. Its only checks there are on Host, Origin and
  `Sec-Fetch-Site` (`apiHandler.ServeHTTP`), which stop another website and
  DNS rebinding. They do not stop another user on the machine, who can reach
  any loopback port.
- **The event stream** (`GET /projects/{projectId}/events`, ADR 316) carries
  each box's `displayState` and its program's OSC 7501 status (`program`), and
  can be resumed with `Last-Event-ID`. Pending requests are not on it. The
  console polls `secret-requests`, `trust-requests` and the other request
  lists on each server (`cli/internal/cli/tui_credentials.go`) and counts them
  per box (`Sandbox.PendingRequests`).
- **A box's state is what its program reports.** The owner ruled on #1710
  (2026-11-24) that an agent's primary state comes from its OSC 7501 status.
  Heuristics such as terminal quiet apply only to a program that reports
  nothing.
- **The "desktop viewer"** the reporter opened is not a Discobox page. It is
  the box's own desktop server (`sandbox-agent/desktop`, port 6900), reached
  through the console's port forward of the open workspace
  (`cli/internal/cli/tui_forward.go`). It is bound on `127.0.0.1` for as long
  as the workspace is attached.
- **There is no desktop app on `main`.** The `discobox desktop` app that
  `assets/brand/LANGUAGE.md` names lives only on `feature/desktop-console`,
  which is not expected to merge.

Three facts about browsers decide most of the design:

1. **A browser cannot dial a unix socket.** The page needs a TCP listener, and
   on loopback TCP every local user is a possible caller.
2. **Cookies ignore the port.** A cookie set by `http://127.0.0.1:PORT` is
   sent to every port on `127.0.0.1`. Every box port that the console or
   `discobox proxy` forwards is on `127.0.0.1`, and a box's programs choose
   what is served there. A session cookie for this page would be sent to
   them. Origin-scoped storage (`sessionStorage`, `localStorage`) and
   notification permission are keyed by scheme, host and port, so they do not
   leak this way.
3. **A browser opens about six HTTP/1.1 connections per origin, and speaks
   HTTP/2 only over TLS.** A page holding one event stream per server and per
   project would use up its connections, and the rest of its requests would
   queue behind those streams.

## Decision

### 1. `discobox web` serves the page, through the same handler as `admin api serve`

A new top-level command, `discobox web`, serves the page and the API on
loopback TCP and opens the page in the person's browser. It runs until it is
interrupted, as `admin api serve` does. It is the same `apiHandler`, with the
same server set, proxy and per-server clients, and with the page mounted
beside `/v1/`. It is not a second proxy. It refuses any address that is not
loopback, as ADR 264 §1 does, and never listens on another interface.

It runs whether or not the console is open, and the console neither starts
nor needs it. The page lists every server `GET /v1/servers` lists, so it
follows ADR 0131's rule: each box and each request is answered on its own
server.

The page is static files (HTML, CSS and JavaScript) embedded in the CLI
binary. It loads nothing from another origin, so it works offline and needs
nothing installed. It follows `assets/brand` and uses `tokens.css`.

### 2. Every TCP listener takes a per-run token, sent as a header and never as a cookie

- **The token.** Each run of a TCP listener (`discobox web`, or
  `admin api serve --listen http://…`) generates 256 random bits. The token
  lives only in that process and changes each run.
- **Every request carries it**, page assets excepted, as
  `Authorization: Bearer <token>`. That includes `/v1/servers`, the proxy and
  every stream. A request without it, or with a wrong one, gets a 401 problem
  and is never proxied. The token is compared in constant time. The
  `Authorization` the CLI drops before proxying (264 §4) is this token, so the
  token never reaches a server.
- **Page assets need no token.** The HTML, CSS and JavaScript are public: any
  local user could read the same bytes from the CLI binary. Serving them
  without a token lets the page load and then ask for the token.
- **No cookie.** The token is never set as a cookie, so it is not sent to a
  box's forwarded port (fact 2), and there is no ambient credential for
  another website to ride (CSRF). The Host, Origin and `Sec-Fetch-Site` checks
  stay as they are, as a second layer against DNS rebinding.
- **How the page gets the token.** The command opens the page at
  `http://127.0.0.1:PORT/#token=…`. The browser never sends a fragment to a
  server, so the token is not in any request line or log. The page moves the
  token into `sessionStorage` and removes the fragment from the address bar.
  `sessionStorage` is scoped to the origin and the tab (fact 2), so the token
  survives a reload but not a closed tab. When the CLI restarts, the old token
  stops working, and the page says to open the link the command printed.
- **The URL never goes on a command line.** Another local user can read a
  process's arguments, so `xdg-open URL` would leak the token. The command
  writes a 0600 HTML file in the CLI's state directory that redirects to the
  URL, and opens that file. This is Jupyter's approach. The command also
  prints the URL, for a person who wants to open it in another browser.
- **`admin api serve` on TCP prints its token** and requires it as well. A
  script on loopback TCP sends the same header. The unix socket is unchanged:
  its permissions are its only access control, as 264 §2 says.

This is the amendment to ADR 264 §2. That ADR rejected "a token in a file"
because the socket's mode already does that job. Over TCP nothing does the
socket's job, and that listener was safe only for a caller that could already
read everything. A page that approves credential requests (§5) cannot rest on
that.

### 3. Nothing a box serves is ever on the page's origin

The page shows what boxes report as text: their names, tags, program
messages and request bodies. It never shows it as markup. It never proxies,
frames or embeds anything a box serves (a desktop, a dev server, an HTML
preview) under its own origin. Any script on the page's origin can read the
token from `sessionStorage` and approve that box's own requests. The page
sets a Content-Security-Policy of `default-src 'self'` with no inline script,
and `X-Content-Type-Options: nosniff`.

**A desktop link** asks `discobox web` to forward that box's desktop port, as
the console's workspace does. The forward is held while a page shows the
link as open, and the link points at the forwarded port, which is another
origin. An image a box produces (phase 3's screenshots) is served as an image
type with `nosniff` and shown in an `<img>`, never as a document.

### 4. One merged stream from the CLI, with state from OSC 7501

The page opens one stream, `GET /v1/events` on the CLI's listener. It reads
the stream with `fetch` rather than `EventSource`, because `EventSource` cannot
send the `Authorization` header. The CLI subscribes to each server's project
event streams itself (ADR 316) and resumes each with its own `Last-Event-ID`.
It polls each server's request lists as the console does. It forwards each
event tagged with the server it came from.

So the page holds one connection whatever the number of servers and projects
(fact 3). A server that stops answering is reported in the stream as not
answering, as the console's listing reports it (ADR 0122), and does not hold
back the rest.

A box's state on the page is exactly two things, with no third rule of the
page's own:

- **The box:** its `displayState`, combined by the server's rule (ADR 560).
- **Its program:** the OSC 7501 state (`working`, `idle`, `done`, `blocked`
  with its `kind`, `error`), as the owner ruled on #1710, plus whether it has
  pending requests.

The terminal-quiet heuristic is not used for a program that reports OSC 7501.

### 5. Notifications while the page is open, and three phases

**Notifications** use the browser's Notification API. `http://127.0.0.1` is
a secure context, so the API is available there. The page asks for
permission on a click, never on load. It notifies when:

- a box's program goes from `working` to `idle` or `done` (finished);
- a program becomes `blocked` or `error`;
- a box gets a new pending request;
- a box's `displayState` becomes a failure.

Each notification is tagged with the box, so a second notification about the
same box replaces the first instead of stacking. Clicking it focuses the box.
Notifications need a page open somewhere. There is no service worker and no
Web Push, which would need a push service outside the machine.

The work is in three phases, each its own issue once this ADR is accepted.
Each later phase rests on the authentication above and needs no new ADR:

1. **Read only:** boxes, live state, notifications, desktop links.
2. **Answering requests:** credential, trust, approval and adoption requests,
   with their hosts, routes and lifetimes, each approved or denied on its own
   server through the proxy. A secret typed into the page goes through the
   same proxy to that server.
3. **Comparing:** a grid of boxes chosen by tag, with title, branch, diff size,
   latest screenshot and desktop link, and memory and disk per box and per pool
   (#1713, #1425).

### Left to implementation

- The default port. The port has to stay the same across runs, because
  notification permission is granted per origin. If the port is taken, the
  command fails and names `--listen`.
- The shape of `/v1/events` messages, and the request poll's interval. It
  reuses the console's poll where it can.
- Whether a JavaScript build step is used (the repository already has
  esbuild), or plain modules are embedded as they are.

## Alternatives rejected

- **Serve the page from `discobox admin api serve`.** The command is under
  `admin`, and its default is the unix socket a browser cannot use. Someone who
  wants to see their boxes is not administering anything, and would have to
  know to add `--listen http://…`. Both commands share one handler, so this
  costs no duplicate code.
- **Serve the page from the Discobox server.** The page has to reach every
  server the person registered, over iroh with this machine's identity, and
  only the CLI can do that (264's rejected alternatives). A page on one server
  shows one server's boxes.
- **Serve it from the console while it runs.** The console being closed is
  the problem the issue reports.
- **A session cookie, as Jupyter uses.** Cookies ignore the port (fact 2), so
  the cookie would reach every box port forwarded on `127.0.0.1`, where a
  box's own program can read it and approve its own requests. A cookie is also
  an ambient credential that needs CSRF defence. A header token needs neither.
- **The token in the query string.** A query string is sent to the server,
  logged, and kept in browser history. A fragment is not sent at all.
- **Loopback TCP with no token, protected by Host and Origin checks alone (264
  §2 as written).** This is enough against websites, but not against another
  user on the machine, who can reach the port. It might be acceptable for a
  read-only list, but not for the phase that approves credential requests.
  One rule for every phase avoids changing the trust model halfway through.
- **A persistent token in a 0600 file**, so a bookmark survives a restart.
  The token would last longer than the process that checks it, and a copied
  link would keep working. A per-run token means only a link this run printed
  works.
- **One event stream per server and project, opened by the page.** This hits
  the per-origin connection limit (fact 3) as soon as a person has a few
  projects. HTTP/2 would lift the limit, but browsers speak it only over TLS,
  and a loopback certificate would have to be trusted by hand.
- **Proxy each box's desktop under the page's origin** (`/boxes/ID/desktop/`).
  One link and no forward, but the box's own scripts would share an origin
  with the token (§3).
- **Web Push, for notifications with no tab open.** It needs a push service
  outside the machine, and a subscription that outlives the CLI process. A
  pinned tab is enough for the reported cases. Revisit if a person reports
  missing a notification because no tab was open.
- **Listening on the LAN, so a phone can use the page.** The page would need
  real authentication and TLS, and a credential approval would be one leaked
  link away from anyone on the network. Remote Control already reaches a box
  from a phone. Answering its requests from there is a separate issue.
- **Wait for the desktop app.** It is not on `main`, and is not expected to
  merge as it stands. A page in any browser is available now, on every
  platform the CLI runs on.

## Consequences

- A person runs `discobox web`, or leaves it running, and sees every box on
  every server in a browser tab, with live state and notifications, without
  the console.
- `admin api serve --listen http://…` needs a token from now on. A script
  that used it unauthenticated has to send the printed token, or switch to
  the unix socket. The socket default is unchanged.
- The CLI gains a merged event route (`/v1/events`), a desktop-forward route,
  embedded web assets, and a browser-opening path that keeps the token out of
  argv.
- The page shows only states the server and OSC 7501 define, so a fix for
  #1710 or a new OSC 7501 state reaches it with no change to the page's
  rules.
- A notification needs an open tab. Closing every tab, or stopping the
  command, ends notifications without warning.
- Phases 2 and 3 are filed as their own issues and build on §2–§4 without a
  new ADR.
