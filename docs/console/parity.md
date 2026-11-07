# Console parity ledger

Every console command is reachable in both consoles, the TUI and the desktop
app, each the way its platform expects (ADR 26-11-07-598 §1). A command one
console lacks is a bug, or an entry here with its reason and the issue that
closes it. An entry is removed in the change that closes the gap.

Both consoles run the shared parity scenarios
(`cli/internal/tui/testdata/parity/*.json`) in CI, each pressing its own
bindings: the TUI in `go tool task ci:test`, the desktop app in
`go tool task ci:desktop`, on Linux.

## What the ledger covers today

The commands the console's spec carries (`cli/internal/tui/spec.go`, exported
to `desktop/src/spec.json`), by their prototype IDs. When the command registry
in `cli/internal/console` replaces that spec, its commands are the ones held to
parity, and the entries below move to its IDs.

## The desktop app lacks

| Command | Reason | Closed by |
| --- | --- | --- |
| `box.new` | Creating a box needs the create flow, which the app does not have yet; it says so. | #810 |
| `box.rename` | Needs a form, which the app does not have yet; it says so. | #808 |
| `box.settings` | Needs a form, which the app does not have yet; it says so. | #808 |
| `box.purge` | Needs a confirmation, which the app does not have yet; it says so. | #808 |
| `box.reviewConfig` | Reviewing a box's harness changes needs that review page. | #812 |

## The TUI lacks

Nothing.
