# 26-11-24-780 — A macOS sandbox has nix and Homebrew, installed on its own disk

- **Status**: Proposed
- **Date**: 2026-11-24
- **Issue**: #1414
- **Would supersede**: [0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md)
  §7's "and the nix and Homebrew seeds" in the absent list. Nothing else in
  §7 changes: nested Docker, the runc wrapper, the pool-shared build path and
  the desktop stay absent.
- **Relates to**: [0075](0075-the-nix-store-is-a-pool-shared-cache-seeded-on-first-use.md),
  [0085](0085-the-nix-seed-stamp-names-the-store-that-seeded-it.md),
  [0107](0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md),
  [26-10-14-129](26-10-14-129-a-discobox-keeps-its-own-nix-daemon-socket.md),
  [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md),
  [26-11-03-986](26-11-03-986-a-machine-pool-snapshots-a-plan-by-committing-a-vm-it-provisioned-and-shut-down.md),
  [26-11-11-963](26-11-11-963-one-vm-provider-type-every-pool-runs-a-linux-pool-agent-in-a-machine-its-driver-makes.md)
  §§8–9, [26-11-12-060](26-11-12-060-a-macos-sandbox-account-takes-uids-from-501.md),
  and [26-11-01-122](26-11-01-122-a-harness-is-only-a-bundle-and-a-sandbox-takes-its-kind-and-platform-from-its-pool.md)
  (Proposed, which would narrow 0145 §3's declared absence to a refusal).

## Context

0145 §7 lists what a non-Linux sandbox does not have: nested Docker and
`runc`, the pool-shared build path, the Xfce desktop, and "the nix and
Homebrew seeds". It calls each one a Linux-container mechanism with nothing
to act on outside Linux. `sandbox-agent/DESIGN.md` (A macOS Sandbox) repeats
the list.

For nix and Homebrew that conflated two things. The seed, the cache volume
and the overlay are Linux-container mechanisms. The tools they deliver are
not. Both tools run on macOS natively, and Homebrew started there.

The owner decided on #63, on 2026-11-19, that a macOS sandbox should be as
close to a Linux one in capabilities as it can be, with nix and brew
installed. Only Docker and the desktop stay absent. That decision is
recorded in the bot's summary on #63 (issuecomment-7001256035): "the darwin
platform entry carries every feature but docker and desktop". PR #1416 (for
#63) builds the platform bundle's darwin/arm64 entry that way.
`sandbox-agent/image.darwin.json` carries nix's env (`NIX_REMOTE=daemon`,
`NIX_CONFIG`, the nix profile PATH entries) and Homebrew's (`HOMEBREW_*`,
`/opt/homebrew/bin` and `sbin`). The `nix` and `brew` feature directories
are in that entry too. Their steps are still Linux's, and #1413 gives them
macOS ones. This ADR is the decision that entry and #1413 build on.

How each tool reaches a Linux sandbox today:

- **nix** (`sandbox-agent/features/nix`). `install-nix.sh` runs the
  Determinate installer with `--init none` and moves the store aside to
  `/usr/local/lib/discobox/nix`. `/nix` is a pool-shared `cache` volume
  (`image.json`, ADR 0075 §2). `discobox-nix-seed.service` copies the seed
  into it on first use, under a stamp that names the store (0075 §3, §5;
  0085). PATH shims in `/usr/local/bin` start that chain before any client
  binary exists (0075 §4). Profiles, gcroots and the daemon socket are
  per-sandbox `data` volumes inside `/nix` (0075 §2; 26-10-14-129).
  `nix-daemon.service.d/proxy.conf` gives the socket-activated daemon the
  proxy env and the MITM bundle.
- **Homebrew** (`sandbox-agent/features/brew`). `install-brew.sh` clones a
  pinned tag into `/home/linuxbrew/.linuxbrew`. The tree is `root:brew`,
  group-writable and setgid. The prefix is a `data` volume, so the
  provisioned tree is an overlay's lower layer (ADR 0107 §1, §3). Boot adds
  the sandbox's uid to `brew` because no uid is known when the tree is built
  (0107 §2). A root sandbox gets brew as well (0107 §4).

A macOS sandbox has none of the ground those mechanisms stand on. It has no
volumes: its disk is its `data`, and its cache is a directory on that disk
(0145 §6). It has no overlay and no systemd. It has one account, which the
darwin base VM gives a uid from 501 (0145 §5, 26-11-12-060). That base VM is
the macOS install the server makes once per host and macOS version
(26-11-11-963 §9). A darwin pool's sandboxes and snapshots are created
`From` it, and vz commits a provisioned VM as a plan snapshot (986;
26-11-11-963 §8).

## Decision

### 1. A macOS sandbox has every platform feature but nested Docker and the desktop

A darwin platform bundle carries every feature the Linux one does except
`docker` and `desktop`. That includes `nix` and `brew`. A feature in that
bundle has darwin steps that work, selected with `when: {os: [darwin]}`
(`provisionspec.Selector`), and its Linux steps carry `when: {os: [linux]}`.
If a feature turns out to have no darwin counterpart, it leaves the darwin
bundle and is declared absent there. It is never shipped with steps that
cannot work. Which steps each toolchain feature takes is #1413's to decide,
not this ADR's.

0145 §7's absent list becomes nested Docker and the runc wrapper, the
pool-shared build path, and the Xfce desktop. The reasoning 0145 gives for
those still holds.

### 2. nix is the multi-user install, its daemon run by launchd, its store on the sandbox's disk

The `nix` feature's darwin steps run the nix installer for macOS as a
multi-user install, with the daemon. That matches `NIX_REMOTE=daemon` in
`image.darwin.json` and the Linux sandbox. On macOS this means:

- **`/nix` is an APFS volume on the guest's own disk.** macOS's sealed system
  volume refuses a directory at `/`. So the installer adds a "Nix Store"
  volume to the disk's APFS container, a `nix` entry in `/etc/synthetic.conf`,
  an `fstab` line, and a launch daemon that mounts the volume at boot. The
  template already uses `synthetic.conf` for `/run` and `/workspace`
  (`sandbox-agent/DESIGN.md`), so the nix entry is added beside those lines
  and never replaces them. The volume lives in the same container as the
  sandbox's data, so a vz commit and every sandbox created `From` it carry
  it. No pool volume is involved.
- **The daemon is the installer's launch daemon** (`org.nixos.nix-daemon`),
  started by launchd. macOS has no socket activation of the systemd kind, so
  the daemon runs from boot.
- **The daemon is given the proxy and the CA.** As on Linux, it is the only
  part of nix that uses the network, and it inherits no exec's env. Egress is
  proxy-only (0145 §7), and a macOS sandbox has no tun device (26-11-11-963
  Deferred). So the darwin steps give the daemon's launch daemon the same
  proxy variables and the same MITM bundle (`NIX_SSL_CERT_FILE` or
  `SSL_CERT_FILE`) that `proxy.conf` gives `nix-daemon.service`, from the
  proxy env the agent renders. How the steps place them (by editing the
  installer's plist, or with a launchd step type) is left to implementation.
  So is how the darwin agent renders the proxy env file the daemon reads.
- **The build users are the daemon's, not a sandbox account.** The installer
  creates its `nixbld` group and build users in macOS's system id range,
  below the 501 that the sandbox's account starts from (26-11-12-060). 0145
  §5 limits execs to the one account. It says nothing about the system
  accounts a daemon runs builds as. No exec can name a build user.
- **devenv and bash** are installed into the default profile, as
  `install-nix.sh` installs them on Linux.

### 3. Homebrew is at `/opt/homebrew`, owned by the sandbox's account

The `brew` feature's darwin steps install Homebrew at `/opt/homebrew`. That
is the one prefix Apple-silicon bottles are built for. Any other prefix
turns every formula into a source build, which is the same reason Linux uses
`/home/linuxbrew/.linuxbrew`. The version is pinned to a release tag, as
`install-brew.sh` pins it on Linux.

The prefix is owned by the sandbox's account. Homebrew's own macOS install
works this way, and on macOS Homebrew refuses to run as root. The install
step therefore runs as that account, never as root. The account and its uid
are fixed in the darwin base VM, before any provisioning (26-11-12-060), so
the group of 0107 §2 has no problem to solve here. No `brew` group exists,
and none is added to `additionalGroups`.

Homebrew on macOS needs the Xcode Command Line Tools. Whether `brew`, `build`
or the darwin base VM supplies them is left open (Open, below). This ADR only
requires that `brew` can rely on them being there.

### 4. Both are installed by provisioning and cached by the plan snapshot

Both tools are installed by their features when the agent provisions the
box (26-10-16-423), as every other feature is. On a vz pool the result is
part of the plan snapshot the pool commits (986; 26-11-11-963 §8). So the
nix installer's volume and launch daemons, and Homebrew's tree, are built
once per plan and pool. Every later sandbox on that plan starts with them.
A create with no snapshot provisions them from scratch, and the snapshot is
built behind it, as on Linux.

The darwin base VM does not get either tool. It stays the macOS install
plus the account (26-11-11-963 §9). Everything a platform pin names stays in
the platform bundle.

### 5. What the Linux design keeps, and what has no macOS counterpart

| Linux mechanism | On macOS |
|---|---|
| Store shipped aside and seeded into a pool-shared `/nix` cache (0075 §1–§3, §5) | **None.** The store is installed in place on the sandbox's disk. There is no volume to share and no seed to copy. |
| Seed stamp naming the seeding store, merge into a warm cache (0085) | **None.** Nothing is merged, because each sandbox's store is its own. |
| PATH shims that start the seed before the client exists (0075 §4) | **None.** The client is in the store from provisioning on. |
| No garbage collection in the sandbox (0075 §6) | **Does not apply.** That rule protected a store other sandboxes share. A macOS sandbox's store is its own, and collecting it is the user's choice. |
| Profiles and gcroots as per-sandbox `data` volumes inside a shared `/nix` (0075 §2) | **None needed.** All of `/nix` is per-sandbox already. |
| The daemon socket as a per-sandbox `data` volume (26-10-14-129) | **None needed.** The problem was a socket on a store shared across sandboxes, and here nothing is shared. |
| `nix-daemon.service.d/proxy.conf` | **Kept, in launchd's form** (§2). |
| Homebrew tree baked under a `data` volume as an overlay's lower layer (0107 §1, §3) | **None.** The prefix is a directory on the sandbox's disk, and a later `brew install` writes to it directly. |
| The `brew` group and boot adding the sandbox's uid to it (0107 §2) | **None.** The account owns the prefix (§3). |
| Root sandboxes get brew (0107 §4) | **Does not apply.** A macOS sandbox's agent is its account, never root. |

What both platforms share: the tool, the env and PATH that
`image.darwin.json` declares, `NIX_REMOTE=daemon`, and the rule that the
daemon alone uses the network.

### 6. Neither is part of the durable tree

On Linux, `/nix`'s per-sandbox paths and the Homebrew prefix are
`excludeFromExport` (`image.json`). They are provisioned content, and a
restored box provisions them again. The same holds on macOS. An export
(0123, 0145 §8) carries neither `/nix` nor `/opt/homebrew`. Where a macOS
export draws that line is left to the macOS export's implementation. This
ADR only requires that those two paths fall outside it.

## Alternatives rejected

- **Keep nix and Homebrew absent, as 0145 §7 says.** This is the owner's
  call, and the owner made it the other way (#63, 2026-11-19). It is also a
  poor fit on the merits. 0145 §7's reason, "a Linux-container mechanism
  whose subject does not exist here", fits the seed and the overlay. It does
  not fit the tools, which run on macOS natively.
- **Bake both into the darwin base VM when the server installs macOS.** It
  would save the first create on each plan an install, but the snapshot
  already covers that cost (§4). It would also put content that no platform
  pin names into a base that is rebuilt only for a new macOS version. Moving
  to a new nix or Homebrew release would then need a reinstall of macOS
  rather than a new platform bundle. 26-10-16-423 made platform content
  provisioned content so that a pin could name all of it.
- **A single-user nix install, owned by the account.** It would avoid the
  build users and the daemon, but it would differ from `NIX_REMOTE=daemon`,
  which Linux sets and `image.darwin.json` declares. It would also let the
  account write the store directly, where the daemon is otherwise the only
  writer. The current nix installer for macOS supports only the multi-user
  install anyway.
- **Share a store across a darwin pool's sandboxes, as Linux does.** A macOS
  sandbox has no volume to share one on (0145 §6). Building one would bring
  back the pool-and-sandbox shared filesystem that 0126 and 0145 §6 refuse.
  The plan snapshot already makes the second sandbox's store free to get.
- **Install Homebrew under `/usr/local`, or in a `brew` group's tree.**
  `/usr/local` is the Intel prefix, and on arm64 it means source builds. A
  group-owned tree solves a uid that is unknown at build time, and the darwin
  base VM already fixes the uid.

## Consequences

- A macOS sandbox gets `nix`, `nix develop`, `devenv` and `brew` at the
  paths and env a Linux one uses, apart from the Homebrew prefix. A project
  whose setup runs either tool can run it on macOS.
- The first create on a darwin pool for a given plan waits for the nix
  installer and Homebrew, plus the Command Line Tools wherever those come
  from, on top of the rest of provisioning. That is minutes. The snapshot
  amortizes it, and the pool reports the snapshot build (26-11-17-316).
- A darwin sandbox's disk grows by its own `/nix` volume and Homebrew tree.
  No pool on any platform shares a store across macOS sandboxes. A darwin
  pool's disk use is per sandbox, apart from what the snapshot's clone
  shares.
- A nix daemon runs in every macOS sandbox from boot, not on first use.
- 0145's status line, and the index row for 0145, are to name this ADR as
  superseding §7's nix and Homebrew clause once it is accepted.
  `sandbox-agent/DESIGN.md` (A macOS Sandbox) then drops its note about the
  departure and says what the nix and brew features do on darwin, with
  #1413's steps.

## Open

- **The Command Line Tools.** Homebrew needs them, and so does every
  compiler-using feature. Which one supplies them on darwin is open: `build`,
  `brew`, or the darwin base VM. It is settled with #1413's feature steps.
  The choice must respect §4: content a platform pin should name stays out of
  the base VM.
- **The `/nix` volume across a clone.** Implementation confirms that a
  sandbox created `From` a committed snapshot mounts the Nix Store volume at
  boot, whether or not the installer encrypted it. Its key, if there is one,
  is in the system keychain on the same disk.
- **Re-applying the features on a provisioned box.** A new platform pin
  re-applies features in place (26-10-16-423). The darwin `nix` step does not
  run the installer again over an existing install, and the `brew` step
  updates its pinned checkout. How each step detects what is already there
  is implementation.
