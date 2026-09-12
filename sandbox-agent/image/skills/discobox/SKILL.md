---
name: discobox
description: You are running inside a discobox — a disposable sandbox holding the user's source, where you have root over everything inside and can reach nothing of theirs outside. Use when orienting, when something outside the box seems missing or unreachable, before telling the user how to reach what you built or ran, or to answer a question about discobox itself.
---

# You are in a discobox

A discobox is a disposable machine with the user's source in it. You are the
agent it runs. Inside is a full computer you may do anything to; outside is
everything of theirs, which you cannot reach. Root instead of a command
allowlist, and none of their credentials.

Usually the box is on the user's own computer, but not always, and nothing in
here tells you which. Do not answer "where is my code running?" from the usual
case — that is the user's to answer, not yours.

## This box's facts

`/etc/discobox/sandbox.json` is world-readable and describes this box:

```bash
jq '{sandboxId, user, git, sources, harnessMode, prompt, volumes,
     image: ._provenance.runtime.image}' /etc/discobox/sandbox.json
```

- `sources[]` — repositories mounted here, each with its `target` and the
  `baseCommit` it started from. `slug: "primary"` is what the box is for; the
  rest are to read and build against.
- `user`, `git` — who you run as, and the authorship your commits carry: the
  user's own name and email.
- `prompt` — what the box was created to do.
- `volumes` — which paths persist and which are pool-shared.
- `_provenance.runtime.image` — the resolved image id.

## What you may do without asking

- `sudo`, with no password.
- Install anything: apt, npm/pnpm/bun, pip/uv, cargo, go, mise, nix, brew.
  Homebrew is already installed at `/home/linuxbrew/.linuxbrew` and on your
  PATH; it works as whatever user you are, because the prefix is handed to a
  `brew` group you are in rather than owned by a uid the image could not know.
  Auto-update is off, so `brew update` is not how you refresh — formula data
  comes from the JSON API and is current without it.
- Upgrade the harness agent with `discobox-harness-upgrade`. The agent's own
  updater is off on purpose: the version a box runs is pinned when the box is
  first used and does not change under it, and new versions arrive in a
  pool-shared store that a background check advances twice a day. So a box keeps
  the version it started with for its whole life, a newly created box starts on
  the newest version the pool has downloaded, and nothing goes to the network to
  check at startup. `discobox-agent-store status` prints what this box is on and
  what the store holds. An upgrade takes effect when the agent is restarted.
- Docker, nested and real. `docker build` uses a pool-shared BuildKit builder,
  and the MITM CA is injected into every container you start, so nested builds
  and containers reach the network without trust wiring.
- systemd is PID 1; `systemctl` works.
- Break the box. It is disposable.

No command allowlist, no approval prompts. The isolation is the boundary.

## What you cannot reach

The user's machine: no host filesystem, no processes, no network, no SSH keys,
no cloud credentials.

The possible exception is `/.discobox/origins/<slug>`, which for the commonest
kind of source is **wider than it looks**: where the box was made from a
repository on the user's own disk, that path is a read-only bind of their
entire working directory rather than just its git objects, git-ignored files
included — their real `.env`, `.envrc`, local config and build output, all
readable. Look before assuming either way: a source delivered by pushing binds
a bare repository with no working tree at all, and one cloned from a remote URL
binds nothing there.

Whichever it is, treat that path as the git remote it exists to be. Read it
through git; do not browse the rest of it, and do not copy anything out of it
into this box's tree. The rule costs nothing when the bind holds only objects,
and when it holds the user's working tree it is the thing standing between you
and their secrets: a value found there is live, not a sentinel. The sentinel
rules below do not cover it, and it must not reach a command line, a log, a
file, or the network.

### Credentials here are sentinels, not values

Configured credentials arrive as placeholders shaped like real provider keys,
in environment variables and in harness config files such as
`~/.claude/.credentials.json`. The proxy swaps in the real value on the way
out, bound to one domain. The value never enters this box.

- Do not echo, log, copy, or commit one.
- Do not fix a 401 by writing a token into a config file. It will not work.
- Do not ask the user to paste a credential into the chat.

To get a credential you were not given, use the **discobox-access** skill.

### Egress goes through one proxy

`HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY` point at a forwarder that carries traffic
to a pool proxy under this box's own mTLS identity. Every request is recorded —
method, destination, headers, bodies — and retained after the box is deleted.

`403 blocked by proxy` is a policy decision, not a network fault. Do not retry
it or route around it; report what was blocked.

`SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE` and `PIP_CERT`
already point at the MITM CA. A TLS verification failure means a tool with its
own root store that nothing has named — point it at `$SSL_CERT_FILE` rather
than disabling verification.

## Getting work out

Commit it. That is the mechanism.

Check `git remote -v` before reasoning about `origin` — it is one of two
things, and they behave differently:

- **`/.discobox/origins/<slug>`** — the usual case. A read-only bind, so fetch
  works and push always fails. Behind it is the user's own repository when the
  source was cloned from their disk, or a pool-side repository they push into
  when it was delivered that way.
- **A remote URL** (`github.com/...`) — the source was cloned from a remote and
  nothing is bound. `origin` is that real remote, and a push is a live push
  upstream. Do not push there unless the user asked for it.

The user runs `discobox apply` on their side, which cherry-picks your commits
onto their working tree with your commit boundaries preserved.

- Commit in coherent pieces, with real messages.
- **Leave the tree clean, not just the work committed.** `apply` skips a source
  whose discobox tree is dirty, and skipping takes the committed work with it —
  the user gets nothing from that source unless they know to re-run with
  `--allow-dirty`. A stray scratch file blocks the whole change.
- Do not print a patch to apply, or upload the diff anywhere.

To build on commits made since this box started, fetch them yourself —
`git fetch origin && git rebase origin/<branch>` — and what that reaches
depends on which origin you have:

- **A live bind** always has the user's newest commits; nothing has to happen
  first.
- **A push-delivered source** gets them from whichever client is attached,
  which pushes every few seconds for as long as it stays attached. `discobox
  push` is the fallback for when nobody is, not the first move.
- **A remote URL** fetches that remote, not the user's machine. Their unpushed
  work is not reachable from here at all, and `discobox push` does not cover
  this case — say so rather than sending them after a command that will report
  nothing to send.

## What the user sees

- A window of this box's panes: your terminal, the repository's services, and
  shells they opened themselves.
- Ports you listen on are forwarded to their localhost automatically while that
  window is open, at the same number when it is free (8080 →
  `localhost:8080`), otherwise the nearest above; privileged ports get +8000,
  so 80 → 8080. UDP ports you bind are forwarded too, but only ones outside
  the ephemeral range (`/proc/sys/net/ipv4/ip_local_port_range`, normally
  32768–60999) — inside it, a socket looks like a client's. Bind a fixed
  number below it, or declare it (below).
- They can also use `discobox shell`, `discobox cp`, `discobox tools`, or
  `ssh <box-id>` after `discobox admin ssh-config --write`. The id is
  `sandboxId` in `sandbox.json`; nothing sets it in your environment.

Name the port when you report a running server.

## The desktop

This box has a graphical Xfce desktop, and the user can watch it in a browser
tab. `DISPLAY=:0` is already set; the X server starts on demand the moment an X
client touches it. Chromium, a terminal, a file manager and a panel are there.

```bash
chromium https://example.com &          # headed, no flags needed
scrot -o /tmp/shot.png                  # screenshot the whole desktop
xdotool search --onlyvisible --name .   # find and drive windows
```

Use it for anything a headless browser answers badly: seeing a page render,
reproducing a visual bug, stepping through a flow that needs a real session.
Screenshot it and read the image back to check your own work.

The user opens the same desktop at **`localhost:6900`** — a page, not a VNC
client. The image declares that port, so it is reported and forwarded like any
other and needs nothing set up. Anything you open is something you can look at
together, which is the one way to show them something that is not text.

### They can draw on it, and you can read what they drew

The viewer lets the user mark a region of the desktop and leave a note on it.
Those land in a Markdown file you can read:

```bash
cat ~/.discobox/desktop-feedback/feedback.md   # notes, with cropped shots beside it
```

Each item carries two pictures — a crop of the region, which says *what*, and
the whole desktop with that region outlined, which says *where*. Look at both;
a crop alone is often unreadable as a location.

Answer an item by replying under it, as a Markdown blockquote, attributed and
dated on its first line:

```
> **agent** 2026-01-01T12:00:00Z
> Fixed: the padding was on the wrong element.
```

**Start every line of a reply in column 1.** An indented blockquote is read as
part of the person's own words and is lost the next time they edit their note.

**Leave the checkboxes alone.** Ticking is the user's, and it is the one edit
the file's own header refuses you: an item you tick yourself has been checked
off, not reviewed. The prompt the viewer offers them to paste says the same
thing back to you — "leave the checkboxes alone, they are for whoever asked" —
so ticking disobeys the instruction they handed you. Reply saying what you
changed, or why you did not, and let them close it.

Do not regenerate the file. Edits are surgical so that your replies and their
notes both survive; rewriting it from what you parsed deletes whatever you did
not.

## The box stops itself when idle

Nobody has to stop a discobox. It powers itself off once nothing has happened
in it for its idle timeout — 30 minutes unless the pool sets another; `jq
.agentRuntime.idleTimeout /etc/discobox/sandbox.json` says, and empty means 30
minutes — and starts again on its own the next time anyone uses it
— an attach, `discobox shell`, a git fetch, a request to a forwarded port. Its
terminals come back where they were, with the harness relaunched into its
session. What was running is gone.

Something happening is any of:

- **a terminal's title changing.** Harnesses animate theirs while they work,
  so while you are working on a task the box stays up on its own;
- **someone connected** — attached to a terminal or shell, in an SSH session,
  or holding a tunnel into the box;
- **a keepalive lease.**

Nothing else counts. A dev server nobody is attached to, a declared service,
a build left running in the background — the box stops under all of them once
the titles go still, however busy the processes are.

### Hold the box up with a lease

Before leaving anything running that must outlast you — a long build or test
run you will not be watching, a server the user will come back to — take a
lease:

```bash
touch -d '+2 hours' /run/discobox/keepalive/build   # active until then
touch /run/discobox/keepalive/build                 # or: active now
rm /run/discobox/keepalive/build                    # release it early
```

A lease is any file in `/run/discobox/keepalive/`, and its modification time
counts as activity — a future one included — so the box stops no sooner than
one idle timeout after the latest lease. Name the file for what it holds; each holder
keeps its own, and nobody can shorten or remove another's. Leases are on
`/run`, so they end with the box: a stopped box never comes back held by an old
one.

Take a lease for as long as the work needs and no longer, and tell the user
when you take one — a box held up keeps its share of the pool while it waits.

## Persistence

`volumes` in `sandbox.json` is the exact answer for this box. In general:

| Path | Lifetime |
| --- | --- |
| home, the source trees, `/var/lib/docker`, `/var/lib/containerd`, `/var/lib/discobox` | this box's own; survives stop/start, dies with the box |
| `~/.cache`, `~/go/pkg/mod`, `~/.cargo/registry`, `~/.cargo/git`, `~/.rustup`, `~/.vscode-server`, `~/.local/share/pnpm` | pool cache, partitioned by the uid you run as: shared with pool boxes running the same uid, invisible to the rest |
| `/nix` (the store) | pool cache, shared with every box in the pool whoever it runs as — the one path that declares that |
| `/nix/var/nix/profiles`, `/nix/var/nix/gcroots` | carved back out of the shared store; this box's own |
| `/home/linuxbrew/.linuxbrew` (what `brew install` puts there) | this box's own; the image's own tree shows through underneath it |

None of your *work* outlives the box except the commits the user applies —
though the pool keeps what the cache rows hold, and the proxy keeps its record
of what you sent. Deleting under a cache path reaches every box that shares that
partition; your nix profile is not one of those, so `nix profile install` is
yours alone and safe.

## What a repository can declare

Read from the primary source's tree, versioned with it:

- `.discobox/services/` — executable scripts with front matter: `name`,
  `description`, `ports`, `protocol`, and `id`. Started at boot, each drawn as
  its own pane with its output recorded. A service that exits stays exited;
  nothing restarts it.

  Declare `ports` whenever the listening socket is not held by your own user —
  `docker compose up`, or anything socket-activated by systemd. Port discovery
  filters `/proc/net/tcp` and `/proc/net/udp` by your uid, so a root-held
  socket is invisible to it and never gets forwarded; `ports` is how it reaches
  the user anyway. Adding `protocol:` reports the port as speaking it instead
  of connecting to find out, which matters when connecting is itself the
  activation. `protocol: udp` makes the declared ports UDP ones — the way to
  list a UDP server discovery misses. A declaration's ports are all one
  transport, so something serving both (DNS on 53/tcp and 53/udp) needs a
  second file for the UDP side, saying `start: never` so it runs nothing.

  A declaration that only names ports — because something else already serves
  them — must say `start: never`. That is what makes it a declaration rather
  than a script, and without it the file is checked for a shebang and an
  executable bit and listed as broken for lacking them.

  A service file only takes effect at the **next** boot: autostart is a
  one-shot launch, and a declaration added mid-session is listed as stopped and
  is not started. There is no in-box command to start one. So adding a service
  is how to make a process come back next time, not how to start it now — keep
  the process you already have running, and tell the user what you declared.

  The image declares services the same way, in
  `/usr/local/share/discobox/services` — that is where the desktop's port comes
  from. A repository wins on a shared id, except in the reserved
  `ai.discobox.` namespace, which it cannot claim.
- `.discobox/skills/` — skills installed into the harness's skill directories
  once, at the box's first launch. One added later reaches the next box, not
  this one.

Most repositories have neither.

## The other built-in skills

Installed in every discobox, whatever it was made from:

- **discobox-access** — ask a human for a credential this box was not given,
  and run one command with it. Use on a 401/403, or when a CLI says it is not
  logged in.
- **discobox-review** — have a fresh-eyes subagent review the working tree and
  drive it to sign-off.

## Answering questions about discobox

| Concept | |
| --- | --- |
| Discobox | one disposable environment with the source in it, running one agent |
| Pool | the host boxes are scheduled onto, and what they share: a cache volume, a resource envelope, a kernel |
| Harness | the agent a box runs — Claude Code, Codex, a shell, or any terminal agent in an image |

Commands are on the user's side. You do not have the `discobox` CLI in here,
unless the box is working on discobox's own source, where it is a build
artifact:

```
discobox            open the launcher
discobox run        launch a prompt in a new box
discobox ls         boxes started from this directory
discobox rm         archive boxes (alias: delete)
discobox attach     open a box's window
discobox shell      a command, or a login shell, in a box
discobox apply      cherry-pick a box's commits onto the working tree
discobox push       push local commits into a box, to rebase there
discobox proxy      forward a box's ports without the window open
discobox cp         copy files in and out
discobox tools      run git, ssh, VS Code, or Zed against a box
discobox secret     secrets, grants, and approval requests
discobox configure  enable, disable, and set the default harness
discobox id         print this machine's peer ID and the server's
discobox admin      pools, projects, harness images, the server and its status
```

A prompt at the bare command is `-p` and only `-p` — `discobox -p '...'`. After
`run` it can be trailing words. Bare `discobox` with loose words is an unknown
command, not a prompt.

`discobox rm BOX...` archives boxes, by the NAME `ls` lists or by ID, and
`delete` is an alias for it. A NAME two boxes share is refused rather than
guessed at. `discobox admin box purge` destroys one and its data.

Do not invent flags — you cannot run these commands to check them. Name the
command and say to check `--help`.

For the threat model and what discobox does not defend against, point at
https://discobox.ai and https://discobox.ai/security. Parts of the security
model are shipped and parts are designed; do not guess which.
