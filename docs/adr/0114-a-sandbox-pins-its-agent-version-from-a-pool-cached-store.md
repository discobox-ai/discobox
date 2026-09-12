# 0114 — A sandbox pins its agent version from a pool-cached store

- **Status**: Accepted
- **Date**: 2026-09-12

## Context

A harness image installs the agent it ships with npm —
`@anthropic-ai/claude-code` in `harness/claude-code/Dockerfile`, `@openai/codex`
in `harness/codex-cli/Dockerfile`. Both packages are a thin JS wrapper over a
per-platform native binary pulled through `optionalDependencies`, and the binary
is not small:

| package | download | unpacked |
| --- | --- | --- |
| `@anthropic-ai/claude-code-linux-x64` | 94 MiB | 210 MiB |
| `@openai/codex` (linux-x64) | 124 MiB | 323 MiB |

Claude Code ships most days, so the version an image was built with is behind
within the week, and the agent upgrades itself. Codex does not: it compares its
version against a release endpoint and *prints* the command, stating outright
that "PATH helpers are not executed". Both check on startup, and both land their
result somewhere under `$HOME`, which is a per-sandbox `data` volume
([0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md) §1). So
a user who runs several sandboxes a day downloads the same release several times
a day.

Where Claude Code's upgrade lands is worth stating exactly, because it is not
where the image suggests. The updater asks `npm -g config get prefix` — which
the base layer points at `%HOME%/.npm-global` — and checks `W_OK` on the answer.
**Nothing creates that directory.** The check fails on `ENOENT`, the npm-global
branch is abandoned, and the updater falls back to an `npm install` into
`~/.claude/local`. Today's upgrades land in a directory chosen by a failed
permission check.

Caching the bytes is not by itself the answer, because the behaviour wanted here
has three parts and they constrain each other:

1. **A sandbox keeps its agent version across restarts.** Stop and start a box
   and it is the same box, running what it was running.
2. **A new sandbox starts on the newest version the pool has already
   downloaded**, not on whatever the image was built with months ago.
3. **No sandbox asks the network for a version at startup.** Not per start, not
   per box.

(1) is what rules out the obvious design — declaring `~/.npm-global` a shared
cache path. npm's global tree is *unversioned*: one directory per package, last
install wins. Two sandboxes linked into it are two sandboxes running whatever
the most recent one installed, which is precisely a box that does not keep its
version across a restart. (3) rules out leaving either agent's own updater on.

The cache mechanism itself is already there and needs nothing new: a declared
`cache` path is backed by the pool cache, partitioned by the sandbox user's uid
([0094](0094-the-pool-cache-is-partitioned-by-the-sandbox-users-uid.md)). What
is missing is a *versioned* store in it, and an owner for the question "when
does a new version appear".

## Decision

### 1. The store is versioned, pool-cached, and built by rename

`sandbox-agent/image.json` declares one cache path:

```jsonc
{ "path": "%HOME%/.local/share/discobox/agents", "volume": "cache", "uid": "%UID%", "gid": "%GID%", "mode": "0755" }
```

Below it, `<package-slug>/<version>/` is a complete npm prefix, produced with
`npm install -g --prefix <tmp> <package>@<version>` and moved into place with a
single `rename` — so a partial or failed install is never a directory another
sandbox can pin.

The slug keeps the scope: every character outside `[A-Za-z0-9._-]` becomes `-`
and a leading `@` is dropped, so `@anthropic-ai/claude-code` is
`anthropic-ai-claude-code` and `@openai/codex` is `openai-codex`. The bare
package name would read better and collide — two harness images whose agents
share a basename under different scopes would land in one directory and pin each
other's versions.

**npm is the fetch mechanism for both agents**, rather than each agent's own.
Claude Code has a native installer with a versioned store
(`$XDG_DATA_HOME/claude/versions/<v>`, with `staging` and `locks` beside it);
codex has nothing of the kind. One mechanism that works for both is worth more
than using each vendor's best one, and npm is the mechanism both vendors publish
through and the one a third-party harness image will already be using.

The path is in the **base layer** because the store is generic. What is
harness-specific is which package goes in it, and that is §7.

### 2. The pin is a symlink in the sandbox's own home, written once

If `~/.local/bin/<bin>` does not exist and the store holds a version newer than
the image's own, it is linked:

```
~/.local/bin/claude -> ~/.local/share/discobox/agents/anthropic-ai-claude-code/<version>/bin/claude
```

`~/.local/bin` precedes `/usr/bin` on the PATH the base layer declares, so
`exec claude` resolves to the pin and **no launcher changes at all** —
`launch.sh` stays exactly what
[0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md)
§3 says a launcher is, in every harness image including ones we do not ship. The
link lives in `$HOME`, a per-sandbox `data` volume, so a restart finds the same
link and runs the same version — requirement (1), as a property of where the
pointer is stored rather than as logic anybody has to run.

**The work runs from `/etc/profile.d`**, not from a launcher. The command is
typed into a terminal's login shell
([0027](0027-harness-terminals-run-as-a-shells-typed-in-job.md)), so the login
sequence is both the earliest point the pin can exist and a place the base image
already owns — `sandbox-dev-tools.sh` sets the PATH this design depends on from
there. Putting it in each `launch.sh` would mean the same two lines in every
harness image, ours and other people's, to do something none of them vary.

**An existing pin is never rewritten.** A sandbox whose store has moved on keeps
its version until someone asks for otherwise (§4).

A sandbox with an empty store pins nothing and runs the image's own copy at
`/usr/bin/<bin>`. Nothing is copied, and nothing blocks on a download at first
launch.

The version directory also carries `pins/<sandbox-id>`, touched every time that
sandbox opens a shell. That file is what retention reads (§5); it is the only
way one sandbox can know another is still using a version.

### 3. Neither agent checks the network at startup

- `harness/claude-code/image.json` env: `DISABLE_AUTOUPDATER=1`. The binary's
  gate reads `DISABLE_UPDATES`, then this, then
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, then the `autoUpdates` config key;
  any one of them disables the updater, and this is the one that says only that.
- `harness/codex-cli/system-config.toml`: `check_for_update_on_startup = false`.

Both are the vendors' documented knobs, set in layers that already exist — the
image env layer, and the system config layer that already sits below
`${CODEX_HOME}/config.toml` so a sandbox can turn it back on for itself.

This is the requirement that forces the rest of the design. With the updaters
off, nothing advances a version on its own, so the store needs an owner.

### 4. The store advances on a stamp in the store, caught up by whoever launches next

`discobox-agent-refresh` takes a `flock` on the store and reads a stamp beside
it. **If the stamp is younger than 12h it exits without touching the network.**
Otherwise it asks `npm view <package> version` — a few KiB — writes the stamp,
and installs only if that version is not already a usable one in the store (§5).

**The trigger is a sandbox being used, not a clock.** The same login sequence
that pins (§2) starts the refresher detached and goes on with the shell, so it
is never in the startup path and nothing waits on it. The
stamp is what makes that a schedule: the first shell opened after the window has
passed does the work, whenever that happens to be, and every shell inside the
window is free. This is anacron's rule, not cron's, and the reason is
[0108](0108-a-sandbox-stops-itself-when-its-terminals-go-quiet.md) — a sandbox
powers itself off when its terminals go quiet, so **nothing is reliably running
at any given hour**. A timer inside a sandbox would fire only in a box that
happened to be up for the whole interval, which for a twice-a-day window means a
box that was up for twelve hours. A stamp in the shared store needs no box to be
up: it needs the *next* box to start, which is exactly when a fresh version is
about to matter.

The window belongs to the store, not to a sandbox, so it is the pool's uid
partition that checks twice a day however many boxes launch. Ten boxes in a
morning are one registry query, and a download only when there is genuinely
something new. That is the honest reading of requirement (3): not "never talk to
the registry", which would freeze the store forever, but "no sandbox's startup
depends on it, and starting ten boxes does not mean ten checks".

**Only a version this pool could actually run is fetched.** `npm view` answers
the `latest` dist-tag, and a tag is a pointer rather than a high-water mark: a
vendor who ships a bad release moves it *back*. A fetch therefore happens only
when the published version sorts above both the store's newest and the image's
own copy — exactly the set §2 would ever pin. Without that test a rollback costs
a 124 MiB download that §5's retention deletes on the next line, because a
version that just landed has no pin and does not sort into the newest two;
every window, forever, and silently, because the refresh is detached. With it, a
rollback is a no-op, and `discobox-harness-upgrade` says the registry is behind
rather than claiming the sandbox is current.

**The stamp is written when the registry is asked, not when the install
finishes.** The query is the thing being rate-limited; a download that fails or
is interrupted simply leaves the version absent, and the next window fetches it
again.

The refresher takes **no keepalive lease**
([0108](0108-a-sandbox-stops-itself-when-its-terminals-go-quiet.md) §4). A
background download is not a reason to keep a box alive, and it does not need to
be: the idle window is 30 minutes by default and the fetch is seconds. A box
stopped mid-download loses a temp directory, never a version — §1's install is a
`rename` — and §5's pass deletes temp directories left behind.

**A refresh never rewrites a pin.** What it produces is what the *next* new
sandbox pins — requirement (2).

`discobox-harness-upgrade` runs the same fetch with the debounce skipped and
re-points the calling sandbox's pin. It is the answer to "I want the newest one
now", and it is a command a person types rather than something that happens to
them.

Because a person is waiting on it, it differs from the background refresh in two
ways. It **queues** for the store lock instead of walking away from a download
already in progress — two sandboxes started a minute apart is enough to meet
that, and "already the newest" from a box that never asked the registry is the
one answer nobody can act on. And it **reports what actually happened**: the
lock timed out, the registry did not answer, the install failed, or the version
moved. Only the last of those is success, and only it exits 0.

### 5. Retention keeps the newest two, plus anything still pinned

After a refresh, versions that are neither among the newest two nor pinned
within the last 30 days (`pins/<id>` mtime) are deleted, along with any temp
directory a killed fetch left behind (§4). Without this the store grows by
210–330 MiB per release, and Claude Code releases most days.

**Retention walks directories, not versions.** The trees that most need deleting
are the ones that are no longer versions: a delete interrupted by an idle
poweroff loses the executable that makes a directory a version, and a pass that
iterated versions could never see that tree again — it would hold its hundreds
of megabytes for the life of the pool cache, invisible to every path here. One
predicate answers "is this a usable version", and everything under the store
that is not one is wreckage: retention deletes it, and a fetch for that version
rebuilds it rather than seeing a directory and skipping.

Deleting a version that a stopped sandbox still points at degrades gracefully:
the pin becomes a dangling symlink, which fails `access(X_OK)`, so PATH
resolution falls through to the image's own copy at `/usr/bin/<bin>`. An older
agent, not a broken box.

### 6. What stays per sandbox, and what else is shared

`~/.npm-global` is **not** shared. It is unversioned (§Context), and it precedes
`/usr/local/bin` on PATH, where the image installs the shims that are required
rather than convenient — the `docker` shim a nested build depends on
([0044](0044-builds-run-on-a-pool-shared-buildkit.md) §8) and the nix shims
([0075](0075-the-nix-store-is-a-pool-shared-cache-seeded-on-first-use.md) §4).
[0107](0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md)
records that a global install through such a directory disarms them for that
sandbox; sharing the prefix would widen that to every same-uid sandbox on the
pool, from a command typed in one of them.

`~/.npm` **is** a cache path:

```jsonc
{ "path": "%HOME%/.npm", "volume": "cache", "uid": "%UID%", "gid": "%GID%", "mode": "0755" }
```

It is npm's content-addressed download cache. Nothing executes from it, it is
safe under concurrency by construction, and it means a store build reuses a
tarball another sandbox already fetched — including the case where two uids or
two pools want the same release.

`~/.local/share/claude`, Claude Code's native store, stays per sandbox. With §3
nothing writes it automatically, and an explicit `claude install` re-points
Claude Code's *own* launcher — which would fight §2's pin. Keeping it unshared
means a hand-run native install changes one box.

### 7. The image declares its agent in a file, not in the manifest

The base image reads `/usr/local/libexec/discobox/agent.conf`:

```sh
AGENT_PACKAGE=@anthropic-ai/claude-code
AGENT_BIN=claude
```

Each harness image ships one; the `shell` harness ships none and none of this
happens for it.

Deliberately **not** a field on `harness.ImageMetadata`. Nothing outside the
sandbox reads it: not the control plane, not the pool agent, not the CLI. A
manifest field would have to cross the image label, the harness config snapshot,
the pool API and `sandbox.json` — the four hops
[0094](0094-the-pool-cache-is-partitioned-by-the-sandbox-users-uid.md) §3 had to
test one by one — to deliver a value only the box it started in ever uses.

It is also not under `/etc/discobox`: boot rebinds the config volume over that
path, so an image file there is shadowed before anything could read it.

## Consequences

- A sandbox runs one agent version for its whole life, however long it lives and
  however many times it restarts. Nothing upgrades it behind its back, and
  nothing nags. `discobox-harness-upgrade` is the way forward for a box that
  wants one.
- A new sandbox starts on the newest version *the pool has*, which may be up to
  half a day behind the registry — longer if nobody launched a box in between,
  since the window only advances when something runs. That is the shape of
  requirement (3), not an accident of the stamp.
- The first sandbox after a pool cache wipe — or on a brand new pool — runs the
  image's version and triggers a background refresh. It is never worse than
  today, and the second box is better.
- **An existing sandbox's `~/.npm` is abandoned in place.** A cache path is a
  plain bind, not an overlay ([0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md)
  §3), so the first start after this change binds the pool cache over whatever
  that sandbox had already downloaded there, and those bytes stay on its data
  volume with nothing able to reach them. Nothing migrates them: the content is
  content-addressed and refetchable, a sandbox is disposable, and a boot-time
  move of a tree of unknown size would be paid by every sandbox that ever starts
  to recover bytes only a long-lived one has. New sandboxes never see it, and
  the version store has no such history — it never existed before.
- The startup path gains a `readlink` and a detached process; it loses two
  network round trips.
- Disk: two versions per agent per uid per pool, plus anything pinned in the
  last 30 days. 12h, 2 and 30 days are the three numbers this design has; they
  live in one script.
- npm is the fetch mechanism, so an agent that publishes elsewhere needs a
  different `agent.conf` shape than the two fields §7 defines. That is the point
  at which this grows a second mechanism, and it should grow one rather than
  teaching the script to guess.
- `pins/<sandbox-id>` puts a sandbox id in the pool cache. It sits inside the
  uid's own partition, which the sandbox already reads and writes whole, so
  nothing is exposed that was not.
- Two agents at two versions on one pool cost two stores. They are separate
  directories with separate retention, which is what makes §5's rule per-package
  rather than global.

## Alternatives rejected

**Share `~/.npm-global/lib/node_modules` and keep `bin` per sandbox.** The first
draft of this ADR. The store would be shared, the link tree local, mirroring
`/nix` with its profiles carved back
([0075](0075-the-nix-store-is-a-pool-shared-cache-seeded-on-first-use.md),
[0094](0094-the-pool-cache-is-partitioned-by-the-sandbox-users-uid.md) §3).
Rejected on requirement (1): npm's global tree holds *one* directory per package,
so a sibling sandbox's upgrade changes the version an existing pin resolves to,
silently, without that box restarting. A version-stable pin needs a
version-keyed store, which npm's global prefix is not.

**Leave the agents' updaters on and cache only the downloads.** Much smaller: two
`cache` declarations, no store, no pin, no refresher. Rejected on (1) and (3)
together — the box upgrades itself mid-session, and every start asks the
registry. It is the design that makes the download cheap and leaves everything
else exactly as it is.

**Copy the pinned version into the sandbox's own data volume.** Gives (1) with
no shared store and no dangling-pin case. Rejected: it pays 210–330 MiB of copy
per sandbox, which is the cost this whole decision exists to remove.

**Use Claude Code's native store and launcher for claude, and something else for
codex.** The native store is versioned, has its own `locks`, and its installer
handles concurrent installers. Rejected because it does not generalize: codex has
no equivalent, so the design would be two mechanisms with two retention policies
and two failure modes. Worth noting the native installer agrees with §2's
approach — when it sees a launcher it did not create, it disables version cleanup
entirely, "because the installer cannot tell which version your launcher needs".
An externally managed launcher means externally managed retention either way, so
we may as well own the store too.

**Refresh from the pool agent instead of from inside a sandbox.** One refresher
per pool is tidier than one debounced refresher per box. Rejected: the pool agent
would have to know each harness image's fetch mechanism, and run it once per uid
partition, for a tree it deliberately does not interpret — it binds
`layout.PoolCache` whole and leaves every path inside it to the sandbox
([0094](0094-the-pool-cache-is-partitioned-by-the-sandbox-users-uid.md) §1). The
box already has npm, the network, and the identity the partition is keyed by.

**A systemd timer in each sandbox rather than a launch-triggered refresh.** The
obvious shape for "twice a day", and it would keep a long-running box's store
current without a relaunch. Rejected on
[0108](0108-a-sandbox-stops-itself-when-its-terminals-go-quiet.md): a sandbox
powers off when its terminals go quiet, so a `OnUnitActiveSec=12h` timer fires
only in a box that stayed up for twelve hours, and a `Persistent=true` timer
catches up per *sandbox* — every box that boots after a gap runs its own check,
which is the per-start registry traffic requirement (3) exists to remove. The
stamp is both halves at once: catch-up like `anacron`, shared like the store it
sits in. It is also one file instead of a unit, a timer and an ordering.

**Let a sandbox ask for a specific version.** Not in scope: the pin is "newest in
the store at first launch", and the only other input is
`discobox-harness-upgrade`. Revisit if pinning to a known-good version after a
bad release is asked for — the store is already version-keyed, so it is a
selection question, not a storage one.
