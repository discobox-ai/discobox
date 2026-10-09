---
name: verify-recipes
description: This repository's recipes for verifying a change at runtime inside a discobox — a CLI or console (TUI) change against the running `task dev` loop (opening the console in an isolated tmux, signalling it, forcing a real out-of-memory kill), the nested-Docker runc wrapper (runcca / sandbox-agent/cmd/discobox-runc) via docker run and kind, the pool proxy (`proxy/`) by driving traffic from a box of the dev pool, the runtime-config intake the pool delivers to its sandboxes, and a change to a skill (`.discobox/skills`, `.agents/skills`) via a headless agent in an isolated HOME. Inside a discobox, the generic `verify` skill (from `.discobox/skills/verify`) reads it first and records what it learns here; outside one, Claude Code's built-in `/verify` does not know this name. Use when verifying a change to the discobox console, its terminal guard, anything reached from bare `./build/discobox`, the runc wrapper, the pool proxy, the runtime-config intake, or a skill.
---

# Verifying the console

Read `.agents/skills/test-fix/driving-task-dev.md` first: the dev loop is
already running, and every CLI call needs `--server http://127.0.0.1:8080`.
Confirm `build/discobox` is newer than your last edit before driving it.

## Open the console

Bare `discobox` opens the console without creating a box; it is the cheapest
way to reach `runConsole` (and the terminal guard it starts).

```bash
tmux -L verify new-session -d -s v -x 150 -y 40 -c "$PWD" \
  "env -u DISCOBOX_SERVER PS1='\$ ' bash --norc --noprofile"
tmux -L verify send-keys -t v './build/discobox --server http://127.0.0.1:8080; echo "exit=$?"' Enter
tmux -L verify capture-pane -p -t v
```

The console is `pgrep -f '^./build/discobox --server'`; its guard is
`pgrep -f 'admin console-guard'` (`pgrep -f` also matches your own tool shell,
so filter it out). Ctrl-C at the welcome screen quits it normally.

## Gotchas

- `sudo` works without a password in the box.
- A real kernel OOM kill: `sudo mkdir /sys/fs/cgroup/<name>`, move the pane's
  shell (`tmux list-panes -F '#{pane_pid}'`) into its `cgroup.procs`, open the
  console, then set `memory.swap.max` to 0 and lower `memory.max` to a few MB.
  Without the swap limit the kernel swaps instead of killing. `rmdir` the
  cgroup after the tmux server is gone.
- bpftrace is not installed: `nix build --no-link --print-out-paths
  nixpkgs#bpftrace`. Inside a box, `args->pid` on a tracepoint is the host's
  pid, not the box's.
- `sudo pkill -f <pattern>` kills your own tool shell when the pattern is in
  its command line; kill by pid instead.
- Kill the tmux server (`tmux -L verify kill-server`) when done.

# Verifying the runc wrapper (runcca, sandbox-agent/cmd/discobox-runc)

The installed wrapper is `/opt/discobox/bin/runc`; dockerd and containerd call
it for every container. Swap a build in, drive `docker run`, then put the
original back.

```bash
S=<scratchpad>
(cd sandbox-agent && CGO_ENABLED=0 go build -o $S/discobox-runc ./cmd/discobox-runc)
cp /opt/discobox/bin/runc $S/runc.orig
sudo install -m 0755 $S/discobox-runc /opt/discobox/bin/runc
docker run --rm -e HTTP_PROXY busybox:1.36 sh -c 'env | grep -i proxy | sort'
sudo install -m 0755 $S/runc.orig /opt/discobox/bin/runc   # always restore
```

- Run the same commands against the original wrapper first, as a baseline.
- Inputs: `/etc/discobox/proxy/bridge.json` (root-only, loopback forwarder)
  and `/run/discobox/proxy/nested-forwarder.json` (bridge forwarder). To get
  the "forwarder unpublished" state, `sudo mv` the second one aside and restore it.
- `--network host` and `--network container:X` exercise the namespace cases.
- End to end with kind: `GOBIN=$S/bin go install sigs.k8s.io/kind@v0.30.0`,
  `kind create cluster --name <n> --wait 0`, then
  `docker exec <n>-control-plane crictl pull docker.io/library/alpine:3.20`.
  kind forwards the caller's proxy env into the node, so its containerd's env
  (`/proc/$(pidof containerd)/environ`) shows what the wrapper left there.
  Delete the cluster afterwards.

# Verifying the pool proxy (`proxy/`)

The dev pool's container runs it as `discobox-pool-agent proxy`, built from
this checkout by the image watcher. Confirm the running binary has the change
(`docker exec <pool> grep -c '<new string>' /usr/local/bin/discobox-pool-agent`)
and that the process started after the build (no `ps` in the image: walk
`/proc/*/cmdline`, `stat -c %y /proc/<pid>`).

- Drive it from a `-H shell` box: its `HTTPS_PROXY` is the box's bridge to the
  pool proxy, and `SSL_CERT_FILE` trusts the MITM CA. `d cp` a static binary in
  for anything the image lacks (python3, perl, curl are there).
- An origin on this box at `172.17.0.1:<port>` is in the pool proxy's
  `NO_PROXY`, so it is dialed directly. Anything else leaves through the outer
  discobox's proxy — older code, HTTP/1.1-only MITM — so a public endpoint
  cannot show what the dev pool proxy sends upstream.
- A self-signed TLS origin is refused (`502`, audited `blocked / policy`) until
  trusted: `discobox-access trust -use ... -why ... HOST:PORT` in the box, then
  `d trust request approve <treq>`.
- What the proxy saw: `d admin audit http --discobox-id <id> --since 10m`;
  bodies with `--discobox-id <id> --body http_N --part request|response`
  (`--body` alone errors). `--host 172.17.0.1` matched nothing.
- A swapped credential needs `discobox-access run`, which the dev server
  refuses without a judge (no default harness) unless `judgeCommands: false`.
- Clean up: `d rm <id>`, `d secret delete <name>`, stop origins by pid.
- No dev pool up (the sandbox-agent image will not build), or the
  harness/sandbox-agent `:local` images missing (an isolated server then fails
  on `has no harness to run`) → embed the real `proxy` package instead. Write a
  scratchpad module (`replace` the repo and goproxy's fork as the root `go.mod`
  does, copy `go.sum`, `GOWORK=off`) whose `main` calls
  `proxy.PrepareCertificates` and `proxy.NewServer` on `proxy.DefaultConfig()`,
  then either:
  - `bridge.New` for a plain `http://127.0.0.1:<port>` to point `HTTPS_PROXY`
    at; trust the MITM CA with `--cacert` (curl) or `PERL_LWP_SSL_CA_FILE`
    (LWP, extrepo). An origin on loopback is dialed directly (trust its CA
    through the process's `SSL_CERT_FILE`); anything else chains through the
    outer proxy, which the proxy picks up from the environment.
  - or serve `ControlHandler()` on a port and drive it over mTLS with
    `curl --noproxy '' --proxy https://127.0.0.1:<port> --proxy-cert/--proxy-key/--proxy-cacert`
    (the box's `NO_PROXY` covers 127.0.0.1) — the route for audit, cache, or
    spool behavior.

  For a cacheable blob, the origin must send `Docker-Content-Digest` or an OCI
  media type, or the content-aware arm refuses it. Either route leaves the
  pool-agent, server, and CLI relays unexercised.

# Verifying a skill change (`.discobox/skills`, `.agents/skills`)

The surface is an agent loading the skill. Install it the way
`sandbox-agent/terminal/skills.go` does — the image's skills, then
`.discobox/skills`, into both `.claude/skills` and `.agents/skills` of an
isolated `HOME` — and run a headless agent in a throwaway repo built to reach
the changed instruction. A changed `.agents/skills/<name>` is a project skill:
the harness reads it from the repo it runs in, so copy it into the throwaway
repo's `.agents/skills/` and link `.claude/skills -> ../.agents/skills` there,
as this repository does: Claude Code reads only a repo's `.claude/skills`
(a run with the skill in `.agents/skills` alone did not list it).

```bash
S=<scratchpad>; H=$S/home; mkdir -p $H/.claude
cp ~/.claude/.credentials.json ~/.claude/settings.json $H/.claude/; cp ~/.claude.json $H/
for d in .claude/skills .agents/skills; do mkdir -p $H/$d
  cp -r /usr/local/share/discobox/skills/. .discobox/skills/. $H/$d/; done
# in a throwaway git repo holding the scenario:
env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT -u CLAUDE_CODE_SESSION_ID -u CLAUDE_PID \
  -u CLAUDE_CODE_MESSAGING_SOCKET -u CLAUDE_CODE_MESSAGING_TOKEN -u CLAUDE_CODE_CHILD_SESSION \
  -u CLAUDE_CODE_SESSION_ATTENDED -u CLAUDE_CODE_EXECPATH -u CLAUDE_EFFORT \
  HOME=$H claude -p '/<skill>' --model sonnet --dangerously-skip-permissions \
  --output-format stream-json --verbose > $S/run.jsonl
```

- The box's credential file is a sentinel the proxy swaps, so copies of it
  authenticate; the unset `CLAUDE_CODE_*` variables keep the run from
  attaching to your session.
- Prove which copy loaded by grepping `run.jsonl` for a phrase only the new
  text has. A personal `~/.claude/skills/<name>` outranks Claude Code's
  built-in of the same name.
- Read what it did from the `tool_use` entries
  (`jq 'select(.type=="assistant") | .message.content[]'`) and the repo's
  `git status`, not only its final report.
- Baseline against the original (the built-in, or the skill at `HEAD`) on an
  identical copy of the repo before calling a behavior a regression.

# Verifying a server/API change against an isolated server

The `task dev` loop's server is the user's; run a second one from your own
build rather than reconfiguring it. `discobox-server` takes no flags and
starts for real, so isolate everything first.

```bash
S=<scratchpad>; SOCK=$(mktemp -d /tmp/v.XXXX)   # sockets: short path, see below
(cd server && go build -o $S/discobox-server ./cmd/discobox-server)
env -i HOME=$HOME PATH=$PATH DISCOBOX_ENV_FILE=$S/empty.env \
  DISCOBOX_DATA_DIR=$S/data DISCOBOX_STATE_DIR=$S/state \
  DISCOBOX_CONFIG_DIR=$S/config DISCOBOX_CACHE_DIR=$S/cache \
  DISCOBOX_SERVER_LISTEN=unix://$SOCK/s.sock,http://127.0.0.1:18471 \
  $S/discobox-server > $S/server.log 2>&1 &
until curl -s 127.0.0.1:18471/projects | grep -q '"id"'; do sleep 1; done
```

- Name a `unix://` socket of your own in `DISCOBOX_SERVER_LISTEN`: otherwise
  the default socket is added, which the dev loop may hold. Unix socket paths
  over ~100 bytes fail with `bind: invalid argument`, so a scratchpad path is
  too long; use `/tmp`.
- It needs a reachable Docker API (`initialize app` fails otherwise) and
  creates a real pool per project: `discobox-vm-pool_<id>` containers,
  `discobox-pool-pool_<id>-docker` and anonymous volumes, and
  `discobox-sbnet-pool_<id>` networks. Remove only those named for *this*
  server's pool IDs, read from its own API (`GET /projects/<id>/pools`)
  before stopping it. Never select by the `discobox-vm-pool_` prefix or by
  what is new since a snapshot: the dev loop recreates its own pool
  containers at any time, and on 10-08 a snapshot diff removed its default
  pool's container.
- `$!` of a backgrounded shell function is the subshell, not the server.
  Stop the server by its own pid (`pgrep -f $S/discobox-server`) *before*
  removing its pools, or it recreates them.
- The server's config file is found by XDG (`~/.config/discobox`), not
  `DISCOBOX_CONFIG_DIR`; none there means defaults plus your env.
- With `DISCOBOX_ENCRYPTION_KEY` unset, secret values are stored unsealed.
- Its pool runs the released `ghcr.io/discobox-ai/discobox-pool-agent:latest`,
  not your checkout's, so anything the pool agent gained since the last
  release is absent — e.g. it declares no platform, and placement then lets
  any harness through as a pre-platform pool. Add
  `DISCOBOX_DOCKER_POOL_IMAGE=<the dev loop's image>` (`docker ps` shows the
  `discobox-pool-agent:dev-*` its pool runs) to the env above.
  For every dev image at once — sandbox, pool, and harnesses, the in-box
  `discobox` CLI included — make `DISCOBOX_ENV_FILE` a copy of the
  `DISCOBOX_DEFAULT_SANDBOX_*`, `DISCOBOX_DOCKER_POOL_*` and
  `DISCOBOX_HARNESS_*` lines of the checkout's `.env`.

- It exits at start (`project … has no harness to run`) until at least one
  `discobox-harness-*:local` image exists; on a fresh box wait for the image
  watcher to build one.
- To race new intent against a pool's first reconcile (supersede paths):
  `admin pool create`, then `admin pool delete` ~3s later — `EnsurePool` of a
  Docker pool takes ~10s, so the delete lands mid-run. Watch `admin job ls -o
  json` (`.jobs[]`, by `resourceId`) for `attempts`/`error` and the log for
  `reconcile failed`. List outputs are wrapped (`.pools[]`, `.providers[]`).

## A lead discobox's calls (the sandbox role)

The dev loop judges every `discobox-access run` with the project's default
harness, which has no model credential here, so a lead's call never gets a
verdict. Use the isolated server above with `DISCOBOX_JUDGE_COMMANDS=false`
in its env file. Then: `new -d -H shell` a lead; inside it,
`discobox-access request --json` for `ai.discobox.sandbox` with the uses to
drive (no `wait`); approve it from outside with `discobox secret request
approve <id>`; read the use IDs with `discobox-access list --json` in the
lead; and run `discobox-access run --use <id> -- discobox …` there. A worker
the lead creates with `--no-source` is listed outside only by `ls --all`.

# Verifying a sandbox-agent route the pool drives (runtime-config, sources)

The pool delivers a runtime-config document at every boot (next section), but
to drive one of these routes with a document of your own, call the in-box agent
directly: `d new -H shell -d ...` a box (from a dirty checkout if the change
reads `sandbox.json`'s source spec), then `curl` it on
`127.0.0.1:3003/api/projects/<project>/sandboxes/<sbx>/...` from `d shell`.

- **Token.** The agent trusts the server's pool-agent issuer key
  (`server_state` row `worker_agent_request_issuer`; unsealed in dev, so
  `json_extract(value,'$.encryptedPrivateKey')` base64-decodes to the key
  text). Sign a PASETO v4.public with audience `sandbox-agent`, claims
  `project_id`/`pool_id`/`sandbox_id`/`scopes` (as
  `server/internal/auth/poolagent.CreateTokenForAudience`) from a scratch
  module; pipe the key in and the token into the box on stdin, never argv.
  The row exists only once the server has created a pool.
- **Your document races the pool's.** The pool's status poll converges on its
  own record, so a revision you send is overtaken at the next poll (it moves
  past whatever the sandbox holds). Read what you need before that, or take
  the pool's identity key instead (next section) so you deliver as it would.
- **An origin.** Serve a bare repository from this box with a small
  `git http-backend` CGI wrapper on `0.0.0.0:<port>`; the box reaches it as
  `http://172.17.0.1:<port>` through its proxy.
- `systemctl restart discobox-sandbox-agent` in the box ends the `d shell`
  it was run from (shells are the agent's execs): background it and reconnect.

# Verifying the runtime-config intake (pool → sandbox delivery)

Drive it from `-H shell` boxes of the dev pool (see the console section for
`d`). The dev pool's sandbox containers run on this box's own Docker daemon, so
`docker exec <discobox-sandbox-...>` reaches them directly.

- What arrived: `/etc/discobox/sandbox.json` (bootstrap: `provider.publicKeys`,
  `provider.pool`, no idle timeout), `/var/lib/discobox/runtime-config.json`
  (kept document and revision), `/run/discobox/secrets/secrets.json`,
  `/etc/discobox/proxy/*`, `/etc/discobox/ready`. The pool's record is
  `/var/lib/discobox/projects/*/pools/*/sandboxes/<id>/runtime-config.json`
  inside `discobox-vm-pool_<id>` (no `jq` there; `cat` it).
- Ordering: compare journal timestamps (`journalctl -o short-precise`) of the
  bridge `Started` lines and the first `discobox-exec-*` unit. A file's mtime is
  when it was staged, not when it was renamed into place, so `ready`'s mtime
  predates the units it waits for.
- The applied revision the control plane sees:
  `d admin box get <id> -o json | jq .runtime.agentStatus.runtimeConfigRevision`.
- Secrets reaching a running box: `d secret create`, then
  `d admin harnesses secrets bind <shell-config-id> ENV <secret>`; a new box
  gets it at create, and rebinding to a secret of another shape re-mints the
  sentinel and pushes it to running boxes.
- The pool's idle timeout cannot be changed on the dev pool in place:
  `d admin provider update --sandbox-idle-timeout` records it but did not
  recreate the pool container within 10 minutes. To see the sandbox apply a
  document, sign a delivery token with the dev pool's own key
  (`/var/lib/discobox/identity/*/*/agent.key` in the pool container, base64
  Ed25519; audience `sandbox-agent`, claims project/pool/sandbox, scopes
  `["runtime-config"]`) from a throwaway `go run` in the pool-agent module, and
  `curl` the route from inside the box. Restore the provider config afterwards.
- `d rm` archives; `d admin box purge <id>` (no `--yes`) removes.
- Certificate renewal: re-sign the box's client certificate in the pool so it
  is inside the 30-day window — `openssl` is in the pool container; sign with
  `/var/lib/discobox/proxy/projects/*/pools/*/certs/mtls-ca.{crt,key}` into
  `certs/clients/<sbx>/client.{crt,key}` with `-days 10` and
  `extendedKeyUsage=clientAuth`. The next poll (≤15s) reissues and delivers a
  new revision; compare the bridges' `systemctl show -p MainPID,NRestarts` and
  `openssl x509 -serial` of `/etc/discobox/proxy/client.crt` before and after.
- `docker cp` into a box's `/tmp` lands under its tmpfs and is invisible
  inside; pipe a file in instead (`docker exec -i <c> sh -c 'cat > /tmp/f' < f`).
- The image watcher recreates the dev pool's container whenever the
  pool-agent image rebuilds, which resets every bridge connection; a long
  transfer that breaks then is the rebuild (`docker inspect -f
  '{{.State.StartedAt}}'` on the pool container), not the change.
- **A live origin needs the server's host ID.** A source is clone-delivered,
  and its origin served live from the developer's `.git`, only when the
  client's host ID (`~/.config/discobox/host-id`) is the server's and the
  repository is somewhere the dev pool sees (`/home`, through
  `DISCOBOX_POOL_HOST_MOUNT_PREFIX=/host`; not `/tmp`). A CLI run with another
  `HOME` gets push delivery and a bare origin that new commits never reach;
  pass `DISCOBOX_HOST_ID` to keep it. Check with
  `d admin box get <id> -o json | jq .config.source.delivery`.
- Sources: the pool's own record of a box's delivery is the `delivered` flags
  in its `runtime-config.json`; flipping them to `false` on a stopped box and
  starting it drives the start-time settle (`settleConverged`), which reopens
  `/etc/discobox/ready` within a poll. Run `d` as a function, not a variable:
  zsh does not split `$D` into a command.

Gotcha: a wait loop of `until ! pgrep -f "docker build"` never ends — its own
shell's command line matches the pattern. Wait on the watcher's outputs
(`.env` image tags, the harness list) instead.

# Verifying the sandbox agent without a PID-1 flow (a VM guest's start)

No disco-vm driver runs here; the sandbox image booted with systemd itself as
PID 1 and no config volume is the same start a VM guest makes.

```bash
docker run -d --name vmlike --privileged --cgroupns=private --tmpfs /run \
  --tmpfs /run/lock --entrypoint /lib/systemd/systemd discobox-sandbox-agent:local
# a real bootstrap: any dev box's, with another sandboxId
d shell <box> -- sudo cat /etc/discobox/sandbox.json | jq '.sandboxId="sbx_vmlike"' > $S/s.json
docker exec -i vmlike sh -c 'cat > /etc/discobox/.s.tmp && mv /etc/discobox/.s.tmp /etc/discobox/sandbox.json' < $S/s.json
docker exec vmlike journalctl -u discobox-sandbox-agent -o short-precise
```

- Place the file by a rename, as a backend must; a `cat >` straight to the
  path can be read half-written.
- Without systemd at all (launchd/SCM stand-in): `--entrypoint
  /usr/local/bin/discobox-sandbox-agent ... --config /etc/discobox/sandbox.json`;
  it logs `waiting for a file to appear` and exits 0 on SIGTERM.
- `docker restart` is the guest's reboot; `systemctl kill -s KILL` the crash.
