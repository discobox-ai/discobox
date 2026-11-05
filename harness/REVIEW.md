# Harness Review Notes

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

- Prove Codex folder trust with an untrusted control against the pinned
  binary, without explicit approval/sandbox policy masking onboarding.
  A prompt reached with those policies alone does not prove folder trust.
  Check direct starts after the launcher and retain earlier system trust
  entries when a later launch runs in a different folder.
