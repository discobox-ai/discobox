# Release UX walk — #582

Walked 2026-11-04 against the real development CLI, server, Docker pool and
three disposable boxes. Baseline: `b70abf7ff4d3` (adoption already merged).
Baseline console captures report `+dirty` because regression tests had been
added; product code was still unchanged. After captures use that commit plus
this PR's product changes. All calls explicitly targeted the dev server on
127.0.0.1:8080, never the control plane hosting this review box.

The [discovery captures](https://github.com/discobox-ai/discobox/issues/582#issuecomment-6632724422)
and [initial live walk](https://github.com/discobox-ai/discobox/issues/582#issuecomment-6632957941)
were posted before product changes. The owner has **not signed off** these
captures. This document records evidence, not release approval.

## Findings

| Finding | Disposition |
| --- | --- |
| Welcome teaches bare `discobox`, which now prints help | Teach `discobox console`; fix the project help too |
| Adoption inbox says “a credential”; workspace/error says “credential request” | Name adoption and its child consistently; use ID when unnamed |
| Read-only adoption first promises write capabilities | Show only effective read authority and explain wider egress |
| Adopt/release help omits prerequisites, approval and worked examples | Explain API access, the person's inbox, stopping the wait, parent replacement, limits and release |
| Real-box UX tests are opt-in; CI does not run them | Split into [#587](https://github.com/discobox-ai/discobox/issues/587) |
| Exec/capture on a cold, still-provisioning box reports “sandbox exec not found” | Split into [#592](https://github.com/discobox-ai/discobox/issues/592) |

## Captures

PNG files render the cells recorded by tmux `capture-pane -p -e`; matching
`.ansi` files preserve the text and SGR attributes. These are terminal captures,
not generated UI mockups. The standard terminal was 150×48; the small approval
capture was 80×24. Baseline console windows inherited `NO_COLOR=1`; colored
after captures explicitly unset it and set `COLORTERM=truecolor`. The palette
was not changed by this PR. PNGs use a dark background and a monospace font;
nonprinting hyperlink sequences are omitted from the PNG rendering.

### Discovery and onboarding

[Full help text and subcommand captures on the issue](https://github.com/discobox-ai/discobox/issues/582#issuecomment-6632724422).

![Grouped help with color always](before/help.png)

| Before: wrong onboarding command | After: opens the console |
| --- | --- |
| ![Welcome before](before/welcome.png) | ![Welcome after](after/welcome.png) |

[Adopt help after polish](after/adopt.png) · [Release help](after/release.txt).

### Terminal commands

A real shell box received `send` followed by `send-key Enter`. Waiting for
`idle=1s` returned, and [capture showed the marker](before/terminal.txt).
A [finished wait with a one-second budget](before/wait-timeout.txt) exited 124.
`exec` ran commands in both the shell box and the provisioned Claude box.

### Harness setup, save and reset

A separate harness config copied the built-in Claude bundle, so this walk
never reset the built-in harness. The box started signed out and displayed
[the setup screen](before/harness-setup.txt) and login chooser. **An external
account login and sharing real credentials were not exercised.** Synthetic
settings changes exercised the same candidate/review/save path without a login.

![Signed-out harness and login chooser](before/harness-login.png)

![Harnesses screen](before/harnesses.png)

![First-time save explanation](before/harness-explainer.png)

![Green changes band with color enabled](after/harness-band.png)

![Settings review with color enabled](after/harness-review.png)

The initial settings candidate was discarded; a subsequent edit produced a
new candidate. Saving the displayed candidate succeeded. Reset then returned
the isolated config to its bundle defaults. A personal file was added; plain
reset kept it in `fileOverrides`, and `--include-overrides` removed it.
No settings or credentials from an external account were used.

![Plain reset confirmation](before/reset.png)

![Reset choices when personal files exist, captured on the final build](after/reset-overrides.png)

### Adoption, approval, denial and release

The requesting shell box had `--discobox-api`; a second shell box was created
by the person, not by that requester. The requester used this checkout's CLI
copied into `/tmp` in the box. The person's console answered its requests.

| Before: generic credential label | After: adoption and child name |
| --- | --- |
| ![Inbox before](before/adoption-inbox.png) | ![Inbox after](after/adoption-inbox.png) |

| Before: credential banner | After: adoption banner |
| --- | --- |
| ![Banner before](before/adoption-banner.png) | ![Banner after](after/adoption-banner.png) |

![Approval names requester, child and current parent](before/adoption-approval.png)

![80×24 approval keeps actions visible and advertises scrolling](before/adoption-80x24.png)

The first approval returned “Now leading” and [the parent filter listed the
child](before/adoption-parent.txt). [Release succeeded](before/adoption-release.txt)
and the filter became empty. A second request was denied; [the requester's
terminal records both outcomes](before/adoption-cli.txt). Esc leaves a request
pending. Restricting the requester's egress then produced a real read-only
request, allowing the authority wording to be compared on both builds.

| Before: promises writes, then refuses them | After: read authority only |
| --- | --- |
| ![Read-only before](before/adoption-read-only.png) | ![Read-only after](after/adoption-read-only.png) |

Archiving the child while the last request waited made approval fail with a
409. The error card identified the adoption, and the request stayed pending.

![Failed approval identifies the adoption that is still waiting](after/adoption-error.png)

## Automated coverage

These model, render, output and HTTP adapter tests run in the existing
`ci:test` → `test:all` CLI module tests. The real-box CI gap remains #587;
manual observations above do not replace that follow-up.

| Flow | Coverage |
| --- | --- |
| Grouped help, color, aliases, command discovery | `help_test.go` |
| Send/key input, capture/trim, wait resume, timeout 124 | `terminal_drive_test.go` |
| Exec command/output | `exec_test.go` |
| Harnesses, reset and preserving overrides | `harnesses_test.go`, `tui_harnesses_test.go` |
| Changes band, first-time explainer, exact displayed candidate save, discard, failures, scrolling | `harness_changes_test.go`, `tui_harness_changes_test.go` |
| Correct welcome command | `welcome_test.go` |
| Adoption help, required reason, pending→approved/denied, release request/output | `adoption_test.go` |
| Adoption inbox/band/error labels, effective authority, approve/deny on correct server, Esc, 80×24 scrolling/actions | `adoptions_test.go` |
| Parent/creator, decisions/events, cycle and project limits, release | `server/internal/store/adoptions_test.go` |

The owner still needs to review the captures and sign off. No experimental VM
driver was included in this walk.
