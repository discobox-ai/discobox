# 26-11-07-983 — `discobox desktop` downloads its app and serves it over stdio

- **Status**: Proposed
- **Date**: 2026-11-07
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
the user cache under that digest (`<cache>/discobox/desktop/<digest>/`) and
runs it from there.

A later CLI release unpacks its own app beside the old one, never over a file
that may be running. Old digests are pruned once no running app uses them. A
download that fails or does not verify is reported, and nothing is run.

### 3. Development and pinned overrides

`--binary`, then `DISCOBOX_DESKTOP_BINARY`, name an app to run in place of the
manifest's. A development CLI, built without a desktop manifest, looks for
`discobox-desktop` beside its own executable. That is where
`task build:desktop` puts it, and it survives the `task dev` loop's rebuilds.

### 4. Platforms: macOS and Windows; Linux deferred

The desktop app is released for macOS (arm64 and amd64) and Windows (amd64
and arm64).

Linux is built and tested in CI so the code stays portable, but it is not
released and `discobox desktop` on Linux says so. Revisit Linux when both
hold:
- the in-window menus and dialogs needed for Windows (Rune's native menus are
  macOS-only) are complete;
- the app has been verified under Wayland and X11 with the distributions'
  Vulkan and GL drivers.

### 5. Signed packages

- **macOS:** the asset is an app bundle (`discobox.app`), signed and notarized,
  so the app has its own name, Dock icon, menu bar and notifications.
- **Windows:** the asset is a signed executable.

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

- The release pipeline builds the app for four targets, signs and notarizes
  the macOS bundle, signs the Windows executable, uploads the assets, and
  writes their digests into the desktop manifest the CLI is linked with.
- The first run needs network access to the release host. An offline machine
  can use `DISCOBOX_DESKTOP_BINARY`.
- The prototype's `go:embed` path and `desktop` build tag are removed.
