# Console parity ledger

Every console command is reachable in both consoles, the TUI and the desktop
app, each the way its platform expects (ADR 26-11-07-598 §1). A command one
console lacks is a bug, or an entry here with its reason and the issue that
closes it. An entry is removed in the change that closes the gap.

Both consoles run the shared parity scenarios
(`cli/internal/console/testdata/parity/*.json`) in CI, each pressing its own
bindings: the TUI in `go tool task ci:test`, the desktop app in
`go tool task ci:desktop`, on Linux.

## What the ledger covers

The commands of the console core's registry (`cli/internal/console/commands.go`),
by their registry IDs, which the core's spec hands to the desktop app
(`cli/internal/console/spec.go`, exported to `desktop/src/spec.json`). The
desktop app's test `every_registry_command_is_answered_or_in_the_ledger`
fails when a registry command is neither answered by the app nor listed
below, and when an entry here names a command the app answers.

Moving between panes and boxes is in the registry too (`next pane`,
`previous box`, `sidebar`): where focus goes is each console's own (ADR
26-11-07-876 §2), but the commands and their keys are the registry's, and
held here like any other. Gestures (the list's cursor, a pane by its number)
are declared in the registry with each console's keys, and are not commands.

Off macOS the desktop's menu bar is drawn in the window from the registry
(each command's desktop `menu`), so every command is in a menu as well as the
palette. On macOS the native menu bar is still a hand list of some commands
(`MENU` in `desktop/src/main.rs`), until #908 builds it from the registry.

## The desktop app lacks

| Command | Reason | Closed by |
| --- | --- | --- |
| `tools` | Needs the tools picker and tool panes. | #815 |
| `audit` | Needs the audit view. | #816 |
| `issue` | Needs the issue tab. | #817 |
| `pr` | Needs the pull request tab. | #817 |
| `files` | Needs the pull request's files-changed review. | #817 |
| `credentials` | Needs the secrets page. | #813 |
| `requests` | Needs the credential requests page. | #814 |
| `rejections` | Needs the rejected credentials page. | #814 |
| `dismiss` | The app has no notices to dismiss until it raises requests as notifications. | #814 |
| `all` | The command is for a window opened on one box, which the desktop has none of: its list is always beside the workspace, and "all discoboxes" in the sidebar is `detach`. | None: by design |
| `mouse` | Mouse reporting to the program is terminal fidelity. | #806 |
| `repaint` | Repainting after a size loss is terminal fidelity. | #806 |

## The TUI lacks

Nothing.
