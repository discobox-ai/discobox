# 26-10-16-535 — A pasted image is carried into the discobox as a file

- **Status**: Accepted (§2's exec of `sh -c 'cat > …'` superseded in part by
  [26-10-22-499](26-10-22-499-a-discobox-has-a-files-api-that-acts-as-its-user.md))
- **Date**: 2026-10-16

## Context

The agents a discobox runs take an image two ways: Ctrl-V (Alt-V on Windows)
reads the system clipboard, and a pasted path to an image file is attached.
Inside a discobox neither works. There is no clipboard, and the path a terminal
pastes for a dragged file names a file on the user's machine. The console sits
between the user's terminal and the session, so it can read what the discobox
cannot.

## Decision

1. **The console reads the image on its own machine.** On Ctrl-V or Alt-V in a
   harness terminal, on an empty bracketed paste, and on a paste that is only
   paths to image files, it reads the OS clipboard (osascript on macOS,
   wl-paste or xclip on Linux, PowerShell on Windows and from WSL) or the named
   files (`cli/internal/clipimage`).
2. **It writes the image into the discobox and pastes the path.** The bytes go
   on the stdin of an exec (`sh -c 'cat > …'`) under
   `${TMPDIR:-/tmp}/discobox-paste`. The pane is given the remote path as a
   bracketed paste, which is the shape of a dragged file and what every agent
   already takes as an image. The agents need no change.
3. **Ctrl-V is taken only on harness terminals, and only when there is an
   image.** With no image the key goes through unchanged. Shells and tools
   never have it taken.
4. **Input is held while an image is in flight.** Keys and pastes that arrive
   meanwhile are delivered after the path, in order.
5. **A dictation tool's Ctrl-V is answered from a clipboard watch.** Wispr
   Flow writes the text to the clipboard, presses Ctrl-V about 55ms later, and
   restores the clipboard about 0.5s after that. A read from WSL takes
   0.3–1.7s, so it races the restore and usually loses. The console therefore
   watches the Windows clipboard for as long as it is open, and pastes text that
   arrived within a second before the key. The watch is a message-only window
   registered with `AddClipboardFormatListener`, so it uses no CPU while idle.
   It runs inside the console on Windows. On WSL it runs as a Go helper
   embedded in the Linux CLI, installed under
   `%LOCALAPPDATA%\discobox\bin\<digest>\`.

## Alternatives rejected

- **`discobox cp` / scp.** It needs an scp binary, an enrolled SSH key and the
  SSH ingress, all for a file of a few hundred KiB. The exec stream is already
  open to the same discobox and works on every platform the console runs on.
- **Content in argv** (as `installToolFiles` does). Linux caps one argument at
  128 KiB, and screenshots are often larger.
- **Forwarding the clipboard into the discobox** (a fake `xclip`/`wl-paste`
  there that asks the console). Each harness reads its clipboard its own way,
  and with nobody attached there is no console to ask. The file is what every
  harness already accepts.
- **OSC 52 reads.** OSC 52 is text only, and most terminals refuse reads.
- **A PowerShell clipboard watcher** instead of the helper. It needs no build
  step, but it takes 0.5–1.7s to start, holds 60–80 MB, and its `Add-Type`
  is blocked in Constrained Language Mode.
- **Polling the clipboard sequence number.** Each check takes about 5µs, but
  waking Go's scheduler every 20ms cost 0.8% of a core while idle. The
  notification approach costs nothing while idle.
- **A C helper.** It would be about 20 KB rather than 860 KB gzipped, but it
  needs a C cross-toolchain in the flake. The Go helper is the same code the
  console runs on Windows.
- **Taking Ctrl-V everywhere.** Shells use it for quoted-insert and vi for
  visual block, and on WSL each probe starts PowerShell (most of a second).

## Consequences

- The image bytes go through the exec's input log like any other stdin.
- From a console run over SSH, the clipboard read is the remote host's. There
  is usually none, and the key goes through unchanged.
- Pasted images accumulate under the discobox's temporary directory until it
  is cleaned.
- While the console is open it sees every clipboard change, passwords
  included. It keeps only the latest change, in memory, never logs it, and uses
  it only for a Ctrl-V within a second of the change.
- Each Linux CLI carries both Windows helpers, about 1.6 MB.
