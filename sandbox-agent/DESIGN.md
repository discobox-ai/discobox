# Sandbox Agent Design

This module owns the sandbox runtime environment and in-sandbox agent REST API
implementation.

The Go implementation serves the generated sandbox-agent subset of
`api/openapi/server.yaml` using the generated `api/sandboxgen` server scaffold.
It validates the sandbox's hard-coded project/sandbox identity, accepts
short-lived control-plane-signed tokens, and owns the sandbox-local exec,
terminal, and service runtime.

## Package Map

| Package/path | Ownership |
| --- | --- |
| `cmd/discobox-sandbox-agent` | Binary entrypoint, config loading, signal handling, and server startup. Also dispatches the `init` PID-1 subcommand, the per-exec `exec-shim`, and the `desktop` viewer, and publishes a hook event when invoked as `discobox-hook-publish` (an `argv[0]` symlink the image installs). |
| `boot` | The PID-1 `init` flow: resolves the sandbox user and adds it to the manifest's `additionalGroups`, wires image-declared data/cache volumes and manifest sources from the primary volumes, binds the config volume onto `/etc/discobox`, seeds the user's `~/.gitconfig`, `~/.config/direnv/direnv.toml`, and `~/Desktop` launchers, writes desktop drop-ins, then execs the container's real init (systemd). See ADR 0007. |
| `config` | Decodes `/etc/discobox/sandbox.json` — the effective config pool-agent already computed; nothing is merged here — into the config the agent runs on, with defaults and validation. Carries the image-contributed `volumes` declaration; its `%HOME%`/`%UID%`/`%GID%` tokens are resolved by the root `harness` package when `boot` wires the volumes. |
| `server` | HTTP router, generated OpenAPI handler adapter, PASETO auth middleware, and identity/scope validation. Also hand-registers the exec attach/start routes and, for ADR 0024's SSH ingress, `GET .../tcp/attach` (`tcp_attach.go`): dials `host:port` from inside this process — sharing the sandbox's network namespace, unlike the pool-agent's container-IP-only `/http/{port}` — and bridges the raw TCP bytes to `execstream/frame` `Input`/`Stdout`/`CloseInput`/`CloseOutput` frames over a websocket, gated by the `tcp:connect` scope. Each direction ends on its own: a `CloseInput` half-closes the TCP conn's write side, and the target's own EOF sends `CloseOutput` and keeps the tunnel open for whatever the client is still sending. In config mode it also provisions `harness.ConfigureDir` (`configuredir.go`) before anything can be seeded into it: the configure command runs as the sandbox user, and this 0700 subdirectory is the only part of root-owned `/run/discobox` that user may write — the rest holds the resolved secrets file, the proxy CA bundles and trust env, and the control-plane and buildkit sockets. Like `boot.WireSecrets`, it runs from this process rather than the PID-1 flow, because systemd mounts its own tmpfs over `/run` after boot execs into it. |
| `runuser` | The sandbox's run identity: merges the image/manifest/request layers ([`sandboxuser`](../sandboxuser/DESIGN.md)) and completes them against the image's own passwd/group database. The **only** package that reads those files for resolution, so faking them fakes them for every consumer including the `setuid` path. Imported by `boot` and `execs`; `terminal` reaches it only through `execs.Manager.ResolveUser`, so none of them derives identity separately. See [runuser/DESIGN.md](runuser/DESIGN.md). |
| `execs` | The sandbox runtime primitive: exec lifecycle, runtime metadata, systemd unit abstraction, stdout/stderr or PTY logging, shim launch, status socket, and attach. Harness terminals are execs. |
| `execs` (`shim.go`) | Per-exec child process: the local Unix socket attach/status/start API, the audit log, and the runtime status file. The process itself is `procio`'s. |
| `procio` | Running a process and owning its descriptors: PTY versus pipes, stdin close, signal mapping, and exit status. No sockets, no frames, no attach — which is what makes its traps testable with a real process and nothing else. |
| [`services`](services) | The sandbox's declared services, from the repository's `.discobox/services` and the image's own directory in the same format, which the sandbox starts at boot and `discobox admin services` and the workspace act on afterwards ([ADR 0070](../docs/adr/0070-services-are-declared-execs-the-sandbox-starts-for-you.md)). Like `terminal` it is a typed layer over `execs` and owns no runtime of its own: a service is an exec created with `Shell`/`ShellCommandLine` (the script, under the run user's login shell), on pipes rather than a PTY, tagged `serviceId`/`serviceName` in exec metadata, and run in the repository root it was declared in. That workdir is named on the request rather than left to the exec default: the two resolve to the same directory today, since discovery is rooted at that default, but a service script reads its own repository through relative paths and which directory those are relative to must be a property of the declaration rather than a coincidence of two derivations agreeing. It keeps no state — declarations are re-read from disk on every listing, and run state is the exec record — so two clients, or a client and the boot flow, cannot disagree about what is running. A declaration may also name the ports it serves, which `ports` reports whatever the scan finds ([ADR 0076](../docs/adr/0076-a-service-may-declare-a-port-discovery-cannot-see.md)); `Declarations` is the seam it is asked through, and it reads the files rather than the exec records, because a port is declared by a file and not by a run.  It reads two directories in the same format ([ADR 0094](../docs/adr/0094-an-image-declares-services-in-the-format-a-repository-does.md)): the repository's, and the image's at `/usr/local/share/discobox/services` — the same pairing `.discobox/skills` has with the image's skills directory (ADR 0080), because some services are true of the sandbox whatever repository is worked on in it. A repository declaration wins on a shared id. Three front-matter keys exist for the image's case: `protocol:`, reported instead of probing the port; `start: never`, which declares ports without declaring anything to run — a socket-activated unit already serves them, and a declaration with nothing to run is not a broken script; and `id:`, a stated lowercase reverse-DNS identity replacing the filename-derived one, so a client can recognize `ai.discobox.desktop` whatever the file is called. That prefix is reserved for image declarations, because a repository wins on a shared id and one claiming the desktop's would take its link with it. |
| `terminal` | Harness-terminal layer built on top of `execs`: image harness resolution, configured-file setup, primary-terminal lifecycle, and revive-in-place. A terminal is an exec created in harness mode, tagged `harnessId`/`primary` in exec metadata, and its exec id is its durable identity across runs (ADR 0038); all runtime mechanics belong to `execs`. Also owns the one thing about an installed file that outlives its install (`secretfiles.go`): a templated file that rendered a sentinel must still contain it, because a harness reading an upstream 401 as an expired login clears its own credential file and cannot restore it — the refresh token it holds is a placeholder, since refreshing happens in the control plane. A standing loop re-renders any such file, so the next launch is signed in. See ADR 0059. Owns the skills install (`skills.go`): two trees copied into `~/.claude/skills` and `~/.agents/skills` on the primary terminal's *first* launch only — the image's own at `/usr/local/share/discobox/skills`, which every sandbox gets whatever it was made from (ADR 0080), then the primary source's `.discobox/skills`, which exist only while that repository is worked on inside a sandbox, beside the `.discobox/services` [`services`](services) reads (ADR 0072). The repository is copied second, so it wins on a name the two share. It hangs off the first launch rather than the per-terminal installer because the copies belong to the harness once they land, and because it reads the source tree, which only that launch is sequenced behind. An absent image tree installs nothing — that is an older image, not a misconfiguration. |
| `shimruntime` | The platform half of an exec attach: Unix socket setup, the HTTP upgrade, the PTY, and the screen emulator behind repaint-on-attach. The stream itself — attachers, ordering, buffering, exit retention — is the root module's `execstream/host`, which this drives and implements `host.Replayer` for. |
| `shimproxy` | The agent's client of a shim's Unix socket: status reads on a short dial timeout, and bridging an exec attach through to the caller as an HTTP upgrade or a websocket (`AttachHTTPUpgrade`, `AttachWebSocket`). |
| `hooks` | Local Unix-socket collector and publisher protocol for coding-harness lifecycle payloads. Arbitrary run users publish through a dedicated sibling runtime directory (0711) and writable socket (0666), while terminal attach sockets remain private. These events are telemetry, not an authorization boundary: they are recorded for `discobox hooks logs` to read back, and nothing derives behavior from them. |
| `secretswatch` | Watches the resolved-secrets file (`/run/discobox/secrets/secrets.json`) and serves its current env-name → sentinel map, which `server.Serve` wires in as `SecretEnv`. Watched live rather than read at boot, because grants, rotations, and OAuth refreshes rewrite it independently of `sandbox.json` (ADR 0012 §3). |
| `sourcesready` | The wait the sandbox's boot work clears when a source is still being delivered by the client (`Source.AwaitsDelivery`): pool-agent's `/etc/discobox/ready` signal, watched with fsnotify and backstopped by a one-second re-check. Two things wait on it — the *first* harness launch, and the start of the repository's declared services — and they share one gate built in `newRouterAndManager` rather than each constructing one, because anything reading the working tree at boot has to be behind the same answer. A sandbox whose sources were in place when its container was built acquires no wait at all, and the boot path is unchanged and stats nothing. It cannot live in `boot`: that runs as PID 1 *before* systemd, so blocking there would keep the agent itself from starting and fail the pool host's create handshake. See ADR 0055. |
| [`runcca`](../runcca) (root module) | The sandbox's runc wrapper, built here as `cmd/discobox-runc`: installed as `runc` ahead of the real one on containerd's and dockerd's PATH, it mounts the sandbox's CA trust bundles and injects proxy-trust env into every container's OCI spec, so a user's Dockerfile or `docker run` never needs MITM awareness. Installing the bundle also *names* it — `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `PIP_CERT` — because Node, Python's `ssl`, requests/certifi and pip each carry a root store of their own and ignore the system one; that comes from the mount rather than from `sandbox.json`, so a pool-side build container, which has no manifest to read, gets it too. Only where nothing has set them already. See ADR 0020, which supersedes 0015's NRI plugin — containerd invokes NRI only from its CRI path, which dockerd does not use. Handles both `runc create` (the `docker run` path, via the containerd shim) and `runc run` (the `docker build` path, via BuildKit's executor). Bundles are staged per boot by `discobox-trust-ca.service` under `/run/discobox/proxy/ca-bundles`; they cannot live beside the CA in `/etc/discobox/proxy`, which is pool-agent's read-only mount. |
| `nestedbridge` | Discovers the nested Docker daemon's bridge address and publishes it under `/run` for the bridge-facing proxy forwarder and the runc wrapper. Also enumerates this sandbox's own directly-connected IPv4 networks (`LocalSubnets`), the resolution target for `sandboxconfig.LocalSubnetsToken`. |
| `proxyenv` | Renders `sandbox.json`'s proxy-trust env (`Env`/`ProxyEnvs`) as a systemd `EnvironmentFile`, for `docker.service` and `nix-daemon.service` — both started by socket activation, not spawned by sandbox-agent, so they inherit no container env and cannot be reached by any per-container injection. Both do their own fetching (image pulls; substitutions and builds the `nix` client hands off), so their egress is the sandbox's egress. Resolves `sandboxconfig.LocalSubnetsToken` against `nestedbridge.LocalSubnets()`, the same substitution `runcca.proxyEnv` applies for nested containers. Run at boot by `discobox-render-proxy-env.service`, ordered before both daemons, writing to `/run/discobox/proxy/proxy.env` (not `/etc/discobox`, which is pool-agent's read-only mount). |
| `dockercache` | The sandbox's `docker` CLI wrapper (`cmd/discobox-docker`): installed as `docker` ahead of the real CLI on PATH, it points `docker build` at the pool-shared BuildKit builder so a build in one sandbox is reused by the others (ADR 0044). It provisions a buildx `remote` instance against the pool mediator, rewrites the build to push its result to the pool registry, then pulls the result back and applies the user's tags — `--load` would reship the whole image on every build, cached or not. Only `docker build` is rewritten; `docker buildx` and everything else is exec'd straight through, so a user who reaches for buildx directly gets buildx. |
| `credentials` | The in-sandbox *server* half of the agent credentials protocol (ADR 0031): a loopback endpoint on the protocol's well-known address that relays list/request/get to the pool over the sandbox's own mTLS client certificate. It holds no credential and makes no authorization decision — it puts a URL in front of the sandbox and carries each call one hop out, where the certificate is the identity. The client half is the separate [`access`](../access/DESIGN.md) module, so the CLI stays liftable into another repository. |
| `agentstatus` | Computes the status a pool agent polls (ADR 0030): per-source git status and a diff stat against the manifest's base commit via bounded `git status`/`git diff --shortstat` shelling, and per terminal session (every terminal still on record, never one-shot execs) its exec status, attacher count, and the OSC title the session's program last set, read from its shim's emulator. What the harness *inside* a session is doing is deliberately not reported: deriving it would need a per-harness hook-event mapping, and no client reads it. Computed fresh on every call, never cached. Listening ports travel in the same payload but come from `ports` instead, and the idle stop's view from `autostop`. |
| `ports` | The TCP ports the sandbox serves: a standing watcher that reads `/proc/net/tcp{,6}` for sockets in `TCP_LISTEN` owned by the run user's uid, and probes each newly seen socket once to classify it `http`/`https`/`tcp` (ADR 0046). It has a second input — the ports `services` declarations name, folded into the same snapshot and marked `declared`, for the sockets the uid filter cannot see because root holds them ([ADR 0076](../docs/adr/0076-a-service-may-declare-a-port-discovery-cannot-see.md)). The only status component that is a cached snapshot rather than computed per request, because classifying a port means connecting to a user's process. A declaration may also state what its port speaks, and then it is believed rather than measured ([ADR 0094](../docs/adr/0094-an-image-declares-services-in-the-format-a-repository-does.md)) — not an optimization: classifying a port means connecting to it, and connecting to a socket-activated port is what starts the service behind it, so probing the desktop's 6900 would boot an X server, a window manager and a VNC server in every sandbox on a timer. The declaration's id and display name also reach the snapshot as `serviceId`/`serviceName` — the pair the exec metadata carries, for the same reason — so a client recognizes `ai.discobox.desktop` and draws it as a Desktop link rather than as an HTTP port on 6900. |
| `autostop` | The sandbox's idle stop ([ADR 0108](../docs/adr/0108-a-sandbox-stops-itself-when-its-terminals-go-quiet.md)): a standing loop that starts `poweroff.target` (non-blocking, `replace-irreversibly`) once nothing has happened in the sandbox for the idle timeout — the pool's `agentRuntime.idleTimeout` from its provider instance's pool policy, 30 minutes when unset. Activity is the latest of each exec's title *change*, attachers and last access (detach included), all reported by its shim; every client connection this process serves — exec attaches, one-shot attaches, and TCP tunnels — held through `Policy.Hold` for as long as it is open, its leaving recorded as activity; the mtimes of keepalive leases, any regular file in the sticky `/run/discobox/keepalive`, a future mtime included; and the agent's own start. Exec activity and leases are re-read on every evaluation. Kept besides are the open connections and the latest non-lease activity ever seen, because an exec that ends takes its shim's record of access with it; leases are never remembered, so removing one releases it. It does not call the pool: the container exiting is the whole signal, reported `stopped` like any other exit, and the pool agent's auto-start latch brings it back (ADR 0017 §§10, 12). Configure-mode sandboxes do not run it. Its view rides the status payload as `autostop`. |
| [`desktop`](desktop/DESIGN.md) | The sandbox's desktop as a page: the branded viewer on `127.0.0.1:6900` that embeds noVNC, sizes the X screen to the browser window, and records what a person draws on that desktop where the agent in the same sandbox can read it. It joins the socket-activated desktop stack rather than replacing any of it — `xvfb.service` pulls it up as a companion beside `xfce4-session@<user>.service`, and `/websockify` reverse-proxies the existing 6080 socket so the whole desktop is one port to forward. Size follows the window continuously through `POST /api/display` (server-side, not by reconnecting, because x11vnc's `-xrandr resize` pushes the change to a live client); scale does not. Scale is an integer settled once per session — at boot from what `scale.env` remembers, 2× when it remembers nothing, and afterwards only when a viewer reports its screen or a person picks one — and the unit is `Type=notify` with `xfce4-session@.service` ordered after it, because readiness here means "the scale is settled": `GDK_SCALE` is read once per process, so a session started before the scale is known draws its panel and window manager at the wrong one, and changing it afterwards means restarting that session. Density is written to xfconf's `xsettings` channel rather than with `xrdb`, because `xfsettingsd` owns that value and publishes it to both XSETTINGS and the resource database; reaching xfconf from a separate unit is what `discobox-desktop-bus.service`, the session bus the desktop shares on a fixed path, exists for. Annotations land in `~/.discobox/desktop-feedback/feedback.md` — the Markdown *is* the store, appended to and parsed tolerantly, because an agent's reply to a note, written into that file, must not also have to be written into a sidecar index it does not know about. Ticking a note done is the viewer's, never the agent's. |
| `resources` | Opaque cgroup/procfs/systemd-style resource snapshot collection for exec runtimes (`collector.go`), and the sandbox-wide cumulative usage the status endpoint reports (`usage.go`; see [Resource Reporting](#resource-reporting)). |
| `store` | Sandbox-local SQLite/GORM audit log, exec records and observed state snapshots, retained resource blobs, harness hook events (what `discobox hooks logs` reads), the primary-terminal launched marker (`AgentState`), and compressed exec/terminal transcript chunks (see ADR 0028). |
| `image.json` | The base manifest layer every harness image inherits (ADR 0086 §2), baked in as the `io.discobox.image.v1.10-sandbox-base` label: the env and PATH, the data/cache volumes `boot` wires, the `additionalGroups` the sandbox user joins (`brew`, `docker`, `kvm`), and seed harness files. A group grant on a host device such as `/dev/kvm` lands only because the image's GID for that group matches the host's — both are the same Debian release installing the same packages. |
| `Dockerfile` | The base sandbox runtime image: development tools, Chromium, a socket-activated Xfce desktop, and Nix and Homebrew tooling, layered on the shared [`base-image`](../base-image/Dockerfile) that supplies Debian, Docker, and the trimmed systemd unit set (ADR 0068). Its installs are one layer per tool group, ordered stablest first, so adding a package does not re-pull every language runtime. Harness image builds live in their owning `harness/<type>` folders. |
| `image/desktop` | Everything that makes the sandbox's Xfce desktop *the brand's* desktop: the xfconf channel defaults that replace Debian's (theme, backdrop, one workspace, no compositing), the single bottom panel that replaces its two, the `wallpaper.svg` the backdrop is rendered from, and `brand-theme`, which derives the GTK and xfwm4 theme at image build — including one pre-scaled decoration variant per desktop scale, because xfwm4 scales no custom theme and would otherwise wear 1x title bars on a 2x desktop. The theme is **derived, not authored**: Debian's Greybird-dark is a complete maintained dark theme whose only problem is that it is Ubuntu blue, and hand-writing ten thousand lines of GTK3 CSS to replace it is not work this repository wants to own. `brand-theme` puts every color in it through one hue transform — near-greys tinted toward the brand's purple-black, the blue accent moved onto the brand purple, **everything else left alone**, because Greybird's red, orange and green are its error, warning and success colors and a blanket recolor erases the one distinction they exist to make. Lightness is preserved throughout, so every contrast ratio the theme was built with survives. The window decorations are pinned by hand instead, because they are the one part of another program's window that is ours to draw: a focused window is outlined in the mark's purple and an unfocused one in the line color, the same treatment the viewer gives the desktop it frames. Being a transform rather than a color list is what lets it survive Debian shipping a new Greybird. |
| `image/nix-seed` | Puts a nix store back at `/nix`, on first use (ADR 0075). The image installs nix and then moves the whole store to `/usr/local/lib/discobox/nix`, leaving `/nix` empty so the pool-shared cache volume can bind over it without hiding image content — the base layer in `sandbox-agent/image.json` declares `/nix` as `cache`, and a cache path is always a plain bind, so a populated `/nix` would simply vanish behind the volume. Run as a oneshot unit that `nix-daemon.service` requires; nix-daemon is socket-activated, so a sandbox that never touches nix never copies a byte. Socket activation needs a `nix-daemon.socket.d` drop-in to survive that empty `/nix`: both vendored units guard on `ConditionPathIsReadWrite=/nix/var/nix/daemon-socket`, so with the store moved aside the socket is skipped at boot and never created — the client's symptom is `cannot connect to socket at '/nix/var/nix/daemon-socket/socket'`, not a failed unit. The drop-in resets the condition; systemd creates the socket's parent directories itself, so binding on an empty `/nix` costs a mkdir and nothing more. Two scopes with separate guards: store and `db` go to the pool cache under a `flock` on the cache filesystem, while `profiles` and `gcroots` are per-sandbox data volumes mounted inside `/nix` and seeded per sandbox — they are keyed by username, and every sandbox in a pool shares the store — `/nix` is the one cache path that declares `"scope": "shared"` (ADR 0094) — so sharing them would let one sandbox's `nix profile install` rewrite another's default profile. Each stamp lists the seed stores merged into its scope, one id per line, appended only after that copy finishes (ADR 0085): an interrupted copy is redone rather than trusted, and a cache seeded by an older image build is re-seeded rather than believed — a boolean stamp would let a rebuilt image hand a sandbox a default profile whose generation link points into a store the pool cache never received. A cache that already holds a database takes the merge path: store paths only, then `nix-store --load-db`, because the pool's `db.sqlite` records everything its sandboxes have built since and a file copy would discard it. The build writes what only it knows beside the seed and never inside it — `/usr/local/lib/discobox/nix-seed.env` (the store's id, a hash of its own content hashes, and the `/nix` path of the `nix-store` binary it delivers) and `nix-db.dump`. The unit is a `Type=oneshot` that does not remain active, so a shim's `systemctl start` re-runs it; an up-to-date run is two file reads under the lock. In-sandbox `nix-collect-garbage` is unsupported: `temproots` are named by pid and each sandbox has its own PID namespace, and per-sandbox gcroots hide each other's live roots. |
| `image/nix-shim` | Installed as `nix`, `nix-shell`, `nix-build`, `nix-env`, `nix-store` and `devenv` on `/usr/local/bin`, it starts `discobox-nix-seed.service` and then execs the real binary. Socket activation alone cannot trigger the seed: the client binaries live in the store too, so before the first seed there is nothing to run and nothing ever reaches the daemon socket. It disarms itself — PATH puts the store's own bin directories ahead of `/usr/local/bin`, so the shim is reachable only while those are empty. Same injection point `dockercache` uses, for the same reason. |
| `image/harness-run` | The base image's `discobox-harness-run` (`harness.RunCommand`), the convention every harness image conforms to (ADR 0086 §3): a harness terminal types `discobox-harness-run [--resume] '<prompt>'` into its login shell. A harness image overwrites it; this copy runs no agent and says so, leaving the user at the shell. |
| `image/services` | The image's own service declarations, installed at `/usr/local/share/discobox/services` for `services` to read: `10-desktop.sh` declares `ai.discobox.desktop` on 6900 with `protocol: http` and `start: never`, because `discobox-desktop.socket` already serves it. |
| `image/skills` | The image's own skills. `discobox` is the skill for the sandbox itself — what a discobox is, what it can and cannot reach, how work leaves it as commits, and what the user's side looks like — and this is exactly where ADR 0080 §3 puts it: the sandbox's behavior is this module's interface, so changing it puts somebody in front of the file. It exists because an agent is dropped into an environment it has no other way to learn: told nothing, it treats `origin` as pushable, reads a sentinel as a real credential to repair, and reports "the server is running" for a port the user's window has already forwarded. `discobox-review` is the other case — a tool this image installs that no module in this repository owns, whose binary the `Dockerfile` installs from `discobox-ai/review`. Its placement **deviates from ADR 0080 §3**, which puts a skill beside the interface it documents so that changing a flag lands in front of the person editing it. That interface is in another repository, so no location here achieves it. Shipping the skill from `discobox-ai/review` would, and is the alternative this rejects: the image takes that binary with `go install <module>@version`, which delivers a binary and no other files, so one Markdown file beside it costs a second checkout of that source, separately pinned and separately bumped. The residual cost is unmitigated and belongs to whoever bumps the pin — `ARG REVIEW_VERSION` can invalidate every command in the skill with nothing in the diff pointing at this file, so **re-read the skill when bumping it**. A skill for an interface a module here owns still lives in that module, as `discobox-access`'s does in [`access/skills`](../access/DESIGN.md). Both trees are copied into `/usr/local/share/discobox/skills`, which the `terminal` package's skills install reads. Nothing mirrors this tree into `.agents/skills`, where this repository keeps the skills it wrote for itself: what these skills describe exists only inside a discobox, so the sandbox is the only thing that installs them. A checkout on a laptop does not get it, and a sandbox created before it shipped gets it when it is next recreated rather than in place. |

## Resource Reporting

The status endpoint reports this sandbox's own CPU and memory as **cumulative
counters, never rates** (ADR 0071). `resources/usage.go` reads cgroup v2 at
`/sys/fs/cgroup` — a private cgroup namespace makes that this container's own
cgroup presented as the root — and falls back to a per-process rollup over
`/proc` when it cannot, recording which in `source`.

Turning counters into "how busy" belongs to the pool agent, which polls every
sandbox in its pool on one tick and can therefore difference all of them over
the same window. Computing a rate here would give each sandbox its own slightly
different window and make the pool's ranking incomparable. It also keeps this
endpoint genuinely computed-fresh, with no sampling state of its own.

Memory is reported twice because both numbers are true and neither substitutes
for the other: `currentBytes` is what the host charges the cgroup (including
page cache and kernel memory), while `virtualBytes`/`residentBytes` are what the
processes think they hold and double-count every shared page. Summed resident
routinely exceeds `currentBytes`.

Processes are offered as a **candidate list** — the union of the top by
cumulative CPU and the top by RSS — each carrying `startTicks` alongside its
PID, because PIDs are reused and the pool agent differences per process.

## Boundary Rules

- Implement the generated in-sandbox exec, service, harness-hook, and status API subset from `api/sandboxgen`;
  canonical route and DTO definitions live in `api/openapi/server.yaml`.
- Depend on root contracts and generated API types only for cross-module data.
- Do not import server internals or provider implementation packages.
- Keep pool registration and control-plane bootstrapping in the `pool-agent`
  module unless a shared contract belongs in the root module.
- Do not call back to the pool agent or server; resolved config is injected
  into the sandbox and read locally.
- The desktop viewer's framebuffer arithmetic exists twice, in `desktop.NormalizeSize`
  and in the page's `quantize()`, and the two must round identically. The page
  sizes its frame to the framebuffer it is about to ask for, so a disagreement of
  one bucket puts the desktop inside a frame that is the wrong shape for it.
  Change one and change the other; the bounds are served to the page as
  `limits` on `/api/session` rather than duplicated in the script.
- Nothing in the desktop viewer may derive the scale from the viewport, the
  device pixel ratio, or the framebuffer size on an ongoing basis. It is settled
  once, before `xfce4-session@.service` is released, and changed afterwards only
  by restarting that session. The channels it travels on do not move together —
  `Xft/DPI` over XSETTINGS reaches running programs at once, `GDK_SCALE` is read
  when a process starts — and they are correct only in combination, so anything
  that moves one without the other puts 2× text inside 1× widgets.
- `scale.env` is written by the boot flow (`seedDesktopScale`) before systemd
  starts, and every session start applies its server half (`desktop
  prepare-session`, the session unit's `ExecStartPre`). The first is what puts
  `GDK_SCALE` in the harness's login shell: the viewer is socket-activated and
  starts only after the harness has read its environment. The second keeps a
  session that a program on `:0` brought up, with no viewer, from starting
  `GDK_SCALE=2` programs against a 96 DPI server. Removing either brings back
  the mixed-channel state above.
- `scale.env` is written once per run whatever the value does, and read back at
  startup. `AdoptScale` does the write, from `settleScale`, with no X server in
  the call; `SetScale`'s `&& d.applied` condition is what makes a display that
  was never adopted write an unchanged value too. `%HOME%` is a data volume: the
  file outlives the process while the in-memory scale resets to 1, so skipping
  either leaves a restarted sandbox running its session at one scale and its
  server at another.
- `SetScale` commits `d.scale` **with** the `scale.env` write and `d.applied`
  only after every step has succeeded. The first pairing is what keeps the file
  and the process agreeing — they are one fact, and the session reads the file
  while the framebuffer and the DPI come from `d.scale`, so a change whose X half
  fails must not leave them apart. The second is what keeps a failure repairable:
  the unchanged-value short-circuit needs `d.applied`, so committing it early
  makes every later call at that value return without doing anything.
- The desktop's density and cursor size are written to xfconf's `xsettings`
  channel, never with `xrdb`. `xfsettingsd` owns those values and writes the
  matching X resources itself, so a merge behind its back is undone at its next
  refresh. That is also the whole reason `discobox-desktop-bus.service` exists:
  xfconf is only reachable over a session bus, and the viewer and the Xfce
  session are separate units that must read the same one.
- `Gdk/WindowScalingFactor` stays at 1, and the desktop scale is delivered as
  `GDK_SCALE` instead. Raising the window scale is the normal way to make an
  Xfce desktop HiDPI and it does not work here: `xterm` reads only `Xft.dpi`, so
  the density has to be raised, and GTK and Chromium then multiply the window
  scale by that raised density and come out at 4×. `GDK_DPI_SCALE` divides that
  back out of GTK but not out of Chromium. The measured comparison is in
  [`desktop/DESIGN.md`](desktop/DESIGN.md); do not "fix" this by reaching for
  the window scale.
- The window decorations are the exception, and are selected rather than scaled.
  xfwm4 draws them from fixed-size pixmaps and scales no custom theme at all, so
  the image ships `Discobox`, `Discobox-2x` and `Discobox-3x` and
  `desktop.decorationTheme` picks one. A new scale needs a new variant:
  `MaxScale` and `brand-theme`'s `DECORATION_SCALES` are one agreement.
- Any code that reads `sandbox.json`'s `Env` to actually launch or configure
  something (an exec, a nested container's OCI spec, a systemd
  `EnvironmentFile`) must resolve `sandboxconfig.LocalSubnetsToken` via
  `nestedbridge.LocalSubnets()` first — pool-agent cannot know a sandbox's own
  directly-connected networks and leaves the token as a placeholder. Resolve it
  at the point of use, not once and cached: the nested Docker bridge and any
  user-created networks only exist after the sandbox has booted, so a value
  resolved earlier goes stale. `execs.EnvWithRuntimeDefaults`, `runcca.proxyEnv`,
  and `proxyenv.Render` are the three current call sites.
- Where home is, when nobody is named. A sandbox whose manifest carries no user
  runs as this process's identity (ADR 0025 §5) — saying nothing about *who* is
  not knowing nothing about *where*. `EnvWithRuntimeDefaults` completes home
  from that identity's own account when no layer supplies one, which is what
  expands `%HOME%` in the image's PATH, defaults `HOME`, and gives `~` and the
  harness file installer somewhere to resolve to. Every sandbox the server
  creates for itself lands here: a configure sandbox names no user, and its
  harness files install against the same home its exec starts in.
- Load the single immutable harness contract from `sandbox.json`'s image layer,
  which the control plane resolved from the image's manifest labels at
  registration (ADR 0012 §6 — there is no file inside the image). Commands,
  static files, and config-mode behavior are image-owned; the sandbox manifest
  contributes selection, mode, and a non-secret project file overlay.
- Volume wiring is declarative and image-owned. The image's manifest lists the
  paths it needs persisted (`data`) or shared across the pool's sandboxes
  (`cache`) — for every harness image that is the base layer this image
  contributes, `sandbox-agent/image.json` (ADR 0086 §2); the pool host mounts the primary volumes (`/.discobox/{data,cache,config,sources,secrets}`)
  and the `boot` init flow wires each declared path onto its backing volume —
  bind when the target is empty, overlay (lower = image content) when it ships
  content. An overlay's upperdir is given the target's own ownership and mode
  before the mount ([ADR 0107](../docs/adr/0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md)), because overlayfs reports the
  *upperdir's* attributes for the merged root: left at the `0755` root-owned
  directory that creating it produces, every overlayed path would present as
  `root:root 0755` however the image built it, and the path's top level alone
  would reject writes that everything beneath it accepts. Cache paths are always a direct shared bind, never an overlay, because
  the cache volume is shared across concurrently running sandboxes. By default it
  is shared only with the ones running as the same uid: a cache path is backed by
  `/.discobox/cache/.users/<uid>/<target>`, because everything under it is
  chowned to the sandbox user and two clients of one server disagree about uids
  whenever their local accounts do (ADR 0094). A path that declares
  `"scope": "shared"` stays at `/.discobox/cache/<target>` and is one directory
  for the whole pool; `/nix` is the only one, being root-owned and
  content-addressed. This flow picks the partition rather than the pool host,
  since the uid is resolved here and may exist only in the image (ADR 0025 §4). A
  data path is unpartitioned — it is this sandbox's alone. Sources are
  bind-mounted from `/.discobox/sources/<slug>` onto the targets named in the
  manifest. See ADR 0007.
- Homebrew ([ADR 0107](../docs/adr/0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md)) is baked at
  `/home/linuxbrew/.linuxbrew` — the only prefix its
  bottles are built for, so any other makes every formula a source build — and
  `sandbox-agent/image.json` declares that prefix a `data` volume. Because the
  image ships content there, boot wires it as an overlay: the image's tree is
  the lower layer, and a sandbox's own `brew install` persists to its data
  volume. This is the plain case of the rule above, and it is why brew needs
  none of nix's seed machinery — ADR 0075 exists because `/nix` had to be a
  pool-shared *cache* volume, and a cache path is always a plain bind that
  would hide what the image shipped. Ownership cannot follow the usual route
  either: the sandbox user's uid is not known until boot, and a recursive
  chown across an overlay copies every inode into the sandbox's volume. So the
  tree is `root:brew`, group-writable, setgid, with `brew` in
  `additionalGroups` — boot adds whatever uid the sandbox runs as to the group,
  and the build's ownership stands unchanged. Bottle *downloads* land in Homebrew's
  default cache, `~/.cache/Homebrew`, on the `%HOME%/.cache` cache volume, so
  they are shared with the pool's same-uid sandboxes rather than pool-wide
  (ADR 0094); only the Cellar is per-sandbox. `HOMEBREW_NO_AUTO_UPDATE` is set for the same overlay reason:
  brew's pre-install `git pull` of its own repository would copy the repository
  up, so `brew update` is not how formula data refreshes here — the JSON API is.
  A root sandbox (a manifest that named no user, ADR 0025 §5) gets a working
  brew rather than a refusal: Homebrew's root guard stands down inside a
  container, and every sandbox is one, so root simply installs root-owned kegs
  and the group arrangement is moot for it. Brew sits **after** `/usr/local/bin`
  on PATH, not before: that is where the image's required shims live, and
  `brew install docker` would otherwise replace the ADR 0044 `docker` shim for
  good, leaving a nested `docker build` to succeed against the wrong daemon.
  The hazard is narrowed rather than closed — `~/.npm-global/bin`, `~/.local/bin`,
  `~/.cargo/bin` and the nix profiles all still precede `/usr/local/bin`, so a `docker` CLI
  installed through any user prefix disarms ADR 0044 the same way.
- A source's origin, when it has one, arrives at `/.discobox/origins/<slug>` as a
  plain read-only bind the pool host already made directly onto that final path
  before the container started — unlike `sources`, `boot` does not rebind it from
  anywhere; it is simply present by the time `boot` runs. Behind it is the
  developer's live directory for a clone-delivered source (ADR 0026) or the
  repository the client pushes into for a push-delivered one (ADR 0058); either
  way it is the repository `origin` names. See ADR 0026.
- Render templated harness files locally at installation time against the public
  `SandboxConfig` object from the manifest, plus two keys the installer adds:
  `secrets` (env name -> sentinel) and `workingDir`. Keep API field names as the
  template surface and expose only deterministic, non-secret formatting helpers.
- `workingDir` is the directory this sandbox's terminals start in, resolved by
  the exec layer that starts them (`execs.Manager.DefaultWorkdir`): the primary
  source's target when there is one, the working root when there is not. It is
  what a harness that gates work on directory trust is told to trust. It is not
  derived from `sources`: a source-less sandbox — `discobox run` with nothing to
  clone, and every configure sandbox — would then trust nothing, and its harness
  would open on a trust prompt for the directory it is already sitting in.
- A repository's `.discobox/skills` is copied, never reconciled. It is installed
  once — on the primary terminal's first launch — and from then on the copies are
  the harness's files: it prunes, renames, and rewrites them, and restoring them
  underneath it would undo that. Nothing re-reads the directory afterwards, so a
  skill added to the repository later reaches a new sandbox, not this one.
  See ADR 0072.
- Treat systemd as the source of truth for terminal unit liveness. Runtime JSON
  files identify known terminals; reconciliation joins those files with systemd
  status and shim status. Durable database-only exec records must be reconciled
  before they are returned: restore manager-owned runtime/socket paths, and do
  not preserve stale `starting` or `running` state when the unit is gone. "Gone"
  means unloaded, not inactive: `systemctl show` succeeds for a unit systemd
  never heard of and calls it inactive, so `UnitStatus.Loaded` — not a status
  error — is what demotes a vanished exec to `lost`. For a terminal, `exited`/
  `failed`/`lost` means "not running, revivable" rather than gone: its exec id
  is a durable identity, and attach/start relaunches it in place (ADR 0038).
- Resolve an exec's workdir after its run user and env, never before: an empty
  request takes the sandbox's configured default (the primary source
  directory), a relative path joins the working root, and a leading `~`/`~/`
  expands against the run user's home directory (`execs.HomeDir`, shared with
  the terminal layer so the two cannot disagree). `~` is how a caller outside
  the sandbox — the SSH ingress, whose sessions must start where a login shell
  would — names a path only the sandbox can resolve. An unresolvable `~` is an
  error, not a fallback to the default: silently starting somewhere else is
  what puts `scp` uploads in the source tree.
- Keep terminal and exec history local. The SQLite store records append-only
  lifecycle events, latest observed runtime state, and retained opaque resource
  samples, but REST runtime state should be derived from runtime/systemd/shim
  observations instead of an in-memory cache.
- The agent credentials endpoint is loopback-only and carries no token, for the
  same reason the hook socket has none: everything inside the sandbox is equally
  untrusted, so a secret shared between in-sandbox processes authenticates
  nothing. Authority comes from the mTLS client certificate one hop out, which
  no in-sandbox process can forge without already having root here — at which
  point it has the listener too. A sandbox with no staged proxy material simply
  does not bring the endpoint up; that is not a startup failure.
- `discobox-access run` refuses to execute a command until a model has
  agreed the command is the use it was approved for, asking through the harness
  image's `discobox-prompt` (ADR 0079). The gate runs in the sandbox, so it is a
  guardrail on the honest path rather than a boundary; the pool agent's
  activation check and the control plane's grant are still what enforce.
- A value taken through the credential CLI is never written to disk or exported
  into a shell. `discobox-access run --use ID -- cmd` injects it into that
  one child process's environment, replacing rather than joining any same-named
  variable, so a stale export cannot shadow the fresh value.
- The CLI is a real binary, not an `argv[0]` alias of `discobox-sandbox-agent`
  the way `discobox-hook-publish` is. It is the client of a portable protocol
  and is meant to leave for its own repository, so it must not be welded to the
  runtime that serves it: it depends only on the stdlib-only `agentcreds`
  package. `execs.EnvWithRuntimeDefaults` advertises the endpoint through
  `agentcreds.URLEnv` so a harness can find it without the address being
  compiled into anything but a default.
- The status endpoint (`GET .../status`, `status:read` scope) is answered
  fresh on every request from the authenticated caller — pool-agent's standing
  poll loop, per ADR 0030 — with one exception, `ports`. Sandbox-agent never
  pushes status anywhere on its own initiative, consistent with the boundary
  rule above.
- `ports` is that exception, and the reason is specific to it
  ([ADR 0046](../docs/adr/0046-listening-ports-are-polled-and-probed-in-the-background.md)):
  what a listening port speaks can only be learned by connecting to a user's
  process and writing a request at it, and the answer does not change while
  that socket lives. So `ports.Watcher` scans and probes on its own interval,
  caches each result against the socket inodes behind the port, and the handler
  reports its snapshot. Discovery is a `/proc/net/tcp{,6}` read filtered by the
  uid `execs.Manager.ResolveUser` returns — never a uid derived some other way,
  and never the manifest layer unresolved (see [REVIEW.md](REVIEW.md)) — so a
  port belongs to the sandbox's own processes by construction rather than by a
  guess about which ports are "user" ports. A new component wanting the same
  exemption needs the same argument; this is not a precedent for caching status
  in general.
- **The uid filter is not widened; a declaration is what covers what it cannot
  see** ([ADR 0076](../docs/adr/0076-a-service-may-declare-a-port-discovery-cannot-see.md)).
  A port published by a nested container or bound by a socket-activated unit is
  root's socket, so the filter excludes it however plainly the sandbox's own
  work is behind it — and dropping the filter would report sshd and every image
  daemon beside it. A service declaring `ports:` is the missing fact, because it
  is intent rather than an observation. Declared ports are folded into the same
  state map, probed like any other (connecting does not care who owns the far
  end), and marked `declared`; `addresses` stays what the scan actually saw, so
  the record never claims the sandbox user is listening somewhere it is not. A
  declared port is listed while it is declared, not while its service runs: the
  script that publishes one has usually exited by the time the port matters.
- A **service is the second typed layer over `execs`**, and the rules that make
  it one are worth stating separately from the terminal's. It is declared in the
  repository, not in the manifest: nothing about pool-agent, `sandbox.json`, or
  sandbox creation knows services exist, and the sandbox discovers them from
  inside itself once it is running. Its exec id is a durable identity the way a
  terminal's is (ADR 0038), so `Restart` is `execs.Relaunch` plus `Start` under
  the same id and a client keyed on it keeps its place. Nothing supervises it:
  a service that exits is reported with its exit status and left alone
  (ADR 0070 §4), so there is no desired state to persist and no reconcile loop
  to run. And it runs on **pipes, never a PTY** — a service's output is read
  after the fact, and `frame.Stdout`/`frame.Stderr` stay distinct all the way to
  `discobox admin services logs`.
- `execs.Manager.Stop` is not `Delete`. Stop ends the run, removes the shim's
  socket so an attach reports the session gone rather than dialing a dead one,
  and marks the record `stopped`; Delete also discards the record and the
  transcript. The `stopped` flag is written at the one place a stop is
  requested because it cannot be inferred afterwards — a stopped process and one
  killed by a signal it did not choose leave the same record — and it is
  persisted (`ExecState.Stopped`) because the runtime file carrying it is on
  tmpfs, so after a reboot the record is all there is. Without it the reconcile
  loop finds the unit gone and calls the exec `lost`, which is true of a unit
  that vanished underneath a live exec and wrong for one that was asked to stop.
- A terminal is one primitive: an exec created in harness mode. The `terminal`
  layer resolves the image harness (or the `shell` fallback harness — a login
  shell — when the image has no harness, or when the manifest declares a
  harness with no command, which is the control plane's way of naming that
  same shell without knowing which shell the run user has: ADR 0032), applies
  image/project files and hooks,
  injects the hook/terminal env, then calls `execs.Manager` with `TTY`,
  `harnessId`/`primary` metadata, `Shell: true`, and — for every harness except
  the `shell` fallback (which already is the shell) — `StartupCommand` set to the
  resolved harness command, typed in once the shell's line editor has taken the
  terminal (`procio.WaitForLineEditor`). Written the instant the process starts,
  those bytes reach a PTY whose ECHO is still on: the kernel echoes them and the
  editor then displays the same line again when it reads them, so the command
  lands on screen twice, once above the prompt and once on it. It is a poll
  because Linux offers nothing to wait on — a PTY in packet mode is documented
  to report slave state changes as `TIOCPKT_IOCTL`, but that bit is BSD's and is
  not implemented here, verified against a real shell. A shell was measured
  taking ~10ms to prep, about ten checks; the wait is capped, and a program that
  never takes the terminal is written to anyway. `execs.Manager` never learns what a harness is;
  `StartupCommand` is a generic exec-primitive capability, not a harness concept.
  One `execs.Manager` runtime backs both plain execs and terminals.
- A **configure sandbox is the exception**: its command is the exec itself
  (`Shell: false`), not a job typed into a login shell. What the flow is for is
  the command's exit status — the server reads it to decide whether the setup
  worked and whether to apply what it wrote — and a command typed into a shell
  has none the exec can report: the shell outlives it, so the terminal never
  reaches `exited` until somebody types `exit`, and the code reported then is
  the shell's. Job control is worth a login shell for a harness you sit in front
  of; it is not worth the answer to "did this succeed" for a program that runs
  once and ends. A revived configure terminal types nothing in either — a
  relaunch re-runs the exec's own command, which is already the setup.
- A terminal's exec id is its durable identity (ADR 0038). Attaching to or
  starting an ended terminal — its own id or the virtual `primary` alias —
  revives it in place: `terminal.Service.Revive` re-resolves env/secrets and
  the harness relaunch command (never the initial prompt), re-ensures hooks and
  files, and calls the generic `execs.Manager.Relaunch`, which fences the old
  run and starts a fresh transient unit generation (`discobox-exec-<id>-g<N>`)
  under the same exec id, socket, and runtime paths. Exec fields describe the
  current run; per-run history stays in the append-only event log and
  transcript store. Plain execs are never revived. `EnsurePrimary` revives the
  newest dead primary record on later boots instead of creating a sibling, so
  the session list holds one entry per terminal identity.
- A harness terminal never execs the harness binary directly. `execs.Manager`
  resolves `Shell: true` to the run user's login shell (as for a plain `shell:
  true` exec) and reports that shell as the exec's `Command` — what is literally
  executed. `StartupCommand`, when set, is a second argv the shim types into that
  shell's PTY once it starts, quoted with `execs.QuoteShellCommand` and followed
  by a newline, exactly as if the user had typed it at the prompt: the harness
  therefore runs as the shell's foreground job, not as the exec's own process.
  The bytes need no handshake to arrive — the PTY's cooked-mode input queue
  holds them until the shell's own first read, regardless of profile/rc timing
  — and the bounded line-editor wait above exists only so they are not echoed
  twice. This is why Ctrl-Z can suspend a harness at all: see the orphaned
  process group rule below. `SandboxExec.command` and `.startupCommand` report
  the two argvs separately; CLI/API display prefers `startupCommand` when set.

## Builds Leave the Sandbox

`docker build` runs on the pool's shared BuildKit, not this sandbox's dockerd
([ADR 0044](../docs/adr/0044-builds-run-on-a-pool-shared-buildkit.md); the pool
side is in [pool-agent/DESIGN.md](../pool-agent/DESIGN.md)). The pieces here:

- `dockercache` rewrites `docker build` onto a buildx `remote` instance and
  brings the result back through the pool registry. It pushes to a name
  synthesized per build, pulls it back, and applies the user's tags locally.
  Attestations are disabled unless asked for: buildx attests provenance
  whenever the output is a registry, and that provenance describes the build
  rather than its result, so identical content would otherwise get a different
  image ID in every sandbox. A tagged build's synthesized name is removed
  however the build ends; an untagged one keeps it, because removing an image's
  last reference deletes the image and there is no way to untag a pulled image
  into the `<none>:<none>` entry a local build leaves.
- Local base images are published and redirected. The pool builder has no
  access to this daemon's image store, so `FROM discobox-sandbox-agent:local`
  normalises to docker.io/library and goes to Hub — the one way a pool-shared
  build visibly is not a local one. The shim scans the build's `FROM`
  instructions (`localbase.go`), and for each reference that names no registry
  and exists locally, pushes it into this sandbox's registry namespace and adds
  `--build-context <ref>=docker-image://<that>`, which BuildKit's frontend
  resolves before it resolves an image. The push is metadata for anything the
  pool built, since the registry already holds its layers. It is here rather
  than in the mediator because the mediator cannot see a build's sources
  (ADR 0044); it degrades to leaving the build untouched at every step. See
  ADR 0047.
- Tagging retries. `docker tag` is not atomic in the containerd image store —
  the daemon creates the record, and finding the name taken, deletes it and
  creates it again — so two builds tagging one name with different targets can
  interleave and leave the loser with `AlreadyExists`. Bringing the result back
  through the registry is what exposes this: a local `docker build -t` names
  the image as part of the build, while this shim tags it afterwards. `task
  dev`'s image watcher rebuilding a `:local` image alongside a hand-run build
  is the case that hits it.
- `discobox-buildkit-bridge.service` is a second `proxy/bridge` instance —
  `127.0.0.1:17082` to the pool mediator over mTLS — for the same reason the
  proxy bridge exists at all: buildx would otherwise need the sandbox's client
  certificate, and the sandbox user cannot read a key only root should hold.
- `daemon.json` marks the pool registry insecure, because it speaks plaintext
  HTTP over a network with no route off-box.

The sandbox's own dockerd still runs builds for anything not rewritten — a
direct `docker buildx` — so `runcca`'s handling of BuildKit's `runc run` path
below remains load-bearing.

## Trust Store on the Boot Path

`discobox-trust-ca.service` is ordered `Before=discobox-sandbox-agent.service`,
so nothing about a sandbox exists — no agent, no primary terminal, nothing to
attach to — until it finishes. It is therefore held to a different standard than
the rest of the boot: it must be proportional to the CAs it is actually adding.

It does not run `update-ca-certificates`. That tool rebuilds the whole store at
a flat ~0.8s whether it is adding one certificate or 150, measured at 1.7s of a
~4.2s wait for an attachable terminal. Instead:

- The image ships the finished system store at **`/opt/discobox/ca-certificates`**,
  built in the Dockerfile by a copy of `update-ca-certificates` with
  `ETCCERTSDIR` redirected there.
- At boot `discobox-ca-anchor -store /etc/ssl/certs` seeds what is missing from
  it, appends every anchor to the bundle, and hashes only the anchors
  (`runcca.MaterializeTrustStore`). ~85ms.

Two constraints on any change here:

- **The prebuilt store cannot live at `/etc/ssl/certs`.** `runcca` bind-mounts a
  staging directory over that path in every container this sandbox's dockerd
  starts, *including BuildKit's*, so a build running inside a sandbox writes
  there into the mount and loses it at layer commit. That is why anything
  built for the store goes under `/opt`.
- **An existing bundle is never replaced.** A nested sandbox boots with one its
  host's wrapper already placed, carrying the host's CA; overwriting it with the
  image's cuts off the egress path the outer proxy owns. Anchors are appended to
  whatever is there.

Subject hashes come from `openssl x509 -hash`, not from Go. The value is a
digest over a canonicalized subject encoding, and getting it subtly wrong would
misplace a link on the path that decides what the sandbox trusts, to save one
10ms exec.

## Development Images

`task build` is the no-argument build entry point for binaries and all local
images. `task build:images` builds the shared base, pool host, sandbox base, and
included harness images.

`task dev` starts `internal/cmd/discobox-docker-image-watch`, which initially builds the
shared base, pool, base sandbox, Codex, Claude Code, and Shell images. Each harness
Dockerfile extends `discobox-sandbox-agent:local` through its
`SANDBOX_AGENT_IMAGE` argument. The watcher tracks shared Docker/runtime inputs
plus this folder's own `image.json` (the base manifest layer) and each harness
folder's Dockerfile, `image.json` where it has one, and configure script.
Harness-specific changes rebuild only that harness image; shared changes rebuild
the base and all affected harness images. Every successful build writes its
content-addressed development image reference to `.env`; pool and sandbox base
images also write their image digest. The watcher atomically writes the complete
reference-to-image-ID set as `.tmp/discobox-dev-images.json` and enables
development image synchronization in `.env`. On restart the server converges
that manifest onto every Docker daemon before reconciling its pool-agent
container, so local-VM, cloud, exec, and host-Docker providers use the same
development images without a registry.

## Runtime Rules

- Every sandbox has a default terminal: on sandbox start the harness always
  launches exactly one primary terminal (`terminal.Service.EnsurePrimary`), so
  clients such as `discobox run` can rely on one existing and attach to it. The
  first start runs the resolved harness with the manifest prompt as arguments;
  later starts run the harness's `relaunchCommand` to resume the previous session
  instead of replaying the prompt. First-vs-subsequent is decided by a durable
  marker in the SQLite store (`AgentState`), so it survives restarts. When no
  harness is configured the primary terminal is a login shell (harness id `shell`)
  and the prompt is not passed, since a shell would run it as a command. The
  launched exec is tagged `primary` in metadata by the sandbox-agent; that tag
  cannot be requested through the terminal create API.
- Bringing a terminal up is single-flighted, keyed by what is being brought up
  (`Service.singleFlightLaunch`). Boot launches the primary from a goroutine
  started just before the HTTP server serves, and clients attach without first
  polling for a terminal (ADR 0039), so boot and a first attach overlap by
  construction. Both a first launch and a revive are check-then-act over records
  nothing else serializes — `execs.Manager` keeps no in-process lock and `List`
  re-reads from disk — so concurrent callers otherwise both act:
    - A duplicated **first launch** gives the sandbox two primary terminals, and
      both callers read the durable launched-marker before either writes it, so
      the prompt runs twice.
    - A duplicated **revive** is worse than wasted work. Both callers derive the
      next unit generation from the same stale record, so they land on the same
      unit name, and the second one's socket removal — which exists to fence the
      *previous* run — deletes the socket the first one's shim has just bound,
      leaving a live run nothing can attach to.
  The primary launch is keyed by the virtual `"primary"` id, which is never a
  real exec id; a revive is keyed by the terminal's own exec id, which is its
  durable identity (ADR 0038). A primary launch that decides to revive uses the
  record's id, so an attach addressing that terminal directly contends on the
  same key rather than reviving it a second time.
- Joining a launch is also the readiness wait: it completes only after install
  and start, so `ResolvePrimary` never hands back a terminal whose shim is not
  listening yet. The whole decision runs under the latch, not just the launch —
  a record exists in `starting` from the moment `execs.Create` writes it, so a
  liveness check outside the latch would return a terminal nothing can attach to
  yet. Each launch runs under a context detached from whichever caller started
  it — an attach that times out must not abort an install that boot and other
  joiners are waiting on — while each joiner waits under its own context,
  bounded by `terminalReadyTimeout`. A failed launch is reported to everyone
  joined to it and clears the key, so the next attach retries.
- `"primary"` (`terminal.PrimaryExecID`) is a virtual exec id accepted anywhere
  the exec API takes one. It always names the sandbox's current primary
  terminal; attach and start resolve it through `terminal.Service.ResolvePrimary`, which
  relaunches a stopped primary and returns the terminal that launch produced
  rather than re-scanning, while reads (get, logs, events, delete) resolve
  it read-only so a client's done-check observes a real exit instead of
  triggering a resume. A real exec id never relaunches: an id names one session,
  and once the shim behind it is gone the attach fails with `execs.ErrSessionGone`
  → `409`, whose message reports the exit status and points at `"primary"` when
  the dead exec was the primary terminal. The control plane proxies exec ids
  opaquely, so clients just send this value.
- Git authorship is seeded per key, never overwritten. `boot.seedGitConfig`
  applies `sandbox.json`'s `git` object to `<home>/.gitconfig` after `seedHome`
  — after, because `seedHome` chowns the tree recursively, and because git's
  lock-and-rename leaves a rewritten file owned by boot. It sets `user.name` and
  `user.email` independently, and only where `git config --get` resolves nothing;
  the unit is the key, not the file, since a `.gitconfig` holding only aliases
  has no identity and is exactly the case that needs seeding. The read is not
  scoped to `--global`, so an identity the image shipped in `/etc/gitconfig`
  counts as an answer. Git is asked rather than the file parsed — the same rule
  the CLI follows reading the identity on the way in — and an image with no `git`
  skips the step rather than failing to boot. Changing your local git identity
  therefore does not propagate into existing sandboxes; see
  [ADR 0042](../docs/adr/0042-git-authorship-identity-is-a-first-class-sandbox-property.md)
  for the condition for revisiting that.
- A source tree's `.envrc` loads without anyone typing `direnv allow`, in two
  halves. The hook is static, so it ships in the image:
  `/etc/profile.d/sandbox-direnv.sh` installs `direnv hook bash` in an
  interactive shell and runs `direnv export bash` once in a shell with no
  prompt — a service is `bash -lc <script>`, which never reaches
  `PROMPT_COMMAND` and would otherwise see none of its repository's
  environment — and `/etc/bash.bashrc` sources the same file for the
  interactive shells `/etc/profile` does not reach. The trust is per-sandbox, so
  `boot.seedDirenvConfig` writes it: `<home>/.config/direnv/direnv.toml`
  whitelisting every manifest source target, after `seedHome` for the reason
  `seedGitConfig` is. It has to be the whitelist rather than an `allow`, because
  an `allow` is a hash of the `.envrc`'s path *and* contents — unwritable before
  the checkout exists and revoked by the first edit to the file — while a prefix
  is a property of the directory and also covers an `.envrc` below the root.
  Written only when absent: direnv reads exactly one config, so a prefix
  somebody adds by hand has nowhere else to live.
- Run identity is owned by [`runuser`](runuser/DESIGN.md): one call resolves who
  a process runs as, so nothing re-derives it. `execs.User` is that package's
  type. `execs.Manager.ResolveUser` is the entry point for execs and terminals —
  it applies the request-vs-manifest and group rules, then resolves — and
  `terminal` asks it rather than rebuilding the user from `ExecDefaults`. See
  [REVIEW.md](REVIEW.md) for the mistakes this prevents and
  [ADR 0025](../docs/adr/0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md)
  for why.
- Which shell a user has is sandbox knowledge, so an exec request asks for one
  (`shell: true`) instead of naming it. `execs.ResolveShell` answers from the run
  user's passwd entry — the current process user when the exec inherits the
  agent's identity — falling back to `$SHELL` from the exec environment and then
  a `/bin/bash`, `/bin/sh` probe when the entry is missing (a bare UID) or names
  a login-refusing shell. `execs.ShellCommand` wraps it as a login shell argv,
  and the `shell` fallback harness uses the same call, so a shell terminal and a
  `shell: true` exec can never resolve differently. The resolved argv is what the
  exec record reports, so a shell exec is self-describing after the fact.
  `shell` is mutually exclusive with `command` and `harnessId`: an empty command
  still means "run the harness" for every caller that did not ask for a shell.
  `CreateRequest.ShellCommandLine` (API field `shellCommandLine`) is the one
  exception to "shell means an interactive login shell": set alongside
  `shell`, it runs the resolved shell with `-lc <ShellCommandLine>` instead.
  It exists for ADR 0024's SSH ingress — SSH's `exec "cmd"` channel type
  carries one opaque command-line string, and sshd, running outside the
  sandbox, cannot resolve a login shell path itself the way a `shell: true`
  request already lets a caller avoid naming one.
- There is one shim (`execs/shim.go`) and one framed attach mechanism. Attacher
  tracking, frame writes, output broadcast, exit frame emission, and pending
  resize state belong to `execstream/host` in the root module; keep Unix socket
  setup, the HTTP upgrade, and everything touching the PTY in `shimruntime`; keep
  process startup, status persistence, stream logging, and stdin-close behavior
  in `execs`. Before publishing terminal status or an exit frame, drain the
  PTY/pipes and flush the asynchronous log queue so status means all command
  output is available. The exec shim serves both TTY (terminal, `exec -t`) and
  stdout/stderr-pipe (plain exec) modes.
- stdout and stderr are separate frames, never merged by the shim: `frame.Stdout`
  and `frame.Stderr` (and the matching `LogStream` values on the audit log), so a
  client can route each the way a local command does — `discobox shell cmd
  2>/dev/null` drops only stderr. Merging is the client's to do and loses no
  information; merging in the shim is irreversible. A TTY exec has nothing to
  split, since the kernel merges both onto the PTY before the shim reads them, so
  it emits `frame.Stdout` only and simply never uses `frame.Stderr`. Nothing on
  the wire distinguishes that from a pipe exec that wrote nothing to stderr, and
  nothing should. Only `frame.Stdout` is screen state.
- Frame types take the file descriptor numbers they carry — `Input` 0, `Stdout`
  1, `Stderr` 2 — with control frames after them (`Resize` 3 through `Repaint` 14, including the
  `execstream/resume` session frames). The
  wire format and its types live in the root module's `execstream/frame`, shared
  with the CLI, so the two ends of a stream cannot disagree about it. See
  [ADR 0008](../docs/adr/0008-attach-stream-packages.md).
- A pipe exec's output pipes are created by `procio` (`os.Pipe`), never by
  `cmd.StdoutPipe`/`StderrPipe`. `cmd.Wait` closes the pipes it made as soon as
  the process exits, and the owner waits in a goroutine alongside the readers, so
  those pipes race the readers and silently discard a fast command's entire
  output. `procio` also closes its copies of the write ends right after `Start`,
  so the readers see EOF at exit.
- Signal frames act on the exec's process group (`kill(-pgid)`) via
  `procio.Process.Signal`, which is its own session because every exec starts
  with `Setsid`. That also means the group is
  permanently *orphaned* — no member has a parent in the same session — and the
  kernel discards SIGTSTP, SIGTTIN, and SIGTTOU sent to an orphaned group. A
  `TSTP` frame therefore maps to **SIGSTOP**, which is never discarded; mapping
  it to SIGTSTP silently does nothing. Ctrl-Z typed into a TTY exec is unaffected
  by this rule when it is a byte, not a frame, and there is a shell in front of
  the command: the remote line discipline signals the foreground job, whose
  group has a parent (the shell) in the same session and so is not orphaned.
  This is exactly what a harness terminal's `Shell: true` + `StartupCommand` buys
  it (see above) — the harness is never the exec's session leader, so its
  process group is never orphaned in the first place. A command that *is* the
  session leader (a plain exec, `discobox shell -t sleep 30`) cannot be stopped by
  Ctrl-Z for the same orphan rule — `ssh host sleep 30` behaves identically —
  which is why terminals do not take that path.
- Exit status uses the shell convention for signal deaths: `128+signum`, so an
  interrupted command reports 130 rather than Go's `ExitCode() == -1`, which
  loses the signal and reads as a generic failure. `procio.Status` carries it.
- An attacher joins the broadcast set before the `101` response is written, not
  after: a client that sees `101` may start the process immediately, and output
  broadcast before registration is lost. That ordering is structural rather
  than a convention — `host.Attach` registers, then invokes
  `AttachOptions.Ready`, which is where `HandleAttach` writes its `101`, so a
  caller cannot reorder the two. The attacher registers buffering, so live
  frames cannot race the handshake bytes onto the wire.
- Terminal attach supports `?replay=true`, which repaints the current screen
  before live output so a client that connects after a program has been running
  sees its state, not just output produced from the attach onward. The repaint
  is a snapshot of an in-memory terminal emulator (`shimruntime.screenBuffer`, reached through `host.Replayer`,
  backed by `charmbracelet/x/vt`), not the raw transcript: the emulator is fed
  every output chunk in `Broadcast`, and a snapshot serializes the current
  screen, capped scrollback (`DefaultScrollbackLines`), the cursor position, the
  input/rendering modes a TUI set before the client connected (mouse, bracketed
  paste, cursor keys, cursor visibility — tracked by scanning the output stream,
  since the emulator does not expose them), and the window title (held from the
  emulator's OSC callback, since it has no accessor either, and replayed as
  `OSC 1`/`OSC 2` — a title that was never set writes nothing rather than an
  empty one, which would clear whatever the client's own terminal had). Only TTY execs have a
  screen: `Runtime.EnableScreen` installs the `Replayer` once the PTY exists, so a
  pipe exec never waits on a repaint handshake it cannot satisfy. The
  durable transcript (`AsyncLogger`) is not used for attach — it backs only
  the `terminal logs` command (full forensic transcript). It batches an
  exec's output into compressed rows in the sandbox-local sqlite store
  (`store.ExecLogChunk`) rather than tmpfs files, so transcripts survive a
  pool container restart; see ADR 0028 for why and the read-staleness
  tradeoff that follows from batching.
- The emulator's title callback also records when the title last changed to a
  *different* value, reported on the shim's `/status` as `titleChangedAt` and
  cleared with the run like `lastAccessedAt`. Re-sending the title a program
  already has is not a change — shells and harnesses do it on every redraw —
  and a still title is what the idle stop reads as a program that is not busy.
- Race-free snapshot: `host.Stream.Broadcast` feeds the `Replayer` and snapshots
  the attacher set under one lock, and registration captures the `Replayer`
  snapshot under that same lock. Every output chunk
  therefore falls on exactly one side of the attach: already absorbed into the
  snapshot, or buffered as a live frame from registration onward and flushed
  after the snapshot — so nothing is lost or duplicated. The shim withholds the
  snapshot until the client sends a `frame.Ready` (the CLI sends it once its
  output reader is running): writing during the HTTP upgrade handshake risks
  losing the leading bytes at an intermediate proxy hop that buffers them before
  its tunnel is wired up. `frame.Ready` proves the tunnel is established end to
  end; a bounded timeout still repaints (best effort) for clients that never
  send it.
- Reconnecting terminal clients establish an opaque logical session through
  `execstream/resume`. Input, signal, and close-input actions are positioned and
  acknowledged only after the shim applies them. The host retains the highest
  applied position for the process lifetime, so retransmission after a lost
  acknowledgement is deduplicated rather than applied twice. Ready remains
  connection-local and resize remains coalesced idempotent state. Repaint is
  neither: it is unpositioned like both, and retained like neither, because a
  reconnect replays on its own and a repaint held across one would land behind
  the repaint that reconnect already did.
- Terminal-query answering: the screen emulator responds to queries in the
  output stream (DA1, DSR, DECRQM, ...) by writing answers to an unbuffered
  internal pipe. `Runtime.pumpScreenResponses` must always drain that pipe —
  an undrained pipe blocks `Broadcast` inside the runtime mutex and deadlocks
  the whole shim (Claude Code emits DA1 right after its first paint). While no
  client is attached the answers are fed to the PTY so a headless TUI blocked
  on a startup query comes up; while a client is attached they are dropped,
  because the client's real terminal sees the query in the raw stream and
  answers it.
- The screen fails open, never closed: every emulator call runs under
  `Runtime.runScreenLocked`, which recovers a panic by dropping the screen —
  repaint-on-attach degrades to plain live streaming instead of the emulator
  bug killing the exec. The PTY handle outlives the screen for this reason.
- The program's repaint is authoritative: after a replay (snapshot present or
  not), `Runtime.AfterReplay` jiggles the PTY one row smaller and back,
  so SIGWINCH makes the program redraw itself and the client converges to the
  program's real screen even when the snapshot was imperfect or missing.
- A client can ask for that same repaint mid-attach (`frame.Repaint`,
  `host.Stream.repaint`): the snapshot, ahead of the live frames buffered behind
  it, then the redraw jiggle — the replay half of an attach, at a moment the
  client chooses. It is answered in `readFrames` rather than through `OnFrame`
  because the snapshot goes to the attacher that asked and to nobody else. Only
  the snapshot: the re-sent size retimes the shared PTY, and `AfterReplay`'s
  jiggle makes the program redraw at it, which is output every attacher
  receives — the client that was laid out correctly becomes the one that is not.
  That is inherent to one PTY with one size, where the last client to name it
  wins, as it did before this frame existed; the frame decides only who asks and
  who is sent a screen. It exists for the size, which is the one piece of
  terminal state a fan-out stream cannot give every client at once: the PTY is
  whatever size the last client to send one asked for, so the others are drawing
  a layout for a window they do not have and cannot tell. The client re-sends
  its size and asks for this; the resize is what makes the program lay out
  again, and the repaint is what makes the screen arrive without waiting for the
  program to produce output. A pipe exec has no screen and ignores the frame.
- The PTY handle is the runtime's while the process holds it, and the size
  ioctls run under `Runtime.mu` for that reason. Asking a `*os.File` for its
  descriptor is not safe against the close that ends the exec, and a resize or a
  repaint can arrive in the moment the process is exiting, so `shimRuntime.close`
  calls `Runtime.ReleaseTTY` before `procio.Process.Close`: afterwards there is
  no terminal to resize, which is the truth about an exec that is ending. Reads
  and writes on the handle need no such care — those are refcounted against
  close; only the descriptor is not.
- No phantom deadlines on attach: `http.Server` per-request read/write
  deadlines survive hijacks and websocket accepts, so long-lived attach
  streams must not inherit them. The shim and `shimproxy.AttachHTTPUpgrade`
  clear conn deadlines after hijacking, the harness HTTP servers set no
  `ReadTimeout`/`WriteTimeout` (only `ReadHeaderTimeout`/`IdleTimeout`), and
  both websocket ends of an attach (CLI dial, `shimproxy.AttachWebSocket`)
  run keepalive ping loops that close the tunnel when the peer stops
  answering.
