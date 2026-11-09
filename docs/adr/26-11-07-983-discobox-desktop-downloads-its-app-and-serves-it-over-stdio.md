# 26-11-07-983 — `discobox desktop` downloads its app and serves it over stdio

- **Status**: Accepted (§2's staging directory and §4 amended before anything shipped: the app is staged in the CLI's state directory, and Linux comes first with macOS and Windows deferred; §5 and the Consequences amended with §4)
- **Date**: 2026-11-07
- Accepted by the owner by merging #844, 2026-11-07
- §2, §4, §5 and the Consequences amended 2026-11-09 (#801), before anything shipped
- **Issue**: [#786](https://github.com/discobox-ai/discobox/issues/786) (epic), [#787](https://github.com/discobox-ai/discobox/issues/787)

## Context

The desktop console is a Rust program on Rune
([26-11-07-876](26-11-07-876-the-console-is-one-core-with-two-renderers.md)).
The console's core, its server access and its credentials are Go, in the CLI.
Two questions follow: how the two programs run together, and how the Rust one
reaches a user's machine.

Checked at `00541f9c90fefeabe1b61134a737fab287861803`:

- The CLI reaches servers through `endpoint.HTTPClient`. The transports are
  the local socket or named pipe, HTTPS and iroh. Reconnecting attach goes
  through `pkg/execstream/resume`, with the token and debug transports
  layered on (`cli/internal/cli/root.go`'s `httpClientWithAutoStart`).
- Rune's event loop is winit's. On macOS it must own the process's main
  thread.
- A release CLI is linked with a server manifest
  (`internal/cmd/discobox-server-manifest`), which pins the server release
  it can launch by digest.
- The prototype embedded the app with `go:embed` behind a `desktop` build
  tag. The CLI grew from 67 MB to 104 MB on one platform. The `task dev` loop
  rebuilds `build/discobox` without the tag, so the prototype also had to
  look for the app beside the CLI.

## Decision

### 1. The app is a child process the CLI serves

`discobox desktop` starts the desktop app as a child process and serves the
bridge ([26-11-07-289](26-11-07-289-the-desktop-bridge-carries-the-console-core.md))
on its stdin and stdout. The app holds no credentials, knows no server
addresses and makes no network requests. `--server`, `--token`, configured
servers, auto-start and every transport behave exactly as for
`discobox console`. The app exits when the bridge closes, and the command
exits when the app does.

### 2. The app is downloaded on demand, pinned by the CLI

A release CLI is linked with a desktop manifest, beside its server manifest.
For each platform the manifest gives:

- the URL of that release's desktop app asset;
- the asset's SHA-256 digest;
- its size.

The first `discobox desktop` downloads the asset through the CLI's own HTTP
client and proxy settings, then verifies the digest. It unpacks the asset into
the CLI's state directory under that digest
(`<discobox state>/desktop/<digest>/`, beside the staged servers) and runs it
from there.

*Amended 2026-11-09, before anything shipped.* This said the user cache
(`<cache>/discobox/desktop/<digest>/`). A staged app is run without being
hashed again, so its directory must be this user's alone. Inside a discobox,
`~/.cache` is shared by every box in the pool, so another box could create the
digest's directory first and choose what runs. The staged servers are in the
state directory for the same reason.

A later CLI release unpacks its own app beside the old one, never over a file
that may be running. Old digests are pruned once no running app uses them. A
download that fails or does not verify is reported, and nothing is run.

### 3. Development and pinned overrides

`--binary`, then `DISCOBOX_DESKTOP_BINARY`, name an app to run in place of the
manifest's. A development CLI, built without a desktop manifest, looks for
`discobox-desktop` beside its own executable. That is where
`task build:desktop` puts it, and it survives the `task dev` loop's rebuilds.

### 4. Platforms: Linux first; macOS and Windows deferred *(amended 2026-11-09)*

*Amended during implementation, before anything shipped.* This originally
released the app for macOS (arm64 and amd64) and Windows (amd64 and arm64),
and had Linux built and tested in CI but not released, with `discobox desktop`
on Linux saying so. The owner then scoped the track to Linux: "I am fine if we
just get it working on Linux first and do windows later as it is not well
supported by rune anyhow", and "Scope the work to Linux only and keep it out of
windows/Mac ci".

On Linux, `discobox desktop` downloads and runs the app (§2). On macOS and
Windows it says the platform is not yet supported: their builds are kept out of
CI, so a release has no app for them to download. An app named with `--binary`
or `DISCOBOX_DESKTOP_BINARY` (§3) still runs anywhere.

Revisit macOS and Windows when the owner brings them back into the desktop CI
jobs; Windows waits on Rune's Windows support, including the in-window menus
and dialogs it needs (Rune's native menus are macOS-only).

### 5. Signed packages

- **macOS:** the asset is an app bundle (`discobox.app`), signed and notarized,
  so the app has its own name, Dock icon, menu bar and notifications.
- **Windows:** the asset is a signed executable.
- **Linux** *(amended 2026-11-09, with §4)*: the asset is a gzip-compressed
  tar holding the executable. There is no platform signature to add; the
  digest is the check.

Each is verified by digest before anything is run, whatever the platform's
own checks.

## Alternatives rejected

- **Load the app into the CLI process (a cdylib through purego).** winit
  needs the main thread on macOS, which the Go runtime also uses. Two runtimes
  in one process would also share signal handling and crash together. A child
  process costs one pipe.
- **Embed the app in every CLI.** Each CLI would grow by about 37 MB per
  platform for a command many users never run. Every release would also ship
  the app inside the binary that `brew` and `winget` install.
- **A separate package (a Homebrew cask, a winget package).** The app would
  then version independently of the CLI that serves it, and the bridge would
  need a compatibility matrix. Pinning by digest keeps one CLI, one app.
- **The app speaks to the server itself.** It would duplicate the transports,
  login and resume protocol in Rust, and the app would hold credentials.
- **A local HTTP or WebSocket server for the app.** Any local process could
  reach it, so it would need its own authentication. Stdio is reachable only
  by the parent.

## Consequences

- The release pipeline builds the app for each supported platform (Linux
  alone while §4 holds), uploads the assets, and writes their digests into the
  desktop manifest the CLI is linked with. Signing and notarizing the macOS
  bundle and signing the Windows executable come back with those platforms.
- The first run needs network access to the release host. An offline machine
  can use `DISCOBOX_DESKTOP_BINARY`.
- The prototype's `go:embed` path and `desktop` build tag are removed.
