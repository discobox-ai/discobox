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

Moving between panes and boxes is navigation, which each console owns
(ADR 26-11-07-876 §2), and is not held here.

## The desktop app lacks

| Command | Reason | Closed by |
| --- | --- | --- |
| `review config` | Reviewing a box's harness changes needs that review page. | #812 |
| `rename` | Needs a form, which the app does not have yet; it says so. | #808 |
| `settings` | Needs a form, which the app does not have yet; it says so. | #808 |
| `purge` | Needs a confirmation, which the app does not have yet; it says so. | #808 |
| `new` | Creating a box needs the create flow, which the app does not have yet; it says so. | #810 |
| `options` | The run options are part of the create flow. | #810 |
| `tools` | Needs the tools picker and tool panes. | #815 |
| `audit` | Needs the audit view. | #816 |
| `issue` | Needs the issue tab. | #817 |
| `pr` | Needs the pull request tab. | #817 |
| `files` | Needs the pull request's files-changed review. | #817 |
| `services` | Needs service panes and the services menu. | #809 |
| `credentials` | Needs the secrets page. | #813 |
| `harnesses` | Needs the harnesses page. | #812 |
| `requests` | Needs the credential requests page. | #814 |
| `rejections` | Needs the rejected credentials page. | #814 |
| `dismiss` | The app has no notices to dismiss until it raises requests as notifications. | #814 |
| `refresh` | The listing is pushed every two seconds; reading it again on demand comes with the list. | #804 |
| `servers` | Choosing the server, folder or tags is the list's view filters. | #804 |
| `archived` | Showing archived boxes is the list's. | #804 |
| `all` | The sidebar always lists every box; the command is for a window opened on one box, which comes with the list and sidebar. | #804 |
| `actions` | A box's action menu opens from its row's context menu; the command and its shortcut come with the command surfaces. | #807 |
| `help` | The help comes with the command surfaces, generated from the registry. | #807 |
| `mouse` | Mouse reporting to the program is terminal fidelity. | #806 |
| `repaint` | Repainting after a size loss is terminal fidelity. | #806 |

## The TUI lacks

Nothing.
