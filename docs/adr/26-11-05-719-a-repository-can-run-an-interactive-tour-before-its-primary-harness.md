# 26-11-05-719 — A repository can run an interactive tour before its primary harness

- **Status**: Accepted
- **Date**: 2026-11-05
- Accepted by the owner by merging #671, 2026-11-05
- **Issue**: [#633](https://github.com/discobox-ai/discobox/issues/633), including
  the owner's choice of `discobox-ai/tour` and a pre-harness program.
- **Relates to**: [0027](0027-harness-terminals-run-as-a-shells-typed-in-job.md),
  [0055](0055-a-delivered-source-settles-before-its-sandbox-runs.md),
  [0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md),
  [26-11-02-152](26-11-02-152-a-harness-is-set-up-in-the-box-it-runs-in-and-configure-is-removed.md).

## Context

The owner wants a separate public `discobox-ai/tour` repository, launched with
`discobox run discobox-ai/tour`. Its menu should teach secrets, access requests,
and console features such as opening a shell and tools, then offer to continue
into the harness the user has already selected. An agent prompt cannot provide
this deterministic experience before that agent starts or signs in.

At `8dfd97bdeb9817dffde6cc6ba566a71405f7e353`:

- `cli/internal/cli/run.go` treats the repository as source and the remaining
  words as a harness prompt. A repository alone does not select a tour program.
- `sandbox-agent/terminal/service.go` creates and revives the primary terminal
  through a login shell, typing its resolved harness command as a foreground
  job. The reserved shell harness has no typed startup command. The same
  service creates auxiliary shells and harness terminals.
- `harness/DESIGN.md` specifies `discobox-harness-run` and its resume convention;
  manifests can override both launch commands. Editing each included harness
  launcher would miss third-party harnesses and explicit overrides.
- `sandbox-agent/DESIGN.md` already describes source readiness, automatic
  `.envrc` loading, and separately managed services. A pre-harness program must
  wait for readiness without blocking the sandbox agent or other services.

The new architectural choice is a source-owned interactive launch gate, separate
from both harness lifecycle-event hooks and provisioning. The tour's prose and
menu layout do not belong in the CLI binary.

## Decision

### 1. The tour is an ordinary public source repository

Publish `discobox-ai/tour` with an executable `.discobox/scripts/pre-harness`
entry point. The documented command is `discobox run discobox-ai/tour`, using
normal source delivery, harness selection and attachment. There is no special
repository-name check, tour harness, new CLI command, or host-side script.

The repository supplies the menu, examples, and a README explaining that its
program runs inside the box. It uses the Linux platform's existing runtime;
additional tools it needs must be declared through supported feature settings
and awaited before use. It must not install a second toolchain just to show a
menu. The tour does not require an agent account or subscription. Continuing
into a selected agent can require that agent's ordinary sign-in.

### 2. The primary terminal has one source-owned pre-harness gate

This initial contract targets Linux sandboxes, including those reached from
macOS and Windows hosts. Native non-Linux sandbox hooks are deferred until a
concrete tour or repository needs a portable executable contract.

On each actual creation or revival of the **primary** terminal, run the primary
source's `.discobox/scripts/pre-harness`, if present, before its resolved harness
command. This includes a primary whose selected harness is `shell`: success
leaves the person in that primary login shell. An absent hook preserves current
behavior. A present but non-executable file, directory, invalid interpreter, or
execution failure is a failure, never silently treated as absence.

Resolve the path from the primary source's checkout root, not the requested
working subdirectory. Only the primary source contributes it; additional work
sources and skills sources do not. The hook must be a regular executable file,
with its resolved path inside that checkout; reject a symlink escaping the
checkout. Its interpreter is the executable's shebang, without evaluating the
file as shell text. Its working directory is the terminal's resolved working
directory, as for the harness.

Auxiliary shells, tools, services, plain execs and additional harness terminals
bypass this primary-only gate. In particular, opening a shell to repair or explore
the tour cannot recursively open the tour. Attaching to an already running
terminal and thawing its existing process do not rerun it. A stopped primary's
next real launch does, using the hook currently in the checkout.

This is sandbox code execution as the existing run user, with the primary's
environment and ordinary credential bindings. It gains no host execution,
root identity, API access, automatic approval, or credential delegation. A
repository can already execute code in the sandbox, including through `.envrc`;
the documentation must also make this new automatic execution point explicit.

### 3. Keep the hook and harness in the same interactive foreground flow

The sandbox agent's terminal layer owns composition of the primary startup
sequence, for both creation and revival. It invokes a platform-owned launch
runner as a job of the existing login shell. That runner executes the hook,
waits for it, and only on success executes the already resolved harness argv.
This common platform mechanism is needed to sequence two interactive programs;
it does not wrap or replace each harness bundle's launcher contract.

The hook receives no harness prompt or resume arguments. The continuation argv
is passed as arguments to the runner, never evaluated as a generated shell
expression. Preserve the existing initial/resume command selection, prompt
arguments and wake environment. Apply the gate to explicit manifest command
overrides too. For the shell harness there is no continuation command: returning
success returns to the original shell, without nesting a second login shell.

Run after source readiness, required provisioning and terminal file/environment
setup, using the terminal's PTY, input, resize and signal behavior. The hook may
wait indefinitely for a person; it is not a readiness task or an API request
that holds a lifecycle lock. The terminal must be attachable while it waits.
The ordinary shell foreground-job model and Ctrl-Z behavior of ADR 0027 remain.

Exit zero continues. Any nonzero exit, signal or execution error prevents the
harness from starting and prints the hook path and failure status in the
terminal. The underlying login shell remains available for recovery. Do not
retry automatically, report that the harness ran, or mark the tour completed.
The person can repair and rerun the entry point or restart the primary. The
hook's descendants follow the terminal's normal process cleanup rules.

### 4. The tour owns repetition and its menu

The runtime has no global or persisted "tour completed" flag. The repository
stores completion state in the box's durable user data, outside its checkout,
and scoped to the tour. Selecting **Continue to my harness** writes that marker
and exits zero. Later primary launches return zero promptly when it is present,
so restarting an agent does not force another walkthrough. A new box starts
with a fresh tour. The repository documents a command to replay the menu in an
existing box without launching another harness when that command returns.

Before completion, greet the person and ask them to press Enter. Offer independent
chapters, a return to the menu, **Continue to my harness**, and **Exit tour**.
Exiting or interrupting without continuing returns nonzero and leaves the shell;
completion is explicit, not inferred from visiting every chapter. The menu is
usable with simple keyboard input and offers visible progress without requiring
mouse support or full-screen terminal graphics.

The required chapters are:

- **What this box is:** copied source, sandbox versus host, durable work, and
  where the selected harness will run.
- **Open a shell:** guide the person through the console's real shell action,
  make a harmless workspace change, and return to the still-running menu.
- **Tools:** open the real tools picker and try an available in-box tool.
  Host editor integration is explained and optional when that editor is absent.
- **Secrets and requests:** issue a real, narrowly scoped credential request,
  explain the host inbox and approval preview, and let the person approve or
  deny it. Use a clearly labeled disposable demo value, never a production
  account. Explain sentinels and scope without displaying any credential.
  Denial and cancellation return to the menu; no approval is required to finish.
- **Harnesses:** explain the selected harness, in-box sign-in and reviewing
  saved changes. Explain changing a stopped box's harness; do not stop this
  tour or switch the person's project default as an exercise.
- **Leaving and returning:** demonstrate detach/reattach and explain stop,
  restart, and explicit cleanup from the host. Never archive the tour box
  automatically or imply that detaching stops it.

The access chapter requests no delegation and cleans up outstanding demo requests
on cancellation where the protocol supports it. It does not need API access to
other boxes. Exact demo credential policy, optional verification target, menu
implementation, progress format and prose remain implementation choices, checked
against the then-current request and console APIs. No remote demo service is a
prerequisite. Ports, services, monitors, multiple pools and child-box orchestration
are deferred; revisit them when first-use feedback shows a concrete missing task.

### 5. Publish one launch contract across entry points

README, public getting-started documentation and the website point to the same
command and describe that the menu precedes the selected harness. They explain
how to exit, replay and clean up. Coordinate wording with #632, #634 and #635;
this decision does not preserve their current welcome, attach or approval UX.

Implementation acceptance requires driving that exact command from a fresh
install on Linux, macOS and Windows hosts into a Linux box, with and without an
existing default harness. Exercise the signed-out case, shell selection,
interactive chapters, request approval and denial, continuation, interrupted
hook, restart before and after completion, reconnect, and shell recovery after
failure. Verify an auxiliary shell bypasses the hook and an explicit harness
command override still follows it. Repositories without a hook retain their
launch behavior. Site and tour repository changes require their own authorized
changes; this ADR PR creates neither repository content nor product behavior.

## Alternatives rejected

- **An agent-driven tour prompt.** It requires a working signed-in harness and
  cannot reliably teach the actions needed before that harness starts.
- **Package the whole tour with the CLI or platform.** It couples instructional
  content to releases and rejects the owner's separate-repository choice.
- **Modify every `discobox-harness-run` script.** It misses overrides and
  third-party bundles, and conflates source behavior with harness ownership.
- **Use `.envrc`, a shell profile, or a boot service.** Those run in the wrong
  lifecycle, can affect auxiliary shells, or lack the primary foreground PTY.
- **Run the gate on every shell or attachment.** It recurses during the shell
  chapter and interrupts an existing conversation on reconnect.
- **A runtime once-only success flag.** It would confuse an ordinary successful
  hook with tour completion and make replay a product-wide state mechanism.
- **Continue after hook failure.** It contradicts the owner's explicit failure
  gate and can launch the agent before prerequisites or instructions finish.

## Consequences

The platform supplies a small primary-launch sequencing mechanism, and the
terminal service applies it consistently across launch and revival. No stored
schema or harness manifest field is needed. The implementation updates the
terminal, harness and source-execution design documentation with the code;
this proposal leaves current-state design docs unchanged.

Repository code can now delay primary harness startup interactively. Recovery
through an auxiliary shell remains available, and authors must choose their own
repeat policy. The tour remains independently maintainable, while its promised
chapters must track the CLI and console's supported actions.
