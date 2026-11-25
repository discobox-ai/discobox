# Harness Review Notes

These apply to every harness; a harness's own pitfalls are in its
`harness/<id>/REVIEW.md` ([codex-cli](codex-cli/REVIEW.md)).

- There is no setup step (ADR 26-11-02-152). A launcher must reach the CLI's
  sign-in with no credential delivered, and treat a missing credential file as
  signed out, never as an error: every box on a harness with nothing saved
  starts that way.
- Every launch-time import and handoff must consume the current credential
  document format. Changing export/installation without updating them can
  leave valid JSON objects where the vendor expects a token string and
  silently break a delivered login.
- A bundle declaring `harness.config` is refused at registration; do not
  reintroduce a setup command, configure ports, or a script that writes a
  harness config's result. A sign-in reaches the config only as a saved
  harness change.
- Hold `~/.config/discobox/harness-capture.lock` around every Discobox-owned
  read/modify/write of credential-bearing state, including startup initialization.
  Otherwise a stale initializer snapshot can restore a raw login after handoff
  has replaced it with an acknowledged sentinel, or lose concurrent trust edits.

- Pass host paths into POSIX test launchers as quoted data, not interpolated
  shell source. Windows backslashes are consumed as shell escapes, and spaces,
  quotes and dollar signs in a checkout path can change the command before the
  tested program sees it. Exercise a path with shell metacharacters locally.

- A settings key a harness CLI writes by itself, before any input, is local
  bookkeeping and goes in its manifest's `saveBack.settings` exclude. Otherwise
  every fresh box opens the save-back prompt (Codex: `tui.model_availability_nux`,
  then `tui.screen_reader_detection_done`). CLIs install at their latest
  release, so a new such key arrives with no change here. When touching a
  harness's settings, start a fresh box, give it no input, and check that
  `admin harness changes` reports `clean`.

- A launcher's resume must still open a session when there is none to
  resume, never fail to the login shell. A relaunch reaches a primary whose
  harness never got a message (a re-attach after a stop, the tour's hand-off);
  Claude Code's `--continue` exits 1 there (issue #1421), so its launcher checks
  for a conversation and otherwise starts fresh with the prompt, which was
  never sent. Check the vendor CLI's no-session behaviour when adding or
  changing a harness's resume, and record it in that harness's own
  `harness/<id>/DESIGN.md`, as each built-in's is.
