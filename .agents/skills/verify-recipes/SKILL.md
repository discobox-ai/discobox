---
name: verify-recipes
description: This repository's recipes for verifying a change at runtime inside a discobox — a CLI or console (TUI) change against the running `task dev` loop (opening the console in an isolated tmux, signalling it, forcing a real out-of-memory kill), the nested-Docker runc wrapper (runcca / the sandbox agent's `runc` personality) via docker run and kind, the pool proxy (`proxy/`) by driving traffic from a box of the dev pool, a pool behind a TLS-terminating edge (`wss` tunnels), the sandbox's DNS stub and nested-Docker name resolution, the runtime-config intake the pool delivers to its sandboxes, a pool route such as the asset API called as the dev pool (minting its assertion), a box's fetches from `assets.discobox.internal` (the pool's bundle cache), feature provisioning (bundle trees, provision.json, placeholders) in a dev box, a Docker pool's box (provisioning, enrollment, an in-place re-apply) and the server's release manifest export, the files API (a box's `.../files/*` routes, curled through the dev server, and the CLI's and harness configure's uses of it), what a box's program reports over OSC 7501 (driving a primary terminal headless, real Claude Code in a dev box, a fake OSC-aware outer terminal for the console), and a change to a skill (`.agents/skills`) via a headless agent in an isolated HOME. The generic `verify` skill (`.agents/skills/verify`) reads it first and records what it learns here; Claude Code's built-in `/verify` does not know this name. Use when verifying a change to the discobox console, its terminal guard, anything reached from bare `./build/discobox`, the runc wrapper, the pool proxy, the DNS stub, the runtime-config intake, a pool route, a pool's box or its bundle, or a skill.
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
- The console's heap: run it as `GODEBUG=gctrace=1 ./build/discobox ... 2>$S/gc.log`
  and read the live heap (`A->B->C MB`, C) from the last lines; RSS keeps
  freed pages. For a before/after, build the baseline from `git archive HEAD
  | tar -x -C $S/base` and run both consoles attached to one box at once.
- A copy (drag with SGR mouse sequences sent by `send-keys -l $'\e[<0;X;YM'`,
  `32;` to drag, a lowercase `m` to release) goes out as OSC 52: the box has no
  clipboard tool, so `tmux -L verify set -g set-clipboard on` first and read it
  with `show-buffer`. The wheel is `$'\e[<64;X;YM'` (up) / `65` (down).
- A paste (an image path dragged on, imagepaste.go) is a bracketed paste sent
  raw: `tmux -L verify send-keys -t v -l $'\e[200~/path/shot.png\e[201~'`.
  Open the box's workspace with `attach <sbx_id>` (it takes the ID, not the
  name), and read what landed with `sha256sum /tmp/discobox-paste/*` typed
  into the box's shell. The box has no wl-paste/xclip, so a Ctrl-V there only
  shows the fallback; a real image needs a real PNG signature (`clipimage`
  sniffs the bytes).
- On a box that has just booted, `new -H shell` can answer "harness config
  not found" until the image watcher has built that harness; the server seeds
  it on its next restart. Build a binary to run into `/tmp`, not `/dev/shm`
  (mounted noexec).
- A change in a module outside the checkout (a `replace` to `../../<fork>`)
  is not rebuilt by the loop: build the console yourself with `go build -o
  $S/discobox ./cli/cmd/discobox`, and expect the image watcher to fail on
  that path until the replace names a pushed version.

# Verifying what a box's program reports (OSC 7501, terminal queries)

No attach needed: drive a `d run -d -H shell --no-source` box's primary with
`d admin terminal input <id> primary "<cmd>" Enter` and read it
with `d admin terminal screen <id> primary`; the shim's record is
`d admin box get <id> -o json | jq '.runtime.agentStatus.sessions[]'` (`ls`
lags it by the 15s poll). `d cp` scripts into `/tmp`.

- A report from a shell builtin or a script that exits is dropped at once: the
  shim drops a group's live records when it leaves the foreground, and an
  interactive shell makes every command its own job. Hold the foreground —
  `bash -c 'printf "\e]7501;state=blocked\e\\\\"; sleep 3600'` — and quote by
  writing a script file instead of nesting quotes through `input`.
- A query's answer: in the box, `stty raw -echo; printf '\e]7501;?\e\\\e[c';
  read -r -t 3 -d c r; stty sane; printf '%q\n' "$r"` shows what came back and
  in what order. Attached from tmux, the DA1 part is tmux's (`?1;2;4`).
- Real Claude Code: `d cp` this box's `claude` binary
  (`/usr/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe`),
  `~/.claude/.credentials.json` (a sentinel the outer proxy swaps) and
  `~/.claude.json` in, accept the trust dialog (`Down Enter`), and run with
  `--permission-mode default` to get a permission prompt. Leave it with
  `/exit` Enter: two `C-c` sent a second apart did not exit it, and the next
  `input` became a prompt it acted on.
- The console speaks OSC 7501 only to a terminal that answers its probe, and
  tmux does not. Run it under a Python `pty.fork()` stand-in that answers
  `\e]7501;?` (and DA1), logs every `\e]7501;` the console writes, and plays
  timed keys (`\x1b[A` focuses the list, `\r` opens a box, `\x01q` quits)
  and `os.system` calls (`d admin terminal input`) to change the box mid-run.

# Verifying the runc wrapper (runcca, the agent's `runc` personality)

The installed wrapper is `/opt/discobox/bin/runc`; dockerd and containerd call
it for every container. It is the sandbox agent's `runc` personality: a
symlink to the agent, which dispatches on `argv[0]`
(`sandbox-agent/cmd/discobox-sandbox-agent/runc.go`). Point the link at a
build, drive `docker run`, then put the original back.

```bash
S=<scratchpad>
(cd sandbox-agent && CGO_ENABLED=0 go build -o $S/discobox-sandbox-agent ./cmd/discobox-sandbox-agent)
cp -P /opt/discobox/bin/runc $S/runc.orig   # the link, or an older image's file
sudo ln -sf $S/discobox-sandbox-agent /opt/discobox/bin/runc
docker run --rm -e HTTP_PROXY busybox:1.36 sh -c 'env | grep -i proxy | sort'
sudo cp -P --remove-destination $S/runc.orig /opt/discobox/bin/runc   # always restore
```

An image from before the fold ships a standalone wrapper file there, and its
agent has no `runc` personality: restore what was there, never a fixed link.

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

# Verifying the sandbox's DNS stub (`sandbox-agent/dnsstub`, nested DNS)

Wait for the image watcher to export the platform bundle (its log's `built
platform as …/build/assets/discobox-platform/<hex>.tar`, and the server's
platform moving) and the box to be pinned to it (`d admin box get <id> -o json
| jq .config.platformBundleDigest`), then `d run -d -H shell --no-source -p
x`; the agent a box runs is its platform pin's, delivered at every start. In the box (`d shell <id> -- ...`): `sudo
iptables -S INPUT` shows the stub's rule, and `docker run --rm busybox:1.36
ping -c1 -W2 example.com` on the default bridge, a user network, one with
`-o com.docker.network.bridge.name=X`, and `--network host` shows nested
resolution (busybox `nslookup` prints no addresses for single-label names;
`ping`'s first line does). What resolved: `d admin audit dns --discobox <id>
--since 15m`.

- The box has no `nslookup`: `docker cp` `/bin/busybox` out of a `docker
  create busybox:1.36` and run `busybox nslookup`.
- A neighbor on the dev pool's sandbox network (`docker run --network
  discobox-sbnet-<pool> --cap-add NET_ADMIN`, routing `169.254.53.53` via the
  box's `eth0`) never reaches the stub here: the host Docker's internal-network
  rule (`ip daddr != <subnet> iifname br-… drop`) drops it first, even with the
  stub's rule deleted. To exercise the stub's rule itself, in the box: `ip
  netns add probe`, a veth whose box end is named `eth9` (dropped) or `nst0`
  (answered), route `169.254.53.53` through it, and `ip netns exec probe
  busybox nslookup example.com 169.254.53.53`; the rule's counter (`iptables
  -L INPUT -v -n`) moves only for `eth9`.
- `sudo systemctl restart discobox-proxy-bridge.service` re-runs the stub's
  claim; the rule must stay single.

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
- What the proxy saw: `d admin audit http --discobox <id> --since 10m`;
  bodies with `--discobox <id> --body http_N --part request|response`
  (`--body` alone errors). `--host 172.17.0.1` matched nothing.
- A swapped credential needs `discobox-access run`, which the dev server
  refuses without a judge (no default harness) unless `judgeCommands: false`.
- A harness credential's 401 (the refused-credential band, a harness secret's
  `rejectionScope`) needs a box of a harness that declares secrets, which is
  refused until the harness is configured, and configuring needs a real
  sign-in. Bind a fake `d secret create` token with `d admin harness secret
  bind <slug> <ENV> <secret>` and a project grant, set `configured=1` on the
  harness's row in `.tmp/discobox/discobox.db` (`sqlite3`; set it back after),
  then `d run -d -H <slug> --no-source -p x`. A `-s ENV=value` secret is not
  swapped until its request is approved (`d secret request approve <sreq>
  --secret <id>`, IDs from `d secret request ls`); an unswapped sentinel
  also gets a 401, so check the audit row's secret column. Sentinels are in
  the box's `/run/discobox/secrets/secrets.json` (root). What was recorded:
  `curl 127.0.0.1:8080/projects/<project>/secret-rejections`.
- A harness bundle's manifest reaches the server only when the image watcher
  has rebuilt it (`.tmp/discobox-dev-images.json` names a new tag). If it never
  does, a build in the chain is failing and retried every 15s: read the
  `docker build` command from `/proc/<pid>/cmdline` and run it under a
  throwaway tag to see the error.
- A secret's routes (ADR 26-10-02-924): `d secret create --token X --host
  172.17.0.1 --route 172.17.0.1/PATH`, `d admin harness secret bind <shell
  harness id> ENV <secret>`, a `--scope harnessConfig` grant, then `d run -d
  -H shell --no-source -p x`. A plain-HTTP origin on `0.0.0.0` echoing the
  `Authorization` header shows swaps; `curl --path-as-is` keeps `..` and
  `%2F` as sent. The pool's per-sentinel routes are in
  `/var/lib/discobox/proxy/projects/<project>/pools/<pool>/secrets.json` in
  the pool container. A route change shows as a `sandboxSentinels` job in `d
  admin job ls`. `docker pause` of the pool makes deliveries hang rather than
  fail, and the dev loop restarts a pool left paused (~25s).
- A box's proxy record (its `clients`, `routes`, `egress` and `gateways`
  entries in that `secrets.json`): give it an egress level with `d admin box
  update <id> --egress-deny-by-default --egress-allow HOST`. To lose the record
  as the orphan reaper does, rewrite the file without the box's keys (`jq
  del(...)` on this box, then `docker exec -i <pool> sh -c 'cat > F.tmp && mv
  F.tmp F'`). The reaper spares a material record
  (`…/pools/<pool>/sandboxes/<id>`) younger than 15 minutes; `touch -d` it
  older, then `docker rm -f` the box's container, to see a reap ~5s later.
- Clean up: `d rm <id>`, `d secret rm <name>`, stop origins by pid.
- No dev pool up (the sandbox base image will not build), or the dev
  harness and platform bundles missing (an isolated server then fails on
  `has no harness to run`) → embed the real `proxy` package instead. Write a
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

# Verifying a pool behind an edge (`pkg/wstunnel`, `wss` pool URLs)

No provider sets `DISCOBOX_POOL_TUNNEL_URL` (ADR 26-10-23-719), so switch the
dev pool's proxy unit by hand and run a stand-in edge inside a `-H shell` box:

- Pool: append `DISCOBOX_POOL_TUNNEL_URL=wss://edge.test` to
  `/etc/discobox/proxy.env` (keep a copy), then `docker exec <pool> sh -c
  'kill -9 <pid>'` (no `kill` binary; find the pid by walking `/proc/*/cmdline`
  for `discobox-pool-agent proxy `). The unit restarts (`Restart=on-failure`)
  and logs `pool tunnels serving addr=[::]:17090`. Restore the copy and kill
  again. Listening ports: read `/proc/<proxy pid>/net/tcp{,6}`, not the
  container's own `/proc/net`.
- Edge: a ~30-line Go `httputil.ReverseProxy` with a self-signed `localhost`
  cert, run in the box with the proxy env unset, to
  `http://discobox-pool-proxy:17090`; log each request's path and `Upgrade` to
  count tunnels. The box's `/tmp` is a tmpfs `docker cp` cannot write: pipe
  files into `/var/tmp` (`docker exec -i <c> sh -c 'cat > /var/tmp/f' < f`).
- Bridge: a copy of `/etc/discobox/proxy/bridge.json` with `wss://localhost:8443/tunnel/{proxy,credentials,dns}`,
  its own `listenAddress` and a loopback `dnsListenAddress`, run as
  `SSL_CERT_FILE=<system bundle + edge cert> /opt/discobox/current/features/core/discobox-sandbox-agent proxy-bridge --config <copy> --dns`.
  `curl --proxy` it; the pool's `admin audit http|dns` shows the box's ID.
- Credentials: the agent's relay reads the real `bridge.json`, and its intake
  rewrites that file when the agent restarts, while its system roots load only
  at start: append the edge cert to the bundle, restart
  `discobox-sandbox-agent.service`, then edit `credentialsUrl`.

# Verifying a skill change (`.agents/skills`)

The surface is an agent loading the skill. Install it the way
`sandbox-agent/terminal/skills.go` does — the image's skills (this repository
has no `.discobox/skills` to follow them) into both `.claude/skills` and
`.agents/skills` of an isolated `HOME` — and run a headless agent in a throwaway repo built to reach
the changed instruction. A changed `.agents/skills/<name>` is a project skill:
the harness reads it from the repo it runs in, so copy it into the throwaway
repo's `.agents/skills/` and link `.claude/skills -> ../.agents/skills` there,
as this repository does: Claude Code reads only a repo's `.claude/skills`
(a run with the skill in `.agents/skills` alone did not list it).

```bash
S=<scratchpad>; H=$S/home; mkdir -p $H/.claude
cp ~/.claude/.credentials.json ~/.claude/settings.json $H/.claude/; cp ~/.claude.json $H/
for d in .claude/skills .agents/skills; do mkdir -p $H/$d
  cp -r /usr/local/share/discobox/skills/. $H/$d/; done
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
- A narrow rule (one tag step, one section) needs no full run: prompt
  `Read .claude/skills/<name>/SKILL.md and do ONLY <step> for <state>`, with
  the state seeded in `$H/.discobox/meta.json`, and read the file afterwards.
- `orchestrate-issues/orchestrate.sh` runs against the dev server with a
  stub `discobox-access` first on `PATH` (`list` prints a use whose
  description matches the script's regex; `run … -- cmd` execs `cmd`), a
  `discobox` link to `./build/discobox`, and `ORCH_DIR` in the scratchpad
  holding a `workers.tsv` row for a box made with `./build/discobox run -d
  -H shell <dir>` (the dev project has no default harness). `rm` only
  archives it; `discobox admin box purge <id>` removes it.

# Verifying `discobox-access` in a dev box (request, wait, list)

Every command goes to the dev loop through `d()` from
`.agents/skills/test-fix/driving-task-dev.md`
(`d() { ./build/discobox --server http://127.0.0.1:8080 "$@"; }`): a bare
`discobox` reaches the outer discobox API, where a fake secret and a forever
grant do not belong.

A `d run -d -H shell --no-source -p x` box talks to the dev pool agent and
server, but its in-image `discobox-access` can lag the checkout: `grep -c
'<new string>' $(command -v discobox-access)` in the box. If it does, build
`CGO_ENABLED=0 go build ./access/cmd/discobox-access` and `d cp` it to
`<box>:/tmp/`.

- Approving needs a secret: `d secret create --name N --host H --token fake
  --max-grant-ttl forever` (the default cap is 1h, so a longer `--grant-ttl`
  is refused without it), then `d secret request approve <sreq> --secret
  N [--grant-ttl 2w|forever]`.
- `request`, `wait`, and `list` need no judge; only `run` does (see the lead
  section below).
- Clean up: `d rm <id>`, `d admin box purge <id>`, `d secret rm N`.

## A request carrying a policy, and its approval forms

- The dev loop's pool may run the released
  `ghcr.io/discobox-ai/discobox-pool-agent:latest` (`docker ps`), which drops
  any protocol field newer than the release — a request's `policy` arrives as
  `null`. Use the isolated server below with
  `DISCOBOX_DOCKER_POOL_IMAGE=discobox-pool-agent:local` and
  `DISCOBOX_DOCKER_SANDBOX_BASE_IMAGE=discobox-sandbox-base:local`.
- With no `shell` harness bundle, `go tool task build:harness-stub-bundle`,
  then `admin harness create --bundle discobox-harness-stub:local` and
  `admin harness configure stub`; `new -d -H stub`. A bundle only the host's
  daemon holds is not one the asset API can serve a pool (it reads the dev
  manifest's layouts, or a registry), so point the server at a dev manifest
  whose `bundles` list the stub's exported layout. Configuring it stores a
  `stub-token` secret: delete it with the harness.
- File one from inside the box with `discobox-access request --statement …`
  (the box's `discobox-access` is the platform bundle's, so a rebuilt
  platform bundle carries a change to it, at the box's next start). A sentence that
  does not parse, or a policy that does not validate, never leaves the box;
  to send the server one anyway, post the agentcreds `RequestBody` with
  `curl --data @/tmp/req.json $DISCOBOX_CREDENTIALS_URL/v1/credentials/requests`
  after `d cp req.json <box>:/tmp/`. A box from a local
  checkout records its `origin`, so `$primary.repo` resolves; `$primary.upstream`
  never does yet, for the "matches nothing" form.
- A shell function `d` does not reach `timeout`; zsh does not split `$D`
  either. Put the CLI and its `--server` in a two-line script.

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
  `DISCOBOX_CONFIG_DIR`; none there means defaults plus your env. Name one of
  your own with `DISCOBOX_CONFIG_FILE=<file>` in its env; a file that fails
  to load exits 1 before anything is created.
- With `DISCOBOX_ENCRYPTION_KEY` unset, secret values are stored unsealed.
- Its pool runs the released `ghcr.io/discobox-ai/discobox-pool-agent:latest`,
  not your checkout's, so anything the pool agent gained since the last
  release is absent — e.g. it declares no platform, and placement then lets
  any harness through as a pre-platform pool. Add
  `DISCOBOX_DOCKER_POOL_IMAGE=<the dev loop's image>` (`docker ps` shows the
  `discobox-pool-agent:dev-*` its pool runs) to the env above.
  For every dev image and bundle at once — sandbox base, pool, platform and
  harnesses, the in-box `discobox` CLI included — make `DISCOBOX_ENV_FILE` a
  copy of the `DISCOBOX_DOCKER_*`, `DISCOBOX_PLATFORM_BUNDLE`,
  `DISCOBOX_HARNESS_*` and `DISCOBOX_DEV_DOCKER_IMAGE_*` lines of the
  checkout's `.env`.

- A manifest-file harness (`admin harness create --image file://<path>`) is read
  only from `$DISCOBOX_CACHE_DIR/overlays`; put the JSON there. To exercise a
  stored-row migration, write the old value with `sqlite3
  $S/data/discobox.db` while the server is stopped and read it back right after
  start: built-in harnesses are re-seeded and pools re-reported within seconds,
  which overwrites what you planted on them.
- It exits at start (`project … has no harness to run`) until it can read at
  least one harness bundle: a layout the image watcher exported, which it
  finds through the dev loop's `DISCOBOX_HARNESS_*`, `DISCOBOX_PLATFORM_BUNDLE`
  and `DISCOBOX_DEV_DOCKER_IMAGE_*` lines in its `DISCOBOX_ENV_FILE` (above).
  A `:local` bundle only the daemon holds seeds, but no pool can be served
  it; on a fresh box wait for the watcher's first export. It also exits that way, every harness failing on
  `platform bundle: inspect bundle "discobox-platform:local"`, when the image
  watcher process predates the tree it watches (a `go run` does not rebuild
  itself) and so has never built the platform bundle. The dev loop's own server
  exits the same way then (`wnb-run-failed.txt`). Restarting the watcher
  half alone fixes both: kill the `discobox-dev-lock docker-image-watch`
  process group and run its Taskfile command (`dev:docker-image-watch`) in
  the background from the repo root; it rebuilds, writes `.env`, and the
  server restarts onto it. Without restarting the
  user's loop: start from a copy of its database instead
  (`sqlite3 .tmp/discobox/discobox.db ".backup $S/data/discobox.db"`, then
  `DATABASE_DSN=sqlite3://$S/data/discobox.db`), whose harnesses are already
  seeded. Delete from `pools`, `sandbox_provider_instances`,
  and `reconcile_dirty`, clear the projects'
  `default_pool_id`/`judge_pool_id`/`judge_sandbox_id`, and set
  `DISCOBOX_JUDGE_COMMANDS=false`: the copy then never touches Docker, and
  never collides with the loop's pools, which share its IDs.
- A pool route on that server, with no real pool: see the no-Docker case
  under [Verifying a pool route as the dev pool](#verifying-a-pool-route-as-the-dev-pool-the-asset-api-any-apipoolspoolid).
- To race new intent against a pool's first reconcile (supersede paths):
  `admin pool create`, then `admin pool rm` ~3s later — `EnsurePool` of a
  Docker pool takes ~10s, so the delete lands mid-run. Watch `admin job ls -o
  json` (`.jobs[]`, by `resourceId`) for `attempts`/`error` and the log for
  `reconcile failed`. List outputs are wrapped (`.pools[]`, `.providers[]`).

- The CLI's `run` against it enrolls an SSH key and adds
  `Include ~/.local/state/discobox/cli/ssh/<project>/config` to `~/.ssh/config`;
  remove that line and directory when you clean up.
- To lose a create mid-request (the pool agent replaced under it): start
  `new -d --no-source -H shell` in the background, poll the new box's
  `runtime.provisionProgress.phase` every 0.1s, and `docker restart -t 0` (or
  `stop`, then `start` later) *your* pool's `discobox-vm-pool_<id>` on
  `preparing_volumes` or `waiting_for_agent`. A fixed delay misses: a create
  takes a second or two once the sandbox base image is on the pool. The server logs
  `pool agent went away during a sandbox create` to stderr, so to
  `$S/server.log` as started above. Sandbox containers
  are siblings on the host's Docker, so one already booted survives the restart.

## Against a release's published images and bundles

The dev loop's boxes apply the bundles its watcher exported. To drive a
release's published bundles instead, build the checkout's server with the
release's pins — the `-X` flags
of `SERVER_LDFLAGS` in `Taskfile.yml`, `REGISTRY=ghcr.io/discobox-ai`,
`PIN_TAG=<a published tag>` — and start it as above, adding `HTTPS_PROXY`,
`HTTP_PROXY` and `NO_PROXY` from your own env to the `env -i`: the server
resolves bundles itself and otherwise cannot reach ghcr.

- A box from a bundle has its harness only once provisioned: wait with
  `admin exec create <id> -- discobox-provision wait <harness>`
  (it fails until the platform's core lands; retry).
- If a nested box's `apt-get` hangs on `Connection failed [IP: 127.0.0.1
  17008]`, its pool has no upstream proxy: the runc wrapper is injecting
  nothing. `docker run --rm <any image> env | grep HTTPS_PROXY` prints nothing
  then; `sudo systemctl restart containerd` brought it back (10-23), then
  `docker restart` the pool container.

## The CLI's autolaunch, as a systemd user unit

A box has no user manager, so the autolaunch falls back to executing the
server directly, which inherits the whole environment and hides anything the
unit path would drop. Bring one up first (all of it is under `/run`, gone with
the box):

```bash
sudo mkdir -p /run/systemd/system/user@$UID.service.d /run/user/$UID
sudo chown $UID: /run/user/$UID && sudo chmod 700 /run/user/$UID
printf '[Service]\nEnvironment=XDG_RUNTIME_DIR=/run/user/%s\n' $UID |
  sudo tee /run/systemd/system/user@$UID.service.d/verify.conf
sudo systemctl daemon-reload && sudo systemctl start user@$UID.service
export XDG_RUNTIME_DIR=/run/user/$UID; unset DBUS_SESSION_BUS_ADDRESS
mkdir -p $XDG_RUNTIME_DIR/systemd/user
printf '[Socket]\nListenStream=%%t/bus\n' > $XDG_RUNTIME_DIR/systemd/user/dbus.socket
printf '[Service]\nExecStart=/usr/bin/dbus-daemon --session --address=systemd: --nofork --nopidfile\n' \
  > $XDG_RUNTIME_DIR/systemd/user/dbus.service
systemctl --user daemon-reload && systemctl --user start dbus.socket
systemd-run --user --quiet --collect --wait --pipe -- /bin/pwd   # prints $HOME when it works
```

Then, with no server of yours running and a fresh `SOCK` and fresh directories
(a server already on that socket answers, and nothing is launched), from
`env -i` with only `HOME`, `PATH`, `XDG_RUNTIME_DIR`, the `DISCOBOX_*_DIR`s
and `DISCOBOX_ENV_FILE` of the isolated server above, plus
`DISCOBOX_SERVER_BINARY=<your build>` and whatever the launch is meant to
carry (`DISCOBOX_CONFIG_FILE=<file>`, an `XDG_*_HOME`):
`discobox --server unix://$SOCK/s.sock --auto-start-server=true ls`. The
server's output is `$DISCOBOX_STATE_DIR/*.log`; `systemctl --user list-units
'discobox*'` shows the unit.

- To see what configuration reached the server without it creating a pool,
  name a config file in `DISCOBOX_CONFIG_FILE` holding `databaseDsn: postgres://127.0.0.1:1/none`: it
  logs `configuration: read <path>` and then exits on the database.
- One that does start created a real pool: clean it up as above, after
  `admin server shutdown`, then `sudo systemctl stop user@$UID.service`.

## A lead discobox's calls (the sandbox role)

The dev loop judges every `discobox-access run` with the project's default
harness, which has no model credential here, so a lead's call never gets a
verdict. Use the isolated server above with `DISCOBOX_JUDGE_COMMANDS=false`
in its env file. Then: `new -d -H shell` a lead; inside it,
`discobox-access request --json` for `ai.discobox.sandbox` with the uses to
drive and a policy (no `wait`); approve it from outside with `discobox secret
request approve <id> --policy-as-asked`; read the use IDs with `discobox-access list --json` in the
lead; and run `discobox-access run --use <id> -- discobox …` there.

- Ask for `ai.discobox.sandbox` with `--json` (`{"id":"ai.discobox.sandbox",
  "justification":…,"uses":[…],"statements":[…],"definitions":{…}}`, as
  orchestrate-issues §1 writes one): by flags it is refused for want of an
  env var and a host.
- A lead answers only the requests of discoboxes it created (`403 … only the
  requests of discoboxes it created`): have it `discobox run` the worker, then
  approve the worker's request under a `secret grant create --delegate` grant
  given to the lead. Approving under a delegation needs a judge: the stub
  judge below, whose image also serves as the boxes' harness when
  `harness-shell:local` is not built yet. Full-precision expiries are in the
  isolated server's `data/discobox.db` (`secret_grants.expires_at`); the API
  prints whole seconds.

# Verifying the iroh transport (`pkg/endpoint`: admission, peer certificates)

The isolated server above exits with `project … has no harness to run` when
no harness bundle is built (no dev loop has exported them) and ghcr
is unreachable. The transport does not need it: a probe `main` placed inside
the tree (`<copy>/probe/main.go`, so it builds in the root module) drives
`endpoint.NewIrohEndpoint` as a server (`Listen` + an `http.Server`,
`Authorize` admitting one ID) and as a client the way
`cli/internal/cli/peer_identity.go` configures it, with `DisableRelay`,
`DisableDiscovery`, `BindAddrs` 127.0.0.1:0 and `Locate` returning the
server's `DirectAddrs()`. Restarting the server endpoint with the same key and
pointing `Locate` at its new addresses forces the client's redial. For a
before/after, build it in `git archive HEAD | tar -x -C $S/old` and in a copy
of the working tree. Shorten a time constant in the copies (never in the
checkout) to reach an expiry in seconds.

# Verifying a pool route as the dev pool (the asset API, any `/api/pools/{poolId}/…`)

A pool route answers only the pool's own signed assertion, so `curl` it with
one minted from the dev pool's key. The key is in the pool container; copy it
into your scratch dir and sign with `poolauth.CreateToken` from a file outside
the repo, run with the pool-agent module (`cd pool-agent && go run $S/mint/main.go
$S/agent.key <projectId> <poolId>`). An assertion lives a minute: mint one per
call.

```bash
P=$(docker ps --format '{{.Names}}' | sed -n 's/^discobox-vm-//p' | head -1)
docker exec discobox-vm-$P find /var/lib/discobox/identity -name agent.key   # .../<projectId>/<poolId>/agent.key
```

- No pool at all — the isolated server on a copy of the loop's database,
  above: with that server stopped, insert a `pools` row whose `public_key`
  is `poolauth.EncodePublicKey` of a key you generate (`provider_instance_id`
  naming nothing, so nothing provisions it) and a `sandboxes` row on it
  (`created_by_user_id` is required), and mint with that key as above.
- The asset store is `.tmp/discobox/cache/assets/blobs/sha256/`. A blob removed
  or damaged there (`dd conv=notrunc`) is what exercises refetch and the
  digest check on the way out; a damaged one must never come back whole —
  `curl` reports a cut-off or empty reply, never a 200 of the full length.
- Linux dev bundles are in no Docker daemon: the watcher exports each to
  `build/assets/<name>/<hex>.tar`, an OCI image layout named by the manifest
  digest its index lists, and `.tmp/discobox-dev-images.json`'s `bundles`
  records its `:dev-*` reference, digest and layout (`jq -r '.bundles[] |
  "\(.reference) \(.layout)"'`). That digest is what the harness config pins
  (`d admin harness list -o json | jq '..|.bundleDigest?'`); a layout's blobs
  are `tar -tf <layout> | grep blobs/`. The daemon holds no bundle and no
  harness image: `docker image ls --format '{{.Repository}}' | grep -E
  '^discobox-(platform|harness-[a-z-]+|sandbox-agent)'` prints nothing but
  what a watcher from before layouts, or before bundles, left behind, which
  nothing reclaims (`docker image rm` them).
- A bundle edit (a harness's `launch.sh`) shows in the watcher's log as
  `built harness-<name> as …/build/assets/discobox-harness-<name>/<hex>.tar`,
  then in the server's as `updated built-in harness config bundle` with the
  new `bundleDigest`; `ls build/assets/<name>` keeps the published layout and
  the one before it. A box applies it at its next start once it is re-pinned.

# Verifying build-mode bundles (a host with no Docker: macOS, Windows)

On an isolated server (above), hand it a dev manifest whose `images` are the
dev loop's built ones (copy-mode, by ID) and whose `bundles` are build
descriptions, so its pool's daemon builds them and the server saves them:

```bash
PM=$(jq -c . sandbox-agent/image.json); HM=$(jq -c . harness/shell/image.json)
jq --arg pm "$PM" --arg hm "$HM" --arg root "$PWD" '{version:1, images:.images, bundles:[
 {reference:"discobox-platform:verify-bm1", build:{dockerfile:"sandbox-agent/Dockerfile", context:$root, target:"platform", args:{IMAGE_LAYER_METADATA:$pm}}},
 {reference:"discobox-harness-shell:verify-bm1", build:{dockerfile:"Dockerfile", context:($root+"/harness/shell"), target:"bundle", args:{HARNESS_METADATA:$hm}}}]}' \
  .tmp/discobox-dev-images.json > $S/bm.json
# server env: DISCOBOX_DEV_DOCKER_IMAGE_SYNC=true DISCOBOX_DEV_DOCKER_IMAGE_MANIFEST=$S/bm.json
# DISCOBOX_DOCKER_POOL_IMAGE / _SANDBOX_BASE_IMAGE from .env,
# DISCOBOX_PLATFORM_BUNDLE=discobox-platform:verify-bm1
# DISCOBOX_HARNESS_SHELL_BUNDLE=discobox-harness-shell:verify-bm1 DISCOBOX_JUDGE_COMMANDS=false
```

- Before the pool builds them, `shell`'s `bundleDigest` is its reference and
  `new -H shell` answers 503 "has not been built yet". The server log then
  shows `building development Docker image` / `saved a development bundle`
  (the platform ~12s on a warm cache) and `updated built-in harness config
  bundle` with the real digest; the layouts are in
  `<cache>/development-bundles/`, and the daemon keeps no `verify-bm1` image.
- A restart builds nothing: the saved layouts are read back by reference.
- The other built-ins fail to seed (`:local`, no registry): expected.

# Verifying `assets.discobox.internal` (the pool's bundle cache)

A pool-agent change reaches the dev pool only once the watcher records the new
`discobox-pool-agent:dev-*` tag in `.tmp/discobox-dev-images.json` and the pool
container is recreated on it (minutes after the build); check the running
image before driving anything.

```bash
d run -d -H shell --no-source -p true        # a box pinned to platform + shell bundle
docker exec <pool> cat /var/lib/discobox/projects/*/pools/*/sandboxes/<sbx>/bundles.json
d shell <sbx> -- curl -sS -D- https://assets.discobox.internal/discobox-platform
```

- Pins on the server: `curl 127.0.0.1:8080/projects/<p>/sandboxes/<sbx>`
  (`config.bundleDigest`, `config.platformBundleDigest`, `runtime.upgrade`).
- The pool's store is `/var/lib/discobox/cache/projects/*/pools/*/build/assets/
  blobs/sha256/` in the pool container; `rm` or `dd conv=notrunc` a blob there
  to exercise refetch and the digest check (a damaged one is cut short, never
  whole). The socket is `/var/lib/discobox/proxy/projects/*/pools/*/assets.sock`.
- A dev shell bundle has no layers; the dev platform bundle is one manifest
  (the watcher exports no index), so walk its layers for blobs. Clean up with `d rm` then `d admin box purge`.

## A delivered box's store and what it fetched

- In the box: `/opt/discobox/bundles/{pins.json,state.json,layers,trees}`,
  `readlink /opt/discobox/current`; on the pool side
  `.../sandboxes/<sbx>/agent-placed.json` in the pool container.
- What it fetched since its last start: `d admin audit http --discobox <sbx>
  --since "$(docker inspect -f '{{.State.StartedAt}}' <ctr> | cut -c1-19)Z"
  --limit 5000 | grep assets.discobox.internal` (table output: `--host` with
  `-o json` answered 503 here). A restart fetches nothing; a moved platform
  fetches its manifests and the layers that differ.
- Move the platform with a comment in one `sandbox-agent/features/*` file and
  wait for `.env`'s `DISCOBOX_PLATFORM_BUNDLE` to move; a stopped box is
  re-pinned, a running one is not until upgraded. Compare builds with
  `docker image inspect <tag> --format '{{range .RootFS.Layers}}{{println .}}{{end}}'`.
- `d rm $B` is refused by the tool's removal check (a variable target);
  type the literal id.

# Verifying a sandbox-agent route the pool drives (runtime-config, sources)

The pool delivers a runtime-config document at every boot (next section), but
to drive one of these routes with a document of your own, call the in-box agent
directly: `d run -H shell -d --include-dirty <dir>` a box (from a dirty checkout if the change
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
- **The auth middleware, from outside.** The agent also listens on the
  container's own address: `curl http://<ip>:3003/execs` from this box (`ip`
  from `docker inspect` of `discobox-sandbox-<pool>-<sbx>`) with no token, or
  a garbage `Bearer`, must answer `401`.
- **SSH direct-tcpip.** `d run` writes the ssh config, so `ssh -W
  127.0.0.1:<port> <box-name>` forwards through the server's sshd and the
  agent's `/tcp/attach`; a closed port shows the rejection reason the client
  prints (`channel 0: open failed: connect failed: ...`).

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
  `d admin harness secret bind <shell-config-id> ENV <secret>`; a new box
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
- **Every source is delivered by the client, into the pool's Git cache** (ADR
  26-10-10-545), wherever the server runs and wherever the repository is
  (`/tmp` included). A box sees new commits only after a push — `d push`, or
  an attached client's — as `discobox/<branch>`. Check with
  `d admin box get <id> -o json | jq '.config.source | {delivery, cacheKey}'`:
  `cache`, and a key of `remote/<host>/<path>` for a checkout with a remote,
  `directory/…` for one without. A CLI run with another `HOME` has another
  host ID, which changes only a `directory/…` key; pass `DISCOBOX_HOST_ID` to
  keep it. The caches are bare repositories under the dev pool's
  `…/pools/<pool>/git-cache/`, one namespace per box source
  (`git for-each-ref refs/namespaces/`).
- Sources: the pool's own record of a box's delivery is the `delivered` flags
  in its `runtime-config.json`; flipping them to `false` on a stopped box and
  starting it drives the start-time settle (`settleConverged`), which reopens
  `/etc/discobox/ready` within a poll. Run `d` as a function, not a variable:
  zsh does not split `$D` into a command.

Gotcha: a wait loop of `until ! pgrep -f "docker build"` never ends — its own
shell's command line matches the pattern. Wait on the watcher's outputs
(`.env` image tags, the harness list) instead.

# Verifying a remote-URL source's clone (`run <url>`)

- **A slow origin of your own.** Serve a bare clone of this repository with
  `git http-backend` behind a handler that writes 16 KiB per 40 ms (~400 KiB/s);
  disco2 then takes about a minute to clone, past `ProvisionProgressFresh`.
  `d run ... http://172.17.0.1:<port>/disco2.git` makes it a remote-URL source.
- **The CLI's `ls-remote` uses the box's proxy**, which cannot reach this
  box's docker bridge: run the CLI with `NO_PROXY=172.17.0.1,$NO_PROXY` (and
  `no_proxy`), or it fails `502 ... connection refused`. The sandbox's own
  clone goes through the dev pool proxy, which dials 172.17.0.1 directly.
- **What the pool recorded:** poll `d admin box get <id> -o json | jq -c
  '.runtime | {state, provisionProgressAt, provisionProgress}'` every 2s.
- **What the user sees:** `run` without `-d` in tmux. The first run in a fresh
  `HOME` stops on the console's welcome screen until Enter, which hides the
  whole create; dismiss it once, then run again.
- **A clone cut short:** stop the origin by exact name (`pgrep -x`); a
  `pkill -f` naming it matches your own tool shell. The clone's error is in
  the box's `journalctl -u discobox-sandbox-agent` (`materialize source`).

# Verifying on a libkrun pool in a dev box

The box has `/dev/kvm`; the runtime the provider dlopens is the flake's:

```bash
nix build .#libkrun-runtime -o ~/.cache/libkrun-runtime   # builds libkrun, minutes
R=~/.cache/libkrun-runtime
d admin provider create --type libkrun --name krun --libkrun-path $R/lib/libkrun.so \
  --passt-path $R/bin/passt --vcpus 4 --memory-mi-b 8192 --data-disk-gi-b 60 --cache-disk-gi-b 30
d admin pool create krunpool --provider <prov_id>
```

- The VM boots the published guest, then the dev loop copies every watcher
  image into its Docker (`synchronizing development Docker images` in the
  server log): minutes, and the pool reads `pending` until it is done.
- The published guest may predate `vm-image`'s move to the containerd image
  store. On the classic store a copied image loses the index digest its
  sandbox is pinned to, and the box fails with `the sandbox is pinned to image
  … but … now resolves to …`. Boot a guest built from the checkout instead:
  `go tool task build:vm-guest` stages it where the provider prefers it, then
  delete and recreate the pool. (`d admin pool build-guest` builds on the
  pool's own Docker, which cannot reach a registry from inside a discobox.)
- `d admin pool rm` of a failed libkrun pool can leave it listed, and the
  provider then refuses deletion (`provider instance has pools`).

# Verifying the sandbox agent with nothing to wire (a VM guest's start)

No disco-vm driver runs here; a box's image booted with no config volume is
the same start a VM guest makes: `discobox-wire.service` is skipped and the
agent provisions from its own start. The sandbox base image has no agent or
units of its own — its pool places both before each start — so boot a stopped
dev box's container committed (`docker commit <its container> vwire:test`,
as for the wiring recipe below), which carries what its pool placed.

```bash
docker run -d --name vmlike --privileged --cgroupns=private --tmpfs /run \
  --tmpfs /run/lock vwire:test
# a real bootstrap: any dev box's, with another sandboxId
d shell <box> -- sudo cat /etc/discobox/sandbox.json | jq '.sandboxId="sbx_vmlike"' > $S/s.json
docker exec -i vmlike sh -c 'cat > /etc/discobox/.s.tmp && mv /etc/discobox/.s.tmp /etc/discobox/sandbox.json' < $S/s.json
docker exec vmlike journalctl -u discobox-sandbox-agent -o short-precise
```

- Place the file by a rename, as a backend must; a `cat >` straight to the
  path can be read half-written.
- Without systemd at all (launchd/SCM stand-in): `--entrypoint
  /opt/discobox/current/features/core/discobox-sandbox-agent ... --config /etc/discobox/sandbox.json`;
  it logs `waiting for a file to appear` and exits 0 on SIGTERM.
- `docker restart` is the guest's reboot; `systemctl kill -s KILL` the crash.

# Verifying feature provisioning (`sandbox-agent/provision`, provision.json)

A box provisions the trees at `/opt/discobox/bundles/{platform,harness}` on
every boot. A dev box is delivered its platform (ADR 26-10-16-423 §7): its
container is `discobox-sandbox-base:dev-*` with `/sbin/init` as PID 1, the
pool places the agent layer, units and `pins.json`, and the agent fetches
the rest into its store, then provisions from scratch — core blocks (ready in
~30s), the rest installs in the background through the pool proxy (`nix`
takes minutes: it installs into a private namespace, then seeds the pool's
`/nix` on first use; check with `d shell $B -- bash -lc 'nix eval --expr
"1 + 2"'`). To try a
feature of your own, drop a tree in and restart:

```bash
# scratch repo whose settings turn features off; -o json shows config.features
git init $S/app && mkdir $S/app/.discobox && echo '{"features":{"brew":false}}' > $S/app/.discobox/settings.json
d run -H shell -d $S/app -o json           # commit something first
# a tree: features/<name>/provision.yaml + scripts, tarred and placed by root
d shell <box> -- sh -c 'sudo tar -xz -C /' < $S/tree.tgz
d stop <box>; d start <box>                # a real boot: wiring in discobox-wire, the rest in the agent
d shell <box> -- cat /run/discobox/provision.json
```

- A slow `run` step (`sleep 90`) shows readiness first: `ready: true` with the
  feature `provisioning`, and its `commands:` as placeholders in
  `/usr/local/lib/discobox/placeholders`; `d shell <box> -- sh -lc '<cmd>'`
  waits, then runs the real one.
- Edit a script in the box to re-run its step (its digest changes); the rest
  report `how: snapshot`.
- The status report is `d ls -o json | jq '.sandboxes[] | .runtime.agentStatus.provision'`;
  the console shows it in place of the name under the cursor (Enter past the
  welcome card, cursor on the row).
- A failing blocking step (core's) fails `d attach --raw <box>` with the
  readiness error naming the log; a failing background one fails only its
  placeholder (exit 127).
- Purge with `d admin box purge <box>` (`d rm` only archives).
- A change to a feature (`sandbox-agent/features/*`, `harness/*/provision.yaml`)
  is proved by the image it builds, not only the box: build `main`'s image
  from `git archive HEAD | tar -x -C $S/base` and this tree's, run one script
  in each (`docker run --rm --entrypoint sh -v $S/inv:/inv:ro <image>
  /inv/inventory.sh`) listing `dpkg-query -W`, the commands on PATH, the
  links and file hashes under `/etc/systemd/system`, `/etc/profile.d`,
  `/etc/xdg` and `/usr/local/share/discobox`, and each tool's `--version`, and
  diff the two. It catches what feature order changes: a package's conffile
  written before the package, `ldconfig` from a later install undoing a link,
  apt resolving an alternative another feature used to satisfy.
- `admin harness configure <id>` in a tmux pane reaches a login harness's CLI
  in a box of its image without credentials; answer `n` to leave it.

# Verifying a feature's Ubuntu path

The sandbox base image is Debian, so a feature's `ubuntu:` packages and its
`when: {distro: [ubuntu]}` steps only run in a box of an Ubuntu base:

```bash
go tool task build:ubuntu-sandbox-base > $S/ubuntu.log 2>&1
# start the isolated server above with
# DISCOBOX_DOCKER_SANDBOX_BASE_IMAGE=discobox-sandbox-base:ubuntu, then
e new -d --no-source -H shell -p x
e admin exec create <id> -- discobox-provision wait <f>
d shell <id> -- sudo cat /var/log/discobox/provision/<f>.log   # result=applied per step
```

Remove `discobox-sandbox-base:ubuntu`, and the isolated server's pool,
afterwards.

# Verifying a container's boot (`discobox-wire.service`)

A dev box shows the wiring: `d shell <box> -- systemctl show discobox-wire -p
ActiveState`, `sudo journalctl -b -u discobox-wire` (`wired sandbox volumes
count=N`), and `findmnt` for the declared paths. `d stop`/`d start` is a new
boot that re-wires; a normal stop exits the container 130.

A failed wiring, without a pool: the sandbox base image carries no agent or
`discobox-wire.service` — its pool places both before each start — so take an
image that does, a stopped dev box's container committed
(`docker commit <its container> vwire:test`). Give it a config volume whose
bootstrap the wiring rejects, and the container exits 1 with the error in the
tail of `docker logs`.

```bash
mkdir -p $S/vfail-config   # not $S/config: the isolated-server recipe uses that
echo '{"sandboxId":"sbx_vfail","user":{"uid":1000,"gid":2000,"groupName":"docker","name":"dev"}}' > $S/vfail-config/sandbox.json
docker run -d --name vfail --privileged --cgroupns=private -t \
  -v $S/vfail-config:/.discobox/config vwire:test
docker inspect vfail --format '{{.State.ExitCode}}'; docker logs vfail | tail -20
```

- A *working* wiring needs no pool either: give the same container a bootstrap
  with a `user` (`name`, `uid`, `gid`, `homeDirectory`) and top-level
  `volumes`, plus `-v $S/data:/.discobox/data -v $S/cache:/.discobox/cache`.
  `docker exec` then shows `findmnt --mountpoint <path>` (one per call) and
  the layout records in `/.discobox/data/.layouts`; `docker stop`/`start` is
  the next boot. Edit `$S/data` with `sudo` (root-owned) between boots. For
  `main`'s behavior on the same tree, build its agent from `git archive HEAD`
  and bind it over `/opt/discobox/current/features/core/discobox-sandbox-agent:ro`.
- A box's export is `d admin box export <box> <file>` (stop it first); the
  export container overrides the entrypoint. Without a pool: the same image
  with `--entrypoint /opt/discobox/current/features/core/discobox-sandbox-agent ... export data`,
  config and data mounted read-only, and `DISCOBOX_USER_{NAME,UID,GID,HOME}`
  set; stdout is the tar.

# Verifying what the judge is asked (judge jobs, facts)

To see the job a judge discobox is handed, without a model: on the isolated
server above, register a stub harness whose `discobox-prompt` logs `--prompt`
and allows, and make it the project's default, which brings up a judge.

```bash
# Bundle discobox-harness-judgestub:local: FROM discobox-harness-stub:local (task
# build:harness-stub-bundle), LABEL io.discobox.image.v1 from
# test/harness-stub/image.json with a new harness.id.
# Its feature also installs a /usr/local/bin/discobox-prompt that appends its
# --prompt to /var/tmp/judge-prompts.jsonl and prints {"allow": true, "reason": "stub"};
# export it as a layout the isolated server's dev manifest lists.
e() { ./build/discobox --server http://127.0.0.1:18471 "$@"; }   # the isolated server, never the dev loop's
e admin harness create --bundle discobox-harness-judgestub:local
e admin harness configure judgestub && e admin harness set-default judgestub
docker exec discobox-sandbox-<pool>-<judge sbx> cat /var/tmp/judge-prompts.jsonl
```

- A bundle registered with `admin harness create --bundle` is never served
  to a pool: the asset API reads layouts only for the references the release
  names (the platform and built-in harnesses), so its configure box fails
  with `fetch the box's bundles … 502`. Stand the stub in for a built-in
  instead: build it with that harness's own `harness/<id>/image.json` label
  and the prompt feature under `/features/<id>/`, export it with `docker
  buildx build --builder default --provenance=false --sbom=false --output
  type=oci,dest=<tar>`, add `{reference, digest, layout}` (digest from the
  tar's `index.json`) to a copy of `.tmp/discobox-dev-images.json` named by
  `DISCOBOX_DEV_DOCKER_IMAGE_MANIFEST`, and point that harness's
  `DISCOBOX_HARNESS_<ID>_BUNDLE` at it. Not `shell`: the server never makes
  a judge of it ("runs no model"); `pi` works, and is configured with no
  credential.
- Every judged command's verdict records the job as the judge read it:
  `sqlite3 -json $S/data/discobox.db "select command, prompt from
  credential_verdicts"`, `.prompt | fromjson | .facts`. Wrapped as
  `{name, why, expect, job}` cases, those jobs go to a real model with
  `discobox-judge-eval -wrapper harness/claude-code/prompt.sh` (the box's
  `claude` is logged in); to compare against `main`, run the same cases from
  `git archive origin/main` extracted to a scratch dir.
- The judge discobox comes up on the judges' scan, up to a minute after
  `set-default`; its ID is `projects.judge_sandbox_id` in
  `$S/data/discobox.db` (the API does not show it, and `admin box ls` hides it).
- A lead's calls to the discobox API (the gate) are request jobs, judged only
  with `DISCOBOX_JUDGE_CREDENTIALS=true` in the env file; with the stub judge
  as the default harness, commands can stay judged too. The job's `facts` name
  the discobox a call targets (ADR 26-10-14-320 §5).
- `discobox-access run` in a box triggers a command job. A delegation job is
  asked only for a condition code cannot compare: have a box create a child
  with `--grant` and a `--grant-statement` whose allow words its `when`
  otherwise than the `purpose: delegate` grant's (`secret grant create
  --delegate --statement …`).
- Approving `com.github.api` with a secret bound only to `api.github.com`
  needs `--hosts api.github.com`: the well-known credential's default hosts
  include `github.com`. Do the same with `--grant com.github.api@api.github.com=…`.
- A grant that carries a policy: ask for it from the box with
  `discobox-access request --statement … --define WORD=DEFINITION` (or
  `statements`/`definitions` in `--json`), from a box on an image built with
  your `discobox-access`, and approve it with `secret request approve <id>
  --secret N --policy-as-asked`. Create the box from SOURCE
  `https://github.com/octocat/Hello-World` so `$primary.repo` has a value.
  The grant's `.policy.resolved` (`GET /projects/<p>/secret-grants`) shows
  what bound; `discobox-access wait request <id>` shows it as sentences.
- To see what a real judge does with a captured job, wrap it as a
  `test/judge-evals` case (`{name, why, expect, job}`) in a scratch directory,
  and run `go run ./internal/cmd/discobox-judge-eval -wrapper
  harness/claude-code/prompt.sh <dir>`.

# Verifying the exec lifecycle (sandbox-agent/execs: shims, runtime files)

Every `d shell <box> -- <cmd>` is one of the agent's shim-backed execs, so a
burst of them drives `execs.Manager`, the shim and the watcher end to end; its
exit status comes back as the client's (`d shell $id -- sh -c 'exit 7'; echo $?`).

- The runtime files are in `/run/discobox/agent-terminals` (the box's
  `sandbox.json` `agentRuntime.runtimeDir`), not the package default
  `/run/discobox/execs`. It is 0700 root: run the whole script under
  `sudo sh -c '...'`, or the glob expands as the box user and matches nothing.
- The box has `jq` but no `inotifywait` or `stat`; `ls -i` shows whether a
  write replaced the file (new inode) or rewrote it in place.
- A probe you run with `d shell` is itself the newest exec; pick the file by
  its `command`, not by `ls -t`.
- The dev server restarts whenever the image watcher finishes an image, which
  drops every `d shell` in flight. Run an in-box loop detached (`nohup sh
  script > /root/log &`) and read its log afterwards.

# Verifying sleep and wake (autostop, discobox-power, the power reconcile)

Drive it from a `-H shell` box of the dev pool (ADR 26-10-16-337). As the box's
user: `docker exec -u <user> -e HOME=/home/<user> <discobox-sandbox-...>
discobox-power sleep --wake-in 2m --note x`, then watch the row:
`sqlite3 -readonly .tmp/discobox/discobox.db "select runtime_state,
wake_at, wake_epoch from sandboxes where id='<sbx>'"` every 10s.

- Handed over first: `admin box get <id> -o json | jq .runtime.agentStatus.autostop`
  shows `controlPlanePower: true` *and* an `idle` field once the pool has
  delivered `agent.controlPlanePower` (a second runtime-config revision); the
  box has `/run/discobox/control-plane-power`.
- Expected: `stopped` with `wake_at` set within ~20s (a status poll, a push,
  a reconcile), `running` with
  `wake_at` cleared a few seconds after it; in the box, `sandbox woken` in
  `journalctl -u discobox-sandbox-agent`, the wake under
  `~/.discobox/wake/.done/`, and `DISCOBOX_WAKE` in the primary shell's
  `/proc/<pid>/environ`.
- **Wait for the image watcher to settle first.** A sandbox image rebuilt while
  the box sleeps makes the pool replace its container on the wake's start
  ("replacing sandbox container ... a spec change" in the pool's log); a
  replacement right after boot reboots past the primary the wake was handed to,
  and the next boot finds nothing due — a known limitation, not only a
  dev-loop one (ADR 26-10-16-337, Consequences).
- Cancelling: while it sleeps, `discobox stop <id>` clears `wake_at` and moves
  `wake_epoch`; nothing starts it at the wake time.
- Idle is the same path with `reason=idle`, but the dev pool's 30-minute
  timeout cannot be shortened in place (see the runtime-config section).

# Verifying the files API (`sandbox-agent/files`, `.../files/*`)

Drive it through the dev loop's server with curl, as the default user
(ScopeAll), on a box made after the image watcher has exported the platform
and harness bundles (`.env`'s `DISCOBOX_PLATFORM_BUNDLE` and
`DISCOBOX_HARNESS_SHELL_BUNDLE` name layouts under `build/assets/`), so the
box is pinned to an agent carrying the change.

```bash
B=http://127.0.0.1:8080/api/projects/default/sandboxes/<sbx_id>/files
curl -s "$B/stat?path=~"
printf hi | curl -s -X PUT -H 'Content-Type: application/octet-stream' \
  --data-binary @- "$B/content?path=notes/a.md&parents=true"
curl -s "$B/content?path=/etc/shadow" -w ' [%{http_code}]\n'   # 403: runs as the user
d shell <sbx_id> -- sh -c 'ls -ln ~/notes/a.md' </dev/null   # owned by the user, not root
```

- There is no `discobox exec`; `shell <id> -- <command…>` runs one, and needs
  `</dev/null` from a tool shell.
- The CLI's callers: a user tool script (`$XDG_CONFIG_HOME/discobox/tools/<n>.sh`,
  `tools <n> <id>`), a tool's `files:` (a `.yaml` tool with the
  default under `tools/<id>/`), a pasted image (console recipe above), and
  harness configure (`go tool task build:harness-stub-bundle`, `admin harness
  create --bundle discobox-harness-stub:local`, served as above, `admin
  harness configure stub` twice in
  tmux: the second run prints the seeded previous config). Delete the stub
  harness after purging its archived `configure-*` boxes, which hold it.


# Verifying a Docker pool's box (`dockerworker/poolbox.go`, the pool bundle)

The dev pool is the box (ADR 26-10-24-953): its container
`discobox-vm-<poolId>` runs the sandbox base image, and the engine places the
agent, runs `provision identity`, enrolls the key, and runs `provision apply`.

- A change to the image watcher itself (`internal/cmd/discobox-docker-image-watch`,
  a new bundle in it) is not picked up by the running loop: it is a `go run`
  from the box's boot. Kill the `discobox-docker-image-watch` process (not the
  dev-lock or `go` above it, and not the server's half), then run `go tool task
  dev:docker-image-watch` in the background; the server keeps running and
  restarts onto the `.env` it writes.
- A fresh pool: `d admin pool create <name> --provider <the Default pool's
  provider>`, then poll `d admin pool ls` to `active` (about 50s). The box's
  key is `/var/lib/discobox/identity/<project>/<pool>/agent.key` in the
  container; `docker exec <c> /opt/discobox/current/features/core/discobox-sandbox-agent
  provision identity --key <it>` prints its public half, which must equal
  `sqlite3 .tmp/discobox/discobox.db "select public_key from pools where id='<pool>'"`.
- A failed apply lands whole in the pool's `errorMessage`
  (`d admin pool ls` cuts it); `d admin pool get <id> -o json | jq -r
  .errorMessage` reads it, though jq has rejected that output while the
  message held multi-line provision logs.
- An in-place re-apply: edit a pool bundle feature, wait for the watcher's
  `built pool-agent-bundle` and the new `DISCOBOX_POOL_AGENT_BUNDLE` in `.env`;
  the container's ID and `StartedAt` stay, and `readlink
  /opt/discobox/bundles/by-name/*` moves to the new digest.
- Identity survives `docker restart` and `docker rm -f` of the container (the
  identity directory is a host bind mount): the recreated box re-enrolls the
  same key. Delete a test pool with `d admin pool rm <id>`.
- The release manifest: `./build/discobox-server manifest` refuses in a dev
  build (its `:latest` defaults); render the release link with `RELEASE_TAG=v9.9.9
  go tool task release:binary TARGET_OS=linux TARGET_ARCH=amd64
  REGISTRY=ghcr.io/example RELEASE_REPO=example/discobox --dry` and run its
  `go run -ldflags=... ./server/cmd/discobox-server manifest`. Never run
  `discobox-server` with any other argument to "try" a flag: it starts a server.
