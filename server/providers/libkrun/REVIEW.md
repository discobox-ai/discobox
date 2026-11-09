# libkrun Review Notes

- Classify every required BLK/NET export in missing-symbol diagnostics,
  including `krun_set_root_disk_remount`. Binding iterates a Go map, so any
  missing export can be the first failure; leaving one with generic version
  guidance makes feature-disabled builds fail with inconsistent advice.

- Recheck queued liveness and fresh log output before declaring a launcher
  inactive. A separate timer selected alongside a heartbeat or the console
  polling tick can otherwise kill a progressing boot at the deadline.

- Keep `CheckHost`'s error on one line and link the install instructions once.
  It is the reason the server prints when it falls back to Docker, and the CLI
  escapes a line break in a warning to a literal `\n`; `errors.Join` or a
  per-cause link brought both back once already.
