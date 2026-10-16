# 26-10-16-965 — The omp judge answers with thinking off, and configure proves it can

- **Status**: Accepted
- **Date**: 2026-10-16
- **Supersedes**: [26-10-14-718](26-10-14-718-the-omp-harness-imports-its-credentials-and-publishes-through-an-extension.md)
  §5's "The judge reasons as its model does by default"

## Context

ADR 26-10-14-718 §5 left omp's judge at its model's default thinking level,
on the grounds that omp's levels are per model, some offer no `off`, and a
level the model does not take would fail every judged command. Read against
omp 18.11.1's source, that is not how omp behaves:

- `off` is a level the CLI accepts for every model (`--thinking`, which also
  takes the `--thinking=off` form), and it outranks a `:level` suffix on the
  `--model` value.
- A model that must reason (`thinking.requiresEffort` in omp's catalog, such
  as OpenAI's `o3`) is not sent `off`: omp raises it to the lowest level the
  model offers before the request goes out (`normalizeMandatoryReasoningOptions`).
- With no `--thinking`, omp's default level is `high`. The tool-free judge
  runs in a home of its own, where no setting lowers it, so every judged
  command reasoned at `high`. Measured on `claude-code`, thinking before the
  verdict costs 5 to 50 seconds of a request held open (`harness/DESIGN.md`).

What the source cannot show is how a provider answers the level omp sends
for a model it does not know as well as its catalog claims, nor how a
provider reached through a user's own sign-in answers it. omp's providers
are the user's choice, so no one model tested here stands for them.

## Decision

1. **The judge answers with thinking off.** `discobox-prompt --model judge`
   passes `--thinking=off`, whatever model it resolves and whatever level
   that model's name carries. The verdict is a few dozen tokens, and how the
   gate thinks is the wrapper's to decide, not the settings', which the
   judged agent can write. `fast` and an unnamed role keep omp's own level.
2. **Configure asks the judge one question before it saves.** After the
   default-model check, `configure-omp` runs the image's own
   `discobox-prompt --model judge --no-tools` in a home holding what a
   sandbox's would — the exported credentials, omp's settings and Discobox's
   — so the judge's model, its thinking level, its isolation and the user's
   real sign-ins are all exercised together. A judge that does not answer is
   reported with omp's reason, the model it ran on, and the command that
   edits `judgeModel` after saving — no omp session can change Discobox's
   own setting — beside the offer to start omp again for another default
   model or sign-in, as a failed default model is; declining saves
   everything as it is.

## Rejected alternatives

- **Leaving thinking at the model's default** (26-10-14-718 §5). It was
  chosen for a failure omp does not have, and costs reasoning at `high` on
  every judged command.
- **A one-off live test of a mandatory-reasoning model, and nothing in
  configure.** It would prove one provider's answer on one day. The check in
  configure proves the judge each user will actually run, with their
  credentials, every time the harness is configured.
- **Falling back to the model's default level when `off` fails.** The judge
  would then think, slowly, wherever `off` fails, and the user would learn of
  it only as commands that take a minute to be allowed. A judge model that
  will not answer with thinking off is a choice to change at configure, where
  the failure names it.

## Consequences

- A harness configured before this change is not re-checked: a judge model
  that refuses `off` refuses every judged command until the harness is
  configured again.
- Configure makes one more model call, on the judge's model.
- The check runs the judge's isolation, so a `judgeModel` it cannot reach — a
  custom provider in `models.yml`, a provider that needs an endpoint from its
  credential (26-10-14-718's consequences) — is now reported at configure
  rather than discovered in a sandbox.
