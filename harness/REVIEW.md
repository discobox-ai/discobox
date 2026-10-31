# Harness Review Notes

- Every configure seeding path must consume the current credential document
  format. Changing export/installation without updating a seed can leave valid
  JSON objects where the vendor expects a token string and silently break a
  reconfigure that should have kept a login.
- Hold `~/.config/discobox/harness-capture.lock` around every Discobox-owned
  read/modify/write of credential-bearing state, including startup initialization.
  Otherwise a stale initializer snapshot can restore a raw login after handoff
  has replaced it with an acknowledged sentinel, or lose concurrent trust edits.

- Pass host paths into POSIX test launchers as quoted data, not interpolated
  shell source. Windows backslashes are consumed as shell escapes, and spaces,
  quotes and dollar signs in a checkout path can change the command before the
  tested program sees it. Exercise a path with shell metacharacters locally.
