# Probing an agent CLI

How the facts behind `harness/copilot` were measured without a real account.
Work in the session scratchpad; every throwaway `HOME` below costs whatever
the CLI writes on first run, so reuse one home, or put homes in `/tmp`
(tmpfs).

## Read the CLI's own words first

- `--help`, every `help <topic>`, every subcommand's `--help` — dump them to a
  file and read it whole. Settings and env-var lists are usually there.
- Many CLIs are a loader plus a native or bundled payload. Run it once and
  look for what it unpacked (copilot: `~/.cache/copilot/pkg/<platform>/<version>`,
  with readable `app.js` and native `.node` addons).
- Search the payload with a small node script printing context around a
  needle; `grep` here is ugrep, whose `.{0,300}` context patterns hit a
  complexity limit. `strings -n 6` on native addons finds paths (`/etc/...`),
  setting schemas (`{"path":"hooks.sessionStart",...}`), and error texts.
- npm packages often ship more than a bundle: omp shipped its TypeScript
  `src/` and its docs (`dist/docs-index.generated.txt`: a JSON list of names,
  then a base64 gzip of a JSON array of contents). Read those before a
  minified bundle.
- A Bun-compiled single binary (opencode 2's 200 MB `bin/opencode.exe`)
  carries its whole bundle as text: `strings -n 8` it to a file once and
  search that with `python3 -I`, printing context. Schemas read as
  `type:b("oauth")`, and migrations, config keys and error texts are all there.
  The vendor's own SDK packages (`npm pack @opencode/plugin`) ship `.d.ts`
  types for plugin and client APIs, which beat reading minified code.
- **A string in the binary is not behavior.** It tells you what to test next.
  Never report an endpoint or flow as "the CLI does X" until a run showed it.

## A fake model

Most CLIs accept a custom OpenAI-compatible endpoint (copilot:
`COPILOT_PROVIDER_BASE_URL` + `COPILOT_MODEL`, "BYOK", which also skips
sign-in). A ~20-line node server answering `/models` and streaming
`chat.completion.chunk` SSE lets you:

- run real sessions, interactive and one-shot, with no account;
- read every request body: the tools offered (count them to verify a
  tools-off switch), the system prompt, what a wrapper composed;
- make the model call a tool on the first turn (return `tool_calls`, then
  text once a `role: tool` message arrives) to fire tool hooks and read the
  tool's result — e.g. `env | grep TOKEN` to see what a tool's shell inherits;
- log request headers to see how the CLI authenticates upstream (copilot sent
  the raw GitHub token as `Bearer`, no exchange).

## A fake identity host

If the CLI takes a host for sign-in (`copilot login --with-token --host
http://127.0.0.1:PORT`), a fake answering the entitlement endpoints shows
exactly what a stored login looks like on disk, and which requests the
credential is sent in. Make it refuse on a flag file to see what the CLI does
when a host rejects the credential — and clear the CLI's user-info cache
first, or a cached answer hides the request entirely.

## A browser sign-in's callback port

A browser sign-in redirects to a callback server on the CLI's own localhost,
which the configure flow can reach only if the image declares the port in
`config.ports` (`harness.ConfigPort`): it forwards the same number from the
user's machine. Point `BROWSER` at a script that appends its arguments to a
file, start the sign-in in an isolated home (the CLI subcommand and the
in-TUI flow both — they can differ), and read `redirect_uri` off the URL.
Do it at least three times, and read the CLI's listen call if you can: a
port the identity provider registered is the same every run and must be
declared, with an `unavailable` message naming the fallback (device code, an
API key). One that changes each run is declared `{"ephemeral": true}`: the
flow then forwards whatever port the sandbox is found listening on, at the
same number — copilot's `listen(0, "127.0.0.1")` works because GitHub accepts
any loopback port. One observed port is not evidence of a fixed one, and
declaring it as fixed forwards a port the next run never uses.

## A request over HTTPS

When it matters what a request carries — a token in a header the proxy can
swap, or in a body it cannot — and the host is fixed (pi's Copilot exchange
at `api.github.com`), read it through a TLS-terminating proxy: an `openssl`
CA and a leaf certificate for the hosts, a ~30-line node server that answers
`CONNECT` and hands the socket to a `tls.TLSSocket` feeding an `http.Server`,
and the CLI run with `NODE_EXTRA_CA_CERTS=ca.pem` and the proxy variables
pointed at it. Unset the lowercase `https_proxy`/`http_proxy`/`no_proxy` the
sandbox sets too: undici prefers them. Wrapping `globalThis.fetch` from a
`--import` preload does not work on a CLI that calls `undici.install()`.
Answer the exchange from the fake to follow the flow on to what the CLI
writes back to its store.

## Hooks

Install a hook file at the candidate system layer whose command appends
`$1` and stdin to a log, one entry per event, then run a session. If nothing
fires, read the CLI's debug log (`--log-level debug --log-dir ...`): it often
names the directories it looked in ("Policy directory /etc/github-copilot/policy.d
not readable"). Check the layer survives the user's own "disable all hooks"
setting. The payloads you log are the ones the audit summary must read.

A CLI whose hooks are a plugin (opencode) gets a probe plugin that logs every
event and every hook it can register. Run it in the TUI *and* the
non-interactive mode, several times each: opencode 2's plugins subscribe to
the bus after its server starts, so `run` missed `session.created` (and once
everything) while the TUI never did. Count a turn's lifecycle in the mode the
launcher actually uses. Force the failure events too: point the model at a
closed port to see how retries and the final failure are reported.

## Interactive behavior

Run the TUI in a detached tmux session and read the screen:

```bash
tmux -L probe new-session -d -x 120 -y 40 -c "$WORKDIR" "env HOME=$H ... cli ...; sleep 30"
sleep 10; tmux -L probe capture-pane -p -t 0
tmux -L probe send-keys -t 0 2 Enter
tmux -L probe kill-server
```

Trust dialogs, permission modes (copilot's footer says "Manual Approval" or
"Allow All"), resume with and without a prior session, a dash-led prompt —
each is a one-line variation. Watch for races: capture after the screen
settles, and re-run a surprising result.

## A real credential

Only for what fakes cannot answer (does the vendor's API accept X?). Ask with
`discobox-access request`, with a `--route` for exactly each vendor API call
the probe reads or writes (method, exact host, path) and a justification
naming them — the routes hold the token to those calls; save responses
to a 0600 file under `/dev/shm`, print only redacted summaries, delete them
afterwards, and tell the user the binding is still live. A refusal is a result:
do not route around a vendor's access control by presenting as another client.

## Pitfalls this cost before

- `pkill -f name` matches its own command line and kills your shell; stop a
  fake by its port (`fuser -k PORT/tcp`).
- Never `rm -rf "$HOME"` after re-pointing `HOME`; make throwaway homes with
  `mktemp -d` and remove them by literal path.
- A full disk silently truncates writes and swallows command output; write
  diagnostics to `/dev/shm` and read them back with Read.
- A shell syntax error anywhere in a command line runs none of it: a
  setup step earlier on the same line silently never happened, and what
  follows measures a home you did not prepare.
- The login shell is zsh: a bare `=====` argument is an expansion error, and
  an unquoted `$flags` holding several words is still one word — write each
  case out or use an array.
- golangci-lint's cache lives under `~/.cache`, which is pool cache shared by
  every discobox of the same uid: `ci:check` can report findings in another
  box's checkout (`../../workspace/source/...`) or in generated code. Run
  `./build/tools/golangci-lint cache clean` and check again before believing
  them.
- A non-interactive mode may read piped stdin as more of the prompt and wait
  for it to close (opencode 2's `run`): give it `</dev/null`. A test of that
  passes vacuously from Go, whose `exec` hands the child `/dev/null` unless
  `cmd.Stdin` is set, so give the wrapper real stdin.
- Check that the non-interactive mode runs on the model the TUI starts on.
  opencode 2's `run` ignores the `/models` pick the TUI uses, so a configure
  check with no `--model` tested a free model and reported success.
- An isolation test with an empty `HOME` passes whatever `HOME` the wrapper
  sets. Plant the files the isolation must shut out, then mutate the line
  under test to a no-op and check the test fails.
- `COPY --chmod=0644 file /new/dir/file` creates the missing `/new/dir` with
  mode 0644 too, so only root can read inside it; `install -d -m 0755` the
  directory first. Check a built image as the sandbox user, never only as
  root: opencode 2 could not load the image's plugin directory and then loaded
  no provider at all, which no root-run check showed.
- `check-hooks` exiting with "hook daemon unsettled" is hooks still running —
  the Dockerfile builds take minutes — not a failure; check again.
