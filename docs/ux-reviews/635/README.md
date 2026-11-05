# Sandbox approval forms: design reference for #635

This is a runnable design prototype, not the implementation of #635. It uses
sample requests and the real console dialog, form, theme and route renderers.
It never connects to an API or grants access.

## Run

From the repository root in the development shell:

```sh
go tool task prototype:approvals:manager
```

Starts on **CURRENT**, reproducing the manager's combined request. F2 switches
to **PROPOSED**; F5 resets; q or Ctrl-C quits. Resize the terminal to explore
wrapping. PgUp/PgDn scrolls the upper list. Tab/arrows move between fields and
buttons, Enter opens a picker or activates the focused Accept/Deny button.

For a simple use-only fixture:

```sh
go tool task prototype:approvals
```

Capture ANSI output, with color and focus:

```sh
go tool task prototype:approvals:manager -- --capture --width 130 --height 45 > /tmp/approval-current.ansi
go tool task prototype:approvals -- --request docs/ux-reviews/635/manager/roles/manager-request.json --policy-root docs/ux-reviews/635/manager --capture --width 130 --height 45 > /tmp/approval-proposed.ansi
```

View with `cat` in a terminal. Screenshots/recordings and a link to this source
can accompany implementation discussions. The program requires this repository
and its Go toolchain; it is not a standalone snippet.

## Design decisions

- Use and delegation are separate screens with independent Accept/Deny buttons.
  Explain the permission on each screen and show only that part's access.
- Accepting use locks credential and duration on delegation. Denying use leaves
  both editable on delegation. Scope is always independent per part and starts
  at Requested routes.
- Group routes by HTTP, GraphQL, MCP, then alphabetically by host. Each group
  starts with one line such as `HTTP api.github.com`, followed immediately by
  its table header.
- Inside each host, sort by path, then method. Expand multiple host/path pairs
  to separate display rows. Preserve method lists and protocol restrictions.
- HTTP tables show method/path. GraphQL tables show path/operation/fields; MCP
  tables show path/tools. Show HTTP method restrictions in protocol tables when
  present; omit the method column when all are unrestricted.
- Mark deny rows with an explicit DENY label and alert color. Sorting must not
  change authorization semantics or merge independent route lists/grants.
- Scope changes update the upper grant-restriction preview immediately and
  reset scrolling. Requested routes retains the requested constraints;
  Requested hosts permits any method/path/protocol on open requested host
  patterns; Anywhere adds no binding route restriction. Credential and parent
  ceilings remain separate and visible in the explanation.
- Offer Forever duration (including additions; PR #650). The three scope
  choices follow PR #638/#629 and do not widen credential or parent ceilings.

## Required for the implementation, deliberately not built here

- **Every control must be mouse clickable:** fields, picker choices, scope
  choices and Accept/Deny. Keyboard and mouse must perform the same actions.
- **If the upper list requires scrolling, disable both Accept and Deny until
  the user has scrolled to the end of the entire list.** Clearly explain why
  the buttons are disabled. This establishes that the end was reached; it
  cannot prove the contents were read. Re-evaluate the gate for the current
  screen/list when scope or other choices change its content.
- Preserve live credential/parent ceilings, maximum lifetimes, add/replace
  behavior and replacement consequences, raw delivery and independent part
  decisions. This prototype does not demonstrate those full flows.
- Wire real request data and server decisions, including scope and historical
  consent. The current backend expects one answer covering the request;
  this prototype's two visual steps do not decide transaction timing.
- Implement creation of a new credential; its preview option is a placeholder.
- Review the new API-access proposal with the owner. Credential use/delegation,
  adoption, host trust and refresh layouts have been accepted.

## Fixture and limitations

The manager fixture copies `discobox-ai/manager/roles/manager-request.json`
and its role documents, fetched through GitHub's Contents API on 2026-11-05.
`manager/source.json` records source blob SHAs. The request has 8 use and 21
delegation policy references, resolving to 52 distinct use and 77 delegation
routes. Policy resolution uses `credentialpolicy.ResolveDocument` and the
server's normalization/deduplication rule. The transport-only `wait` field is
ignored. Box identity, age and available credentials are illustrative.

CURRENT uses the actual `askAboutCredential` builder with its side effects
replaced; later wizard screens are not included. PROPOSED has sample-only
receipts. Narrow tables still wrap. Scope previews show the binding restriction,
not a calculated effective union of credential/parent routes.

Preview source is gated by `-tags prototype`. The shared dialog's explicit
form-button support is enabled only by the preview; ordinary forms keep their
controls. The default go-lsp hook does not understand the preview tag and reports
an unresolved preview entrypoint; the tagged binary builds and runs. Full SDLC
review and runtime verification of real approvals were intentionally skipped.

## Adoption (layout accepted by owner)

```sh
go tool task prototype:approvals:adoption
```

Starts CURRENT; F2 compares the proposed screen. F3 switches between full and
read-only authority in either layout. Tab/arrows choose Accept/Deny; Enter
activates; Esc leaves the sample waiting. No real relationship changes occur.

The proposal identifies requesting box, target box, current parent and the
box-supplied reason first. Full authority is an aligned table of terminals,
lifecycle, metadata/source, ports and bounded credential decisions. A separate
consequence section explicitly names the parent being replaced and says the
child's credentials stay its own. The read-only variant instead explains wider
egress and refusal of all writes/sends, without promising management authority.

The original screen comes from `askAboutAdoption`, with its action callback
replaced. The new design uses the same themed form/dialog components and modal
buttons as credential approval. The owner accepted this layout. The mouse and scroll-to-end requirements
above also apply here.

## Host trust (layout accepted by owner)

```sh
go tool task prototype:approvals:trust
```

F2 compares current/proposed. The current initial card comes from askAboutTrust;
its action callback is replaced and the later lifetime wizard is not included.
The proposal combines pin and duration on one card. Host, box and box-supplied
reason appear first, followed by dynamically selected certificate details
(subject, issuer, validity and full CA or public-key SHA-256). The pin picker
uses the real trustPinOptions ordering and server default. Changing it updates
the displayed certificate. Duration is finite (1h/day/week/month); trust has no
Forever option. Both decisions use the established modal buttons.

The scope is explicit: this host and box only. The CA choice trusts a verifying
chain to that CA; the host-key choice is tied to its public key and breaks on
key rotation. Denial leaves the host refused; expiration requires trust again.
All data and fingerprints are synthetic, and no host is contacted. The owner accepted this layout. Mouse and scroll-gate requirements apply.

## Credential refresh (layout accepted by owner)

```sh
go tool task prototype:approvals:refresh
```

F2 compares current/proposed; F3 switches expired/rejected causes. The original
initial screen comes from askAboutRefresh with its callback replaced. The new
single card identifies box/credential/reason, destination and value lifetime.
Choose Run saved command or Paste a value. For command renewal, display the
exact command and explain that it runs locally as the person, without a shell;
permission is once or while this console window stays open. The upper summary
tracks those choices. Manual entry is masked and command permission is dimmed.

Use Refresh/Dismiss modal buttons: dismissal keeps the existing value and may
prompt again later. Refresh changes a project token value, not existing grant
routes/lifetimes. No command executes and no token is sent. Use dummy text only.
The program never prints manual input in its receipt and clears it on submit.
The fixture includes a saved command; no-command and OAuth/exchange-specific
renewal flows remain implementation work. Mouse and scroll-gate requirements
apply. The owner accepted this layout.

## Final batch: API access and remaining variants

```sh
go tool task prototype:approvals:api
```

F2 compares the actual settingsDialog (the current API toggle) with a proposed
API permission card. There is no structured API request endpoint today: the
new request reason is illustrative, and transaction/intake design remains open.
The proposed card covers project discovery/creation and bounded management of
led boxes, with explicit credential/egress ceilings and persistent on/off state.
F3 toggles between enabling and turning access off, with consequences and modal
buttons. This proposal awaits owner review. No setting changes occur.

Additional variants, in either CURRENT or PROPOSED:

- Adoption F3: full/read-only authority.
- Trust F3: certificate-chain CA/agent-supplied CA. Pin choices still include
  the chain CA and host key; the real default ordering is retained.
- Refresh F3: expired/rejected; F4: saved command/no saved command. Without a
  command, only masked manual entry is offered.
- API F3: enable/disable.

Add `-- --compact` to any task to constrain its renderer to 80x24, even in a
larger terminal. All proposed cards were rendered at that size and their upper
content scrolls while controls remain visible. Long table rows still wrap. The
preserved CURRENT Settings/API screen needs 25 lines at this size; it is a
baseline limitation, not new intended behavior. Captures do not exercise mouse
interaction or enforce the deferred scroll-to-end gate.

### Commands for the complete review

```sh
go tool task prototype:approvals              # simple credential use
go tool task prototype:approvals:manager      # use, then delegation
go tool task prototype:approvals:adoption
go tool task prototype:approvals:trust
go tool task prototype:approvals:refresh
go tool task prototype:approvals:api
```

All named category tasks start CURRENT; F2 shows PROPOSED. The simple credential
preview starts PROPOSED. Example narrow-screen command:

```sh
go tool task prototype:approvals:api -- --compact
```
