# Approval form design workbench

Run from the repository root, in the development shell:

```sh
go tool task prototype:approvals
```

This is an interactive **design sketch**, using the console's actual dialog,
form, theme, focus and picker renderers. It starts with credential use, the
first screen under discussion. No design has been accepted yet.

- F2 switches between the current initial credential picker and the proposed card.
- Arrows move and change choices; Enter opens a picker; Ctrl-S submits the sketch.
- F5 resets; q or Ctrl-C quits. Resize the terminal to try narrower layouts.
- Every action shows a sample-only receipt. There is no API connection.

The current screen uses the actual `askAboutCredential` builder, with its action
callback replaced. Its later wizard screens are not demonstrated. The proposed
screen uses the existing form component, so Approve/Deny is a decision picker
rather than the separate buttons in the original chat sketch. New credential
creation is a placeholder. All names, routes and durations are fixtures.

Export an ANSI frame for sharing, including colors and the focused row:

```sh
go tool task prototype:approvals -- --capture --width 100 --height 36 > /tmp/approval-proposed.ansi
go tool task prototype:approvals -- --capture --current --width 100 --height 36 > /tmp/approval-current.ansi
```

Open the ANSI files with `cat` in a terminal. For issue/PR evidence, attach a
terminal screenshot or recording and link this runnable source at its commit.
The preview needs the repository and Go toolchain; it is not a standalone snippet.
No GitHub attachment or publication is performed by this workbench.

The preview code is gated by `-tags prototype`; it is excluded from normal builds.
