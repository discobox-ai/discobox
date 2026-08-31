# 0078. The WSL bridge, corrected: quoting, the key's ACL, and both sides written

Status: Accepted

Amends [0074](0074-a-wsl-cli-writes-the-ssh-config-windows-reads.md) §2, §3, and
its consequence that only `tools vscode` writes the Windows side.

## Context

ADR 0074 was written from the Windows and WSL documentation and landed without
a Windows machine to try it on. Run on real WSL2, the config it generates is
correct in every particular — the `Include` chain resolves, the stanza matches,
the mirrored key is found — and the connection still fails:

```
debug1: Executing proxy command: exec wsl.exe -d "Ubuntu-24.04" -e "/home/…/discobox" --server "unix:///…" admin ssh-proxy
kex_exchange_identification: banner line contains invalid characters
banner exchange: Connection to UNKNOWN port 65535: invalid format
```

Two of its decisions were wrong about how Windows behaves, and each was wrong
in a way that reads as something else.

**The quoting.** §2 said to quote each word "chosen by the target rather than by
`runtime.GOOS`", meaning `%COMSPEC%`'s double quotes. `cmd.exe` does hand the
line on with those quotes intact, but `wsl.exe` does not strip them from the
words it consumes: the Linux side gets an `execvp` of a program whose name
begins with a double quote, fails, and `wsl.exe` reports that failure **in
UTF-16 on stdout** — where ssh expects an identification string. Hence "invalid
characters" rather than "invalid format": the bytes ssh read were NUL-laden
text, not a wrong banner. Replacing only the quoting, with the same chain and
the same binary, connects and authenticates.

**The ACL.** §3 said the copy "takes the ACL of the Windows directory it lands
in — the user, SYSTEM and Administrators — which is what `ssh.exe` requires".
It does not, either way it can be written:

```
written from WSL:      D:PAI(A;;FA;;;S-1-5-21-…-1001)(A;;FA;;;S-1-5-32)
created from Windows:  beenie\CodexSandboxUsers:(I)(RX)  NT AUTHORITY\SYSTEM:(I)(F)  …
```

WSL puts an explicit `S-1-5-32` ACE on everything it creates on a drive mount,
and a file created on the Windows side inherits whatever the profile grants
below it — here a group with read access. ssh refuses a private key any
principal but its owner can reach, so both are refused, with the message
"Permissions for … are too open" from a program the user never ran directly.
`os.WriteFile` over an existing file makes it worse: it replaces the contents
and keeps the old DACL, so a key inherited a stale ACL from a native Windows
CLI that had written to that path years earlier.

## Decision

### 1. §2 amended: the command is quoted for the shell that parses it

```
ProxyCommand wsl.exe -d Ubuntu-24.04 -e sh -c "exec '/home/u/bin/discobox' --server '…' admin ssh-proxy"
```

The command goes to `sh -c` as a single double-quoted argument, which `cmd.exe`
and `wsl.exe` pass across whole, and inside it the path and the endpoint carry
POSIX single quotes, which `sh` strips. Nothing inside that argument is a double
quote, so there is no nesting to get wrong, and a path or an endpoint with a
space in it — which is why the quoting existed — still survives.

Words `wsl.exe` reads for itself, the distribution name among them, are bare: a
quoted one names a distribution that does not exist. A distribution whose name
contains a space is therefore unreachable this way, which is a limit worth
having over a form that fails for everyone.

Leaving the words bare instead was rejected: it works today only because
nothing on the machine has a space in its path.

### 2. §3 amended: the ACL is set, and then read back

After the copy, `icacls <key> /inheritance:r /remove:g *S-1-5-32 /grant:r
<user>:F` — the boundary's equivalent of `restrictToUser`, which cannot run
from the Linux side because a mode bit written there means nothing to Windows.
`/inheritance:r` drops what the profile grants downward, `/remove:g` drops the
ACE WSL adds, and the user is granted the file outright: full control rather
than read, because this CLI rewrites the key on every run.

The result is read back and the run fails if any other principal remains.
`icacls` reports success for a grant that leaves another ACE in place — which
is exactly the failure this exists to prevent — and the cost of not checking is
an error from Remote-SSH about a file the user did not create.

Creating the file from the Windows side instead, so it would inherit a clean
ACL, was rejected: measured on the machine that motivated this, the inherited
ACL is the worse of the two.

### 3. Both of a machine's ssh installations are written, by both commands

0074 gave the choice of target to `tools vscode`, on the grounds that it is the
command that knows which program will drive ssh, and left `admin ssh-config
--write` writing this side only. That was the wrong cut. A WSL2 machine has two
ssh installations and the programs that drive them are spread across both — a
Windows VS Code or JetBrains Gateway on one side, `git`, `scp` and `rsync` on
the other — so a machine configured on one side only works until you reach for
the other tool. It also made `tools vscode` the *only* way to produce the
Windows config, which is a strange thing for a command that opens an editor to
be responsible for, and it showed: the first time that config needed
regenerating during development, the way to do it was to launch VS Code.

So `machineSSHTargets` answers with every ssh installation on the machine, and
both commands write all of them. The stanzas are built once and rendered per
target: what differs between them is how a path is spelled and what the
`ProxyCommand` runs, not which sandboxes exist or which key authenticates.

Printed output — `admin ssh-config` with no `--write` — stays this side only.
It exists to be pasted into a config by hand, and the hand doing that is here.

The Windows side needs interop to resolve at all, so its failure is a warning
rather than an error: `--write` has still written a usable config for this
side, and says what it could not do. `tools vscode` launching a Windows editor
is the one case where it is fatal, because there is then nothing for the editor
to connect through.

## Consequences

- The private key exists on the Windows side with an ACL this CLI sets rather
  than one it inherits, and the run fails loudly if that cannot be achieved.
- `admin ssh-config --write` on WSL now writes, and rewrites, files under the
  Windows profile. That is a wider blast radius for a command that used to
  touch only this side, and the reason it is acceptable is that every file it
  writes there is one this CLI owns and rewrites wholesale anyway.
- The `ProxyCommand` now names `sh`, so the distribution must have one at
  `/bin/sh`. Every distribution WSL ships does.
- Two lessons for anything else that crosses this boundary: `wsl.exe` does not
  strip quotes from the words it reads, and its own diagnostics arrive on
  stdout in UTF-16, which is indistinguishable from a corrupted stream unless
  you are looking for it.
