# 26-11-02-152 — A harness is set up in the box it runs in, and configure is removed

- **Status**: Accepted (supersedes [0009](0009-previous-configure-secrets-are-prefixed-sentinels.md)
  and [26-10-09-867](26-10-09-867-a-configure-port-may-be-ephemeral-and-is-still-bound-at-its-own-number.md);
  supersedes [0002](0002-harness-config-is-the-only-harness-concept.md)'s
  `Configured` enable flag, its server-side configure flow and its
  `harnessMode=config` sandbox, keeping its single harness-config concept and
  deconfigure as reset (§3); completes
  [26-10-26-573](26-10-26-573-a-harness-exports-local-configuration-back-to-its-config.md)
  §8's follow-up)
- **Date**: 2026-11-02

## Context

A harness that needs a credential could not run until a person configured it:
the server created an ephemeral `harnessMode=config` sandbox, the client
attached to the bundle's configure command, and a commit read the command's
output file, applied its files and secrets, and set `Configured`. Every
built-in carried a configure script that duplicated its own sign-in, a seed of
the previous configuration (ADR 0009), and callback ports the client bound
for the sign-in (ADR 26-10-09-867).

Save-back (ADR 26-10-26-573) now captures the same settings and credentials
from any running box: the agent watches the harness's files, exports a
candidate, and a person saves it into the harness config, where every other
box on that config receives it. §8 of that ADR left removing configure as a
follow-up once save-back could create a first credential. It can.

Two ways of reaching the same shared state is one too many. Configure is the
worse one: a sandbox that exists only to be thrown away, a second copy of each
harness's sign-in logic, and an enable gate a fresh harness has to pass before
anyone can try it.

## Decision

### 1. Every registered harness can run

`Configured`, `ConfigureError` and `ConfigureSandboxID` are dropped from the
harness config, in the model, the store and the API. Create, a harness change
and project copy select any registered harness; nothing is gated on having
been configured. A box on a harness with no credential bound starts its
harness signed out, and the person signs in there, the way they would on their
own machine.

The configured overlay stays. `ConfiguredFiles` and `ConfiguredSecretIDs` are
what save-back writes and what a box receives over the bundle's baseline; they
keep their names and their columns.

### 2. Configure is removed

The configure, configure/attach and configure/commit routes, the configure
reaper, `harnessMode=config`, the agent's deferred configure primary, the
configure directory and account, the previous-configuration seed and its
`PREV_` sentinels, and the bundle's `configCommand`, `configReminder` and
`configPorts` are removed. Each built-in bundle drops its configure script.
A bundle that still declares a configure command or configure ports is
refused at registration, saying the declaration is retired, rather than
having it silently ignored.

A sign-in callback needs no declared port. A running box's workspace forward
binds each listening port at its own number when that number is free, which
is where a loopback redirect sends the browser; when it is taken, the person
uses the harness's device-code or paste-back sign-in, as configure's port
warning already told them to.

Existing `harnessMode=config` sandboxes are deleted by migration: each was
an unfinished configure run that nothing can now commit.

### 3. Deconfigure becomes reset

`POST .../deconfigure` becomes `POST .../reset`, `admin harness reset` and the
console's reset action. It removes the saved overlay and the secrets save-back
created for the config, returning it to its bundle's defaults. It no longer
has an enable flag to clear.

### 4. The console explains save-back the first time it is needed

The first time a person's console sees a box report a pending harness change,
it opens a dialog explaining what the change is, that saving it makes it the
shared default for every box on that harness config in the project (signed-in
credentials included), and that discarding leaves only this box changed. It
offers to review the change now. Afterwards the row hint and `H` review are
the only prompts.

Once is per project, recorded on the project as `harnessChangesExplained`,
the same way `welcomed` records its introduction (ADR 26-10-20-596's state is
per machine; this, like the welcome, is about the project's shared harness).
`project update --harness-changes-explained=false` shows it again.

## Alternatives rejected

- **Keep `Configured` and set it on the first save.** A harness would still
  need a way to run before its first save, which is exactly the gate this
  removes; the flag would record only that a save once happened.
- **Keep configure beside save-back.** Two capture paths with different
  semantics (ADR 0009's authoritative replacement against 573's patch) for one
  result, plus a configure script per harness that duplicates its own
  sign-in.
- **Save the first change automatically.** Editing a file in a box is not
  authority to change every other box's defaults or share its login
  (573 §2). The dialog explains the save; it does not make it.
- **Remember the explainer per machine.** It would explain again on a second
  machine and never to the second person on a project, which is why the
  welcome is the project's.

## Consequences

- A fresh project can run any built-in at once; the first box on a credentialed
  harness signs in, and saving that box's change shares the login.
- Harness config rows lose three columns in place; existing overlays and
  configure-created secrets are kept and keep being delivered.
- `discobox configure` stays the console's harnesses screen, now for the
  default, the config, file edits and reset.
- A client built before this change that calls configure gets 404.
