# 0138 — A shell opened for a person is their own shell, when the sandbox has it

- **Status**: Accepted
- **Date**: 2026-09-18

## Context

`discobox shell` with no command, and a shell pane in the TUI, both send
`shell: true` and let the sandbox resolve the run user's login shell from its
passwd entry (`execs.ResolveShell`). Boot creates that account with
`--shell /bin/bash`, so in practice every shell a person opens is bash, whatever
they use every day. The CLI said so deliberately: "the local `$SHELL` describes
this machine and says nothing about the identity the exec runs as."

That is still true of the *account*. It is not true of the person sitting in
the terminal, who has a shell they prefer and — in a repository whose flake
provides it — a sandbox that can run it. Two facts shape how:

1. **The shell may exist only after direnv runs.** A repository's `.envrc`
   (`use flake`) is what puts zsh, fish or nu on PATH. Nothing can look it up
   before the login profile and the starting directory's `.envrc` have loaded,
   and only a shell can load them.
2. **Everything else typed into a shell is written for the login shell.** A
   harness terminal types a single-quoted `discobox-harness-run '…'` into it
   (ADR 0027); a service runs `<shell> -lc '<script path>'`. nu reads either as
   a string literal, not a command, and `nu -l` reads none of `/etc/profile`.

## Decision

1. **The preference is the client's `DISCOBOX_SHELL`, else `nu` under
   nushell, else its `$SHELL`, sent per request and stored nowhere.** Reading `DISCOBOX_SHELL` first lets a
   person want a different shell in a sandbox than on their own machine. Run
   from nushell, the preference is `nu`: nu sets `NU_VERSION` for what it starts
   but leaves `$SHELL` naming the login shell it was launched from.
   `discobox shell` (when its streams are a terminal) and the TUI's shell pane
   put it in the exec request's `env` as `DISCOBOX_SHELL`
   (`sandboxshell.PreferredEnv`). Nothing on the server, the sandbox record, or
   the manifest records it; it belongs to whoever is at the keyboard.
2. **The sandbox still decides the login shell, and passes it as the
   fallback.** For `shell: true` with no `shellCommandLine` and no
   `startupCommand`, and a `DISCOBOX_SHELL` in the *request's* env, the agent
   runs `discobox-shell <preferred> <login shell> -l`, where the login shell is
   `ResolveShell`'s answer, unchanged. A preference naming the login shell
   itself — the same path, or the same name — runs the login shell unwrapped.
   The same variable arriving through the image or manifest env is ignored.
3. **The image's `discobox-shell` finds the shell after the environment
   loads.** In a subshell that has sourced `/etc/profile` — which ends by
   exporting the starting directory's `.envrc` — it looks for the preferred
   path, else its basename on PATH. It then `exec`s what it found with `SHELL`
   set to it, failing that the fallback, having first set the terminal title to
   that shell's name. A shell with no profile.d of its own (zsh, nu) starts with
   the loaded environment, so the Nix PATH and the `.envrc` are there. The
   fallback, and any shell that reads `/etc/profile` itself (bash, sh, dash,
   ksh, mksh), start from the environment the launcher was given: a second run
   of the profile resets PATH, and direnv, seeing the `.envrc` already loaded,
   would not put the repository's back.

4. **The base image ships zsh and nu, each set up the way bash is.** zsh from
   Debian; nu, which Debian does not package, as the pinned upstream static
   musl build for amd64 and arm64. Each gets a direnv hook of its own
   (`direnv hook zsh` in `/etc/zsh/zshrc`, a pre-prompt hook in nu's vendor
   autoload directory), and zsh's first-start questionnaire is replaced by
   seeding Debian's recommended `.zshrc`, so the first shell a person with
   either `$SHELL` opens is usable as it is.

Services, harness terminals, `shellCommandLine` execs, piped `discobox shell`,
and SSH sessions keep the login shell.

## Alternatives rejected

- **A `preferredShell` field on the exec request.** The sandbox API rejects an
  unknown field (`additionalProperties: false`, ogen's strict decode), and a
  sandbox keeps its image — and so its agent — until it is explicitly upgraded
  (ADR 0016). A new field would have turned every existing sandbox's shell pane
  into a 400. An env entry an older agent does not recognize is passed through
  and changes nothing.
- **Making the preferred shell the account's passwd shell** (a `--shell` on
  `discobox new`, or `useradd --shell`). Every service and harness terminal
  would then run under it, and fact 2 breaks them for nu; `nu -l` also never
  reaches `/etc/profile.d`, so the Nix PATH and direnv would be gone too.
- **A profile.d trampoline in bash** (`exec "$DISCOBOX_SHELL" -l` at the end of
  the login sequence). It works, but it can only be told which terminals may
  switch by a variable the agent sets anyway, it depends on the login shell
  being bash, and the exec record would report `bash -l` for a zsh session.
  The launcher's argv is what the record reports — the preferred shell and the
  fallback — so the exec says what was asked and what it falls back to, and
  the terminal title the launcher sets names the one that ran.
- **Resolving the shell in the agent with `direnv exec`.** The agent would be
  running direnv and a Nix evaluation itself, and the shell it found would
  still start without `/etc/profile`.
- **Storing a per-user or per-sandbox shell setting on the server.** A
  preference that follows the keyboard needs no storage, and one on the server
  would have to answer whose it is when two people open the same sandbox.

## Consequences

- A person whose `$SHELL` is zsh or nu gets it in every sandbox on the base
  image, and any other shell in a sandbox that has it — including one whose
  flake alone provides it — and bash, with no error, in one that does not.
- A shell other than bash, zsh or nu has the environment at start but reloads
  `.envrc` on `cd` only with its own direnv hook (`direnv hook fish`, say) in
  the person's own startup files.
- The login profile runs twice for a shell found by the launcher — once in the
  lookup, once in that shell or for it — and once in the lookup alone for the
  fallback. Every profile.d script is safe to repeat, so this costs time; the
  lookup's output is discarded, so direnv's messages print once.
- A pane opened through the launcher is titled by the launcher until its shell
  sets a title, so the launcher sets one: the name of the shell it starts.
