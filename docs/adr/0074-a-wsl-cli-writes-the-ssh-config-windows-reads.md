# 0074. A CLI in WSL writes the ssh_config Windows reads

Status: Accepted

## Context

`discobox tools vscode` opens a discobox in VS Code over Remote-SSH. ADR 0057
settled how: the CLI refreshes a managed `ssh_config` whose stanzas carry a
`ProxyCommand` naming this executable, and hands VS Code
`--remote ssh-remote+<alias>` plus the working tree to open. Remote-SSH drives
the system `ssh` binary, so putting the host where `ssh` finds it is the only
way to hand it one.

That assumed one machine. WSL is two, and `tools vscode` broke on both counts:

```
$ discobox tools vscode sbx_58nrjgb8cs2k65p0
wrote /home/darren/.local/state/discobox/cli/ssh/proj_5xcm32vpzghwvx3s/config
opening sincere_carson:/home/darren/src/disco2 in /mnt/c/Users/…/Microsoft VS Code/bin/code
Option 'remote' is defined more than once. Using value 'ssh-remote+sincere_carson'.
```

A window opened on the *local* WSL directory, not the discobox.

1. **The launcher rewrites path arguments.** `code` on a WSL PATH is normally
   the Windows installation, reached through interop. Its CLI notices it was
   started from WSL, adds a `--remote wsl+<distro>` of its own — the duplicate
   the warning is about — and translates every bare path argument into a path
   in that remote. The folder we asked for became a folder in the distribution.

2. **The ssh that connects is on the other side.** A Windows VS Code runs
   Windows `ssh.exe`, which reads `%USERPROFILE%\.ssh\config` — not the
   `~/.ssh/config` in the distribution the CLI just wrote an `Include` into. Its
   contents would be no use there either: the `ProxyCommand` names a Linux
   binary Windows cannot execute, and `IdentityFile` and `UserKnownHostsFile`
   name paths Windows cannot open.

Fixing only the first leaves `Could not resolve hostname`. The command is not
usable on WSL until both are fixed.

## Decision

### 1. The ssh installation the config is written for is chosen by the program that will drive ssh

`sshTarget` (`cli/internal/cli/ssh_target.go`) is the OpenSSH installation an
emitted config is written for: the state directory its files live in, the
`ssh_config` that gains the `Include`, the spelling of a path inside it, and the
`ProxyCommand` that reaches this CLI from it. Every path it produces carries two
spellings — the one this process opens (`/mnt/c/Users/…`) and the one the
reading `ssh` uses (`C:\Users\…`).

`admin ssh-config` writes for this machine's own ssh, as it always has.
`tools vscode` picks the target from the editor it resolved: a Windows program
launched from WSL means the Windows target. Nothing else in the CLI is
WSL-aware, because nothing else hands work to a program on the far side —
`tools ssh` carries its own connection and never touches `ssh_config`.

Which build the editor is comes from `wslpath -w`, not a `/mnt` prefix test:
where the drives are mounted is configurable, and a path inside the
distribution answers unmistakably as a `\\wsl.localhost` UNC share.

### 2. The Windows `ProxyCommand` re-enters the distribution

```
ProxyCommand wsl.exe -d "Ubuntu" -e "/home/u/bin/discobox" --server "…" admin ssh-proxy
```

`ssh.exe` cannot execute a Linux binary, and the CLI it needs — the one holding
this machine's endpoint, credentials and iroh identity — is the Linux one. So
the command crosses back rather than being duplicated: `-e` so no shell inside
re-splits what `%COMSPEC%` already parsed, and quoting chosen by the target
rather than by `runtime.GOOS`, because the process writing the line is not the
one whose shell will run it.

`wsl.exe` gives the Linux process the pipe pair ssh hands its `ProxyCommand`,
which is what the protocol needs; the console translation people run into with
`wsl.exe` applies to a terminal, and ssh never gives it one.

Requiring a native Windows `discobox.exe` instead was rejected: it would need
its own endpoint configuration, its own enrolled key and its own iroh identity
on a machine where the user has already set all of that up once, on the side
they work on.

### 3. The identity is copied to the Windows side

Windows `ssh.exe` opens the key file itself, so the key exists on both sides of
the boundary: the private key resolved and enrolled in the distribution is
copied into `%LOCALAPPDATA%\discobox\cli\ssh\`, rewritten on every run so a
rotated or re-enrolled key never leaves a stale copy behind.

Rejected alternatives:

- **Name the key over `\\wsl.localhost`.** Nothing is duplicated, but Windows
  OpenSSH runs an owner and permission check before reading a private key, and
  a 9p share does not reliably satisfy it. A key that cannot be read fails with
  "Bad owner or permissions" and nothing else.
- **Generate a separate Windows key and enroll it too.** Nothing crosses the
  boundary and it is revocable on its own, at the cost of a second enrolled key
  per machine authenticating the same person from the same machine — which
  `resolveSSHIdentity` already goes out of its way to avoid.

The copy takes the ACL of the Windows directory it lands in — the user, SYSTEM
and Administrators — which is what `ssh.exe` requires and what a mode bit set
from the Linux side could not give it.

### 4. VS Code is pointed at a folder URI, never a path

```
code --new-window --folder-uri vscode-remote://ssh-remote+devbox/home/agent/repo
```

A folder URI carries its own remote authority, so there is nothing for the
launcher to translate and no `--remote` for it to duplicate. This is not
conditional on WSL: one form that is right everywhere beats a second code path
that is only exercised on one platform. `--remote` is still used for the one
case a URI cannot express — a discobox that never reported where its source
landed, which opens on the host with no folder.

## Consequences

- On WSL, `tools vscode` writes into the Windows user's profile:
  `%LOCALAPPDATA%\discobox\cli\ssh\<project>\`, an `Include` in
  `%USERPROFILE%\.ssh\config`, and a copy of the SSH private key. It says so on
  stderr; it is not something to leave the user to infer from a path.
- Two spellings of every managed path means the `Include` machinery compares
  paths the way the reading client does — case-insensitively, separator
  normalized — rather than the way `filepath` does.
- `admin ssh-config --write` on WSL still writes only the distribution's own
  config. Someone who wants Windows-side `git` or `scp` over these stanzas has
  no command for it yet; `tools vscode` is the one that knows a Windows program
  is involved. If that turns out to be wanted on its own, the target belongs
  behind a flag there rather than in a second mechanism.
- Values in the emitted stanzas that can contain a space — `IdentityFile`,
  `UserKnownHostsFile`, and the `Include` line — are quoted. A Windows profile
  under a name with a space in it is ordinary, and unquoted `ssh` reads the
  first word as the whole filename.
