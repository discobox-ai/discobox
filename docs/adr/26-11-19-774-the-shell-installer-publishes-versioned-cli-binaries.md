# 26-11-19-774 — The shell installer publishes versioned CLI binaries

- **Status**: Proposed
- **Date**: 2026-11-19
- **Issue**: [#1457](https://github.com/discobox-ai/discobox/issues/1457)

## Context

[`install.sh`](../../pkg/installer/install.sh) creates its destination directory
and replaces the executable there. On distributions that check for
`~/.local/bin` at login, creating it during install can leave the command
unavailable in the current session. An upgrade leaves no previous binary.
[`admin uninstall`](../../cli/internal/cli/uninstall.go) finds installer
leftovers but protects the running executable and its containing directories.

[ADR 0110](0110-the-install-script-is-a-release-asset-and-a-channel-names-which-one-runs.md)
specifies a stamped installer per release and delegates requests for other
versions to their own installers.
[ADR 26-11-16-249](26-11-16-249-main-is-a-channel-built-into-its-own-repository-and-latest-is-no-channels-name.md)
defines the current channels.

## Decision

### Layout and activation

Use these paths for installs by the Unix installer.

| Scope | Install root | Default external command |
| --- | --- | --- |
| User | `$HOME/.local/share/discobox` | `$HOME/.local/bin/discobox` when its directory exists and is on `PATH` |
| System, when effective UID is zero | `/usr/local/lib/discobox` | `/usr/local/bin/discobox` |

The install root is fixed within each scope so upgrades and uninstall can find
it even if the environment changes. Managed installation state covers an
explicit set of entries below that root, which may also hold other Discobox
data. Each release's verified binary lives at `versions/<release-tag>/discobox`.
The active command is `bin/discobox`, a relative symlink to
`../versions/<release-tag>/discobox`. An external command symlinks to the
absolute path of that stable command. `--dir` and `DISCOBOX_INSTALL_DIR`
still choose the external command directory, with the flag taking precedence.
An explicit directory can be created, with the exception of the user's
`~/.local/bin`, which is eligible only under the default rule above. An
explicit ineligible `~/.local/bin` falls back to the app's own command.

Validate the tag as a single pathname component. Stage and checksum the new
binary before publishing it. Reuse an existing version only if its binary
matches the stamped digest. Treat an occupied version path with different
contents as an error. Publish the prepared file and replace the active symlink
using same-filesystem rename operations. Rename over the symlink itself,
including on BSD systems. A download or validation failure leaves the prior
command usable. An upgrade leaves a running process's executable intact and
updates only the inner link when the outer link already points to it. Perform
the existing client version check and optional server staging through the
activated command.

At eligible external destinations, replace a legacy standalone Discobox
binary atomically with the outer link. Preserve other files and links,
including package-manager links, and report a conflict at the requested
destination. When the default user directory is ineligible, leave it alone
even if it contains a legacy binary. Warn when another command would shadow
the activated command.

### PATH consent

When neither the chosen command directory nor the app's own bin directory is
on `PATH`, offer a profile change for a user install. Read the answer from the
controlling terminal so the ordinary `curl | sh` invocation can prompt without
consuming script input. Delegated installers can use the same terminal. Require
a terminal for the prompt and explicit affirmative consent. On refusal, no
terminal, or an unsupported shell, print the shell-appropriate PATH command
and the absolute executable path for immediate use. For a system install,
print manual instructions if the command is off PATH. System installs make no
user profile edits.

Use the user's login shell, with the supported shell files below. For bash,
place the block in `~/.bashrc` and the first existing login file in bash's
search order (`~/.bash_profile`, `~/.bash_login`, `~/.profile`), using
`~/.profile` if none exists. For POSIX sh use `~/.profile`. For zsh use
`.zshrc` and `.zprofile` under `${ZDOTDIR:-$HOME}`. For fish use
`fish/conf.d/discobox.fish` under `${XDG_CONFIG_HOME:-$HOME/.config}`.
Use an idempotent PATH expression and clear start and end markers. Record any
separator inserted to append the block to a file lacking a final newline.
After consent, report the exact files changed and when a new shell reads them.
An already-on-PATH command requires no profile edit.

### Retention and uninstall

Keep the active release and the two most recently activated distinct releases
other than the active one, across channels. Use successful activation order
so an explicit downgrade retains the build the user just left. Prune only
installer-owned version entries after successful activation. Retained binaries
can be invoked by full path. Defer a local version-switch command until there
is a use case for changing the default without invoking a release installer.

Keep a local record of the installer-owned entries, external links, and
profile edits for each install root. Record exact paths so changes to
`PATH`, shell configuration, and custom destinations do not hide cleanup
targets. Serialize install, prune, and uninstall changes within an install
root, and make interrupted changes discoverable on retry. The manifest format,
locking mechanism, and portable implementation of symlink replacement remain
implementation decisions.

Add these entries to the existing uninstall plan and confirmation flow. Find
a managed install via its known roots and the resolved executable. Before
deleting, verify that links still target the owned command and remove only
intact managed profile blocks. Read profiles again at the time of editing and
preserve all bytes outside those blocks and their recorded separators. Report
an ownership or edit conflict and preserve the affected entry. Remove only
the dedicated managed installation entries, including the active binary on
Unix. Keep the general executable protection for unmanaged installs and the
existing protections around runtime and user data.

### Release and platform boundaries

This changes the Unix destination policy in ADR 0110 §6. The selected
release's stamped installer still owns installation and checksum validation.
A release cut before this change still runs its original installer. Installing
one can replace an external link with a standalone binary or create its old
default directory. A later managed install can migrate that binary at an
eligible destination.

On Linux and macOS, a new `install.ps1` hands off to its release's
`install.sh` after resolving the release, forwarding the selected destination
and staging option. Honor PowerShell's `-NoModifyPath` and
`DISCOBOX_NO_MODIFY_PATH` when handing off. Native Windows keeps the current
PowerShell layout, replacement procedure, and user PATH behavior. Homebrew
continues to own its binaries and links.

## Consequences

A shell install has one active command and up to two retained releases. The
entry on PATH can stay in place across upgrades. The install can succeed
without profile consent because the stable app command can be run directly.
Integration coverage should exercise bash login and interactive startup,
POSIX sh, zsh, fish, consent and refusal through a terminal, no terminal,
repeated installs, legacy migration, a running executable across activation,
retention after downgrade, and byte-preserving uninstall.

## Alternatives considered

- Create `~/.local/bin` and add it to PATH. This changes the search path for
  all executables in a general-purpose directory. Adding the app's own bin
  directory limits consent to Discobox's command.
- Link each external command directly to a version. Every upgrade would have
  to find and update all previously published links. One app-owned link
  provides a single activation point.
- Apply the new layout to every old release through the newest installer.
  This would change which script governs a pinned release and require a new
  source of trusted checksums for historical binaries.
- Use Unix-style symlinks on native Windows. Link creation depends on Windows
  privileges and configuration. Revisit a Windows-specific versioned layout
  when Windows needs retained binaries and has a reliable activation method.
