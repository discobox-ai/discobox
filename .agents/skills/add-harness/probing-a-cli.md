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

## Hooks

Install a hook file at the candidate system layer whose command appends
`$1` and stdin to a log, one entry per event, then run a session. If nothing
fires, read the CLI's debug log (`--log-level debug --log-dir ...`): it often
names the directories it looked in ("Policy directory /etc/github-copilot/policy.d
not readable"). Check the layer survives the user's own "disable all hooks"
setting. The payloads you log are the ones the audit summary must read.

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
`discobox-access request`, uses written as the literal calls; save responses
to a 0600 file under `/dev/shm`, print only redacted summaries, delete them
afterwards, and tell the user the grant is still live. A refusal is a result:
do not route around a vendor's access control by presenting as another client.

## Pitfalls this cost before

- `pkill -f name` matches its own command line and kills your shell; stop a
  fake by its port (`fuser -k PORT/tcp`).
- Never `rm -rf "$HOME"` after re-pointing `HOME`; make throwaway homes with
  `mktemp -d` and remove them by literal path.
- A full disk silently truncates writes and swallows command output; write
  diagnostics to `/dev/shm` and read them back with Read.
- The login shell is zsh: a bare `=====` argument is an expansion error.
- golangci-lint's cache lives under `~/.cache`, which is pool cache shared by
  every discobox of the same uid: `ci:check` can report findings in another
  box's checkout (`../../workspace/source/...`) or in generated code. Run
  `./build/tools/golangci-lint cache clean` and check again before believing
  them.
- `check-hooks` exiting with "hook daemon unsettled" is hooks still running —
  the Dockerfile builds take minutes — not a failure; check again.
