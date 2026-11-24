# 26-11-24-372 — A person exempts the Windows server from Winsock LSPs once, and it runs from a fixed path

- **Status**: Proposed
- **Date**: 2026-11-24
- **Issue**: #1566
- **Would amend**: [0099](0099-the-cli-downloads-the-server-it-starts.md) §5.
  On Windows, a staged server runs from one fixed directory that the verified
  versioned set is linked into. It no longer runs from inside the versioned
  directory. Staging, verification, the versioned layout and the resolution
  order (§6) are unchanged.
- **Relates to**: #1567 (detecting a third-party LSP and saying what it
  means), [0120](0120-wslc-guest-programs-are-streamed-in-not-mounted.md)
  (every guest program is streamed in over a guest process's stdio, so stdio
  is required).

## Context

On a Windows host where a VPN or proxy tool has installed a layered Winsock
service provider (an LSP, such as Astrill's `ASProxy64.dll`), every guest
operation on a wslc pool fails with `HRESULT 0x8007273D` (WSAEOPNOTSUPP), and
the pool reconciler retries it forever.

The cause, checked at `e6f5ea446`:

- The wslc driver gets a guest process's stdin and stdout from the WSL service
  through `IWSLCCompatProcess::GetStdHandle`
  (`server/providers/wslc/internal/wslcsession/comapi.go`, `GetStdHandle`).
  The handle comes back as a marshalled Winsock `SOCKET` (an hvsocket). COM
  unmarshals it **inside `discobox-server.exe`**, through that process's
  Winsock catalog. An LSP layered over it cannot take the hvsocket's protocol
  info, and fails the unmarshal.
- `Session.Exec` (`.../wslcsession/session.go`) returns the `GetStdHandle`
  error unwrapped, so the log shows a bare HRESULT with no `start guest
  process` prefix. Every guest exec fails: the relay install
  (`server/providers/wslc/relaysession.go`), the `/var/lib/docker` resize
  (`server/providers/wslc/storagedisk.go`), and everything after them.
- WSL avoids this for itself. Every executable under `C:\Program Files\WSL\`
  is tagged `LSP_SYSTEM` (0x80000000) with `WSCSetApplicationCategory`, so
  Winsock leaves out any LSP that does not declare itself system-safe. The
  reporter set the same category on `discobox-server.exe` and the tour worked
  straight away.
- `WSCSetApplicationCategory` writes HKLM, so it needs an elevated token.
  `WSCGetApplicationCategory` does not. Nothing in the repository calls
  either one. The installer (`pkg/installer/install.ps1`) is per-user and
  never elevates. The winget package is a portable zip holding only
  `discobox.exe` (`scripts/winget-manifests.sh`). Nothing else in the tree
  runs elevated either: the only elevation-aware code is
  `pkg/layout/privileged_windows.go`, which only checks for it.
- The category is keyed by the executable's path. Each staged server sits in
  its own versioned directory,
  `%LOCALAPPDATA%\discobox\server\windows-amd64\v<version>\discobox-server.exe`
  (`pkg/serverstage`, `Manifest.Dir`; ADR 0099 §5). So a category set on one
  version does not carry over to the next.
- `discobox.exe` makes no wslc calls. The driver lives only in the server
  module, and `cli/internal/wsl` only shells out to Windows from inside a WSL
  distribution. An LSP in the CLI does it no harm.

The issue asks three things: where the elevated step lives, how it survives
upgrades, and which processes need it.

## Decision

### 1. The elevated step is an explicit, opt-in command

`discobox admin server lsp-exempt` exists on Windows builds only. It tags the
server the CLI would run (§3) with `LSP_SYSTEM` and then checks the result.

- **Status first.** It reads the current category with
  `WSCGetApplicationCategory`, which needs no elevation. It reports whether
  the server path is already exempt. `--check` does only this and changes
  nothing.
- **Elevation is per call.** Run from an elevated prompt, the command makes
  the call directly. Otherwise it starts itself elevated through
  `ShellExecuteEx` with the `runas` verb, which shows the UAC prompt, and
  waits for that process. The elevated process runs one hidden subcommand. It
  takes the absolute path as an argument from the unelevated parent and
  refuses anything that is not an existing file named `discobox-server.exe`.
  It sets only `LSP_SYSTEM`, keeping whatever other category bits are
  already set. The path has to come from the parent: with over-the-shoulder
  credentials, the elevated process runs as a different user, whose
  `%LOCALAPPDATA%` points at another profile.
- **Verified, then explained.** After the elevated process exits, the parent
  reads the category again and reports what it found, never what it
  expected. Winsock reads the category when a process starts, so the command
  then says that a running local server must restart, and names
  `discobox admin server shutdown`. The next command autolaunches the server
  again.
- **Undo.** `--remove` clears the `LSP_SYSTEM` bit through the same elevated
  path. `discobox uninstall` does not elevate. If the bit is set, uninstall
  says the entry remains, that it is harmless, and how to remove it.

Nothing runs elevated without the person asking for it. The step changes
what the server's own traffic passes through (see Consequences), so it is the
person's choice to make.

### 2. Where the step is offered: the place that recognises the failure

The command is offered where the failure is recognised, which is #1567's
detection. When the server sees WSAEOPNOTSUPP from a guest operation on
Windows and finds a non-Microsoft LSP in the catalog, its message names the
LSP and gives the command. It does this only if the running server's path is
not already exempt, which `WSCGetApplicationCategory` answers without
elevation. The message travels on the paths #1567 sets up (the server log, the
pool's status, and the startup warnings the CLI already prints from
`health.Status.Warnings`). This ADR adds only the offer to that message. The
detection, the translation of the code and the retry limit belong to #1567.

### 3. On Windows the staged server runs from one fixed directory

The category is keyed by path, so the path has to stay the same across
upgrades. On Windows, a server from 0099 §6 step 2 or step 4 (a staged set)
runs from a fixed directory, `<state>\discobox\server\windows-amd64\run\`. It
does not run from its versioned directory.

- **Filled by hard links from the verified set.** Each asset of the versioned
  directory is hard-linked into `run\`. Both directories are under the same
  state root, so they are on the same volume, and a link is instant. Copying
  is the fallback when linking fails. The versioned directory stays the
  record (0099 §5): it is what is staged, verified, and rolled back to, and it
  keeps `manifest.json`. `run\` holds no `manifest.json`, so
  `migrateLegacyLayout` never takes it for a legacy set. Its name does not
  contain `.staging-` or `.replaced-`, so `sweepAbandoned` never removes it.
- **Refreshed only after the older server stops.** A running image cannot be
  replaced. `endpoint.EnsureRunning` resolves the new server *before* it stops
  an older one (`pkg/endpoint/autolaunch.go`), and that order stays the same:
  resolution can mean downloading for minutes while the old server keeps
  answering. So `run\` is brought to the resolved version in the switch-over
  gap, after `replaceOlderServer` and before the launch. A local link
  operation adds no visible time to that gap. When the CLI finds `run\`
  already holding the version it resolved, it does nothing.
- **Checked before it runs.** Before `run\`'s command is executed, its size
  and modification time are compared with the versioned copy, and `run\` is
  relinked if they differ. This is not a re-hash. Like 0099 §5, it does not
  defend the user's own state directory against the user. It catches a
  half-finished refresh.
- **Overrides stay as they are.** A `--binary` / `DISCOBOX_SERVER_BINARY`
  path, and a sibling of the CLI (§6 step 3, `build\` in development), run
  from where they are. The person chose those paths. `lsp-exempt` tags
  whatever the resolver returns. If that is not the fixed directory, the
  command also says that the exemption applies to that path alone.
- **Windows only.** Every other platform keeps 0099 §5 exactly. Nothing there
  is keyed by path.

### 4. Only the server is exempted

`discobox-server.exe` is the process that unmarshals the wslc sockets, so it
is the only one tagged. `discobox.exe` is not tagged. It makes no wslc calls,
and tagging it would take the VPN's proxying off the CLI's own traffic
(downloads, remote servers) for no gain. If the CLI ever makes wslc calls
itself, revisit this.

## Alternatives rejected

- **The installer elevates and tags.** `install.ps1` is per-user and
  deliberately never elevates. winget installs never run it. The LSP may
  also be installed after Discobox, which leaves an installer-time step
  nothing to act on. A UAC prompt in every install, for a problem most
  machines do not have, is the wrong default.
- **Offer it on the first wslc pool start.** Pools start in the background
  server, which has no terminal. A UAC prompt raised from a detached process
  appears with no context, at a moment the person did not choose. The pool
  start is where the failure is *seen* (§2), but not where it can be *acted
  on*.
- **Re-tag each new versioned path at stage time, or when the CLI first
  starts it.** Every upgrade would need a UAC prompt, raised in the middle of
  whatever command triggered the upgrade. Stale entries for every old version
  would also pile up in HKLM. The fixed path (§3) needs one prompt, ever.
- **A symbolic link or junction at the fixed path.** Winsock judges the image
  path the process was loaded from. Through a reparse point, that can be the
  target's (versioned) path, which would undo the point of the fixed path. A
  hard link is its own name for the file.
- **A scheduled task or service that re-tags with standing privilege.** This
  is a permanently elevated component installed to save a single prompt. It
  adds far more privileged surface than the problem needs.
- **Move the COM and stdio work into a separate, exempted helper process,**
  so that the server's own traffic keeps passing through the LSP. Each socket
  is used by the process that unmarshals it, so the helper would have to own
  every guest exec and stdio relay: in effect, the wslc driver out of
  process. That is a large split, made to keep a VPN's app-level proxy on
  registry pulls that work without it. **Deferred.** Revisit if someone
  reports needing the LSP on the server's own egress.
- **Detection and an error alone** (#1567 by itself). It explains the failure
  but leaves the user to find and run `NoLsp.exe` by hand, and to do it again
  on every upgrade. #1567 still lands, and it is where §2's offer is made.

## Consequences

- One UAC prompt fixes a machine for good. After it, upgrades run from the
  same path and keep the exemption.
- An exempt server's own Winsock traffic (staging downloads, image pulls,
  remote provider APIs) no longer passes through LSPs that do not declare
  themselves system-safe. That is the same trade WSL makes for its own
  executables. VPNs that work at the network or WFP layer are unaffected.
  The command says this before it elevates.
- The tagged path is under the user's writable profile, so any program the
  user runs could put an executable there and get the exemption. That gains
  it nothing. An LSP is not a security boundary, and such a program can
  already make its own Winsock calls.
- The HKLM entry is per machine. It names one user's path, and another user's
  server path stays unexempted until that user runs the command.
- `discobox admin server stage`, and the resolver's reporting of where a
  server is, describe the versioned directory as before. Only the
  autolaunch and `discobox admin server` execute from `run\`.

## Open for implementation

- Whether "the older server has stopped" is a new hook in
  `endpoint.LaunchOptions`, or the refresh is part of a `Command` that is
  resolved in two phases. Either way, the order in §3 is what must hold.
- The exact text of the message offering the command, which lands with #1567.
- Whether `Extra` (the command-line argument identity in
  `WSCSetApplicationCategory`) has to be empty to match a server started
  with environment but no arguments, as the autolaunch does (0099 §7). The
  reporter's manual fix suggests it does, but this has to be confirmed on a
  Windows host with an LSP installed. Unit tests can cover only the path,
  linking and category-bit logic.
