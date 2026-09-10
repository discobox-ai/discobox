# 0102 — A mirrored key carries the ACL ssh reads, and the Windows side may fail

- **Status**: Accepted
- **Date**: 2026-09-10
- **Supersedes**: [0078](0078-the-wsl-proxycommand-is-quoted-for-sh.md) §2's
  "granted to the user alone" and the read-back that enforced it. §1 (the
  `ProxyCommand` quoting) and §3 (both installations are written by both
  commands) stand, and this ADR is built on them.

## Context

ADR 0078 §2 set the mirrored private key's ACL from WSL with

```
icacls <key> /inheritance:r /remove:g *S-1-5-32 /grant:r <user>:F
```

and failed the run if the read-back showed any principal but the user. On the
machine that motivated it that read back clean. On a WSL2 machine whose drive
mount hands its files the profile's own entries, it does not:

```
sync SSH config: C:\Users\sheng\AppData\Local\discobox\cli\ssh\id_ed25519 is
readable by NT AUTHORITY\SYSTEM, BUILTIN\Administrators, and ssh will not read
a private key that anyone else can; grant it to sheng alone
```

Both halves of that were wrong.

**"ssh will not read a private key that anyone else can" is not what ssh does.**
Windows OpenSSH checks a private key's DACL and accepts three principals: the
owner, `NT AUTHORITY\SYSTEM`, and `BUILTIN\Administrators`. It has to — those
two are on every file a Windows profile holds, including the keys `ssh-keygen`
writes into `%USERPROFILE%\.ssh`, which ssh reads without complaint. This
repository already knew it: `restrictToUser`, the native-Windows half of the
same job, grants exactly those three and says so. Only the WSL half disagreed,
and it was the half with no Windows machine to check against.

**And the failure took the whole run down with it.** `discobox run` refreshes
the project's ssh_config after the create, so a discobox was made, provisioned
and left running, and then the command exited non-zero over the ACL of a
mirrored key — for a second ssh installation that the attach about to happen
does not use at all. The user's `ps` after a few attempts was eight running
discoboxes and no terminal. 0078 §3 had already decided the shape of the
answer — "the Windows side needs interop to resolve at all, so its failure is a
warning rather than an error" — and applied it to exactly one call, the one
that resolves the Windows folders. Everything after it stayed fatal.

## Decision

### 1. The mirrored key is granted the three ACEs ssh reads a key under

```
icacls <key> /inheritance:r /remove:g *S-1-5-32 \
    /grant:r <user>:F /grant:r *S-1-5-18:F /grant:r *S-1-5-32-544:F
```

The same three `restrictToUser` grants natively, so a key reaches ssh.exe with
the same ACL whichever side of the machine this CLI is installed on. Removing
SYSTEM and Administrators takes nothing away from anybody — Administrators can
take ownership of the file regardless — and costs the user the recovery path
they would need if it ever went wrong.

The well-known two are granted by SID rather than by name: `BUILTIN\Administrators`
and `NT AUTHORITY\SYSTEM` are spelled in the machine's display language, and a
grant by an English name is a grant to nobody on a machine that is not.

### 2. The read-back counts the ACEs rather than naming them

The check itself is kept: `icacls` reports success for a grant that leaves an
explicit ACE in place, which is the whole reason it exists, and an error here
beats "Permissions for … are too open" from a program the user did not run.

What it can no longer do is match names, for the reason the grants use SIDs.
So it counts: three ACEs, one of them this user's, is the grant above and
nothing else. A fourth is an explicit entry that survived `/inheritance:r`, and
the error names every principal on the file so the reader can see which.

Parsing SDDL out of `icacls /save` was rejected. It answers the question
exactly, by SID, and it costs a temp file on the Windows side, a UTF-16 decode,
and an SDDL parser — for a check whose only job is to notice that the ACL is
not the one just set.

### 3. A target that may fail carries that on itself

`sshTarget` gains `optional`, set on the Windows side of a WSL machine and
nowhere else, and every step of building and writing a config honours it: a
failure is reported through the caller's note sink and that target is dropped.
This side's config is written, and the work that asked for the refresh carries
on.

A copy this cannot vouch for is removed again — a wide ACL, but equally an
`icacls` that could not be run or did not answer — which §2's read-back did not
have to do while it was fatal. The key is on the Windows filesystem
only to be read by ssh.exe; leaving it there under an ACL that was just refused,
on a failure the caller now carries on from, would deposit exactly the file the
read-back exists to prevent.

`tools vscode` launching a Windows editor clears it, which is the same
exception 0078 §3 already made for the same reason: that editor connects with
Windows OpenSSH, and a window opening on a config that was never written is
worse than the error explaining why it could not be.

Making the failure non-fatal at each call site instead was rejected: `run`, the
launcher window, `admin ssh-config --write` and `tools vscode` all reach this
through two functions, and four copies of the same judgement is how one of them
ends up being the one that still fails.

## Consequences

- A WSL machine whose drive mount hands its files SYSTEM and Administrators —
  which is the ordinary case, not the exception — can run `discobox run`.
- A refused key leaves nothing behind, so the Windows side of a machine that
  keeps failing this check has no discobox key on it rather than one no program
  will read. The run that refused it writes no stanzas for that side either —
  the target is dropped before the write — but what an *earlier* successful run
  left there stays: a machine that worked once and now fails keeps a Windows
  config, still `Include`d, naming an `IdentityFile` that is now gone. The next
  run that passes the check rewrites both; nothing prunes them in between.
- Nothing about the Windows side can fail a create. What it could not do is
  said on the same line the create narrates everything else on, and a run that
  ends in an attach takes that row back before the terminal starts, so the
  reader of a failed Windows write is whoever runs `discobox admin ssh-config
  --write` or `discobox tools vscode` afterwards.
- The read-back is a count, so it cannot say *which* of the ACEs is the
  unexpected one, only that there is one and what the file lists. That is
  enough to act on and it is language-independent, which naming them was not.
- Two lessons, both of them 0078's: a decision made from documentation about a
  platform nobody on the project runs is a hypothesis, and the place to find
  out is a user's machine. And a bridge to another platform should degrade to
  a warning by default — the config it writes is a convenience for a second
  program, never the thing the command was asked to do.
