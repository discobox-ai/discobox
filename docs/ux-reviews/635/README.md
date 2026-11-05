# Sandbox approval forms: implementation evidence

Captured on Linux on 2026-11-05 against the running development server, Docker
pool and two disposable shell boxes. The build reports `cd958f25285b+dirty`:
the base commit plus this change's working tree. Console and TUI are the same
product surface. These are raw **live tmux pane captures**, not fixture
renderings, screenshots, or a simulation of server decisions. Every API/CLI
command explicitly targeted `http://127.0.0.1:8080`; commands inside the boxes
used their pool-authenticated `https://api.discobox.internal` endpoint.

The original forms and CLI output are recorded in [the issue's triage](https://github.com/discobox-ai/discobox/issues/635#issuecomment-6643298336).
The owner-approved design is [draft reference PR #653](https://github.com/discobox-ai/discobox/pull/653).
No prototype commits or tagged sample entrypoints were merged into this work.

## Observed flows

| Flow | Live captures | Persisted observation |
| --- | --- | --- |
| API access | [80×24](captures/api-final-80x24.txt), [150×45](captures/api-final-150x45.txt), [reviewed summary](captures/api-reviewed-80x24.txt), [CLI](captures/api-cli.txt) | A box with access off filed its own request. Enter before reading did nothing. After scrolling, mouse Accept completed the waiting command and enabled discovery. Disabling the setting immediately refused discovery; a new request remained available and mouse Deny kept access off. [History](outcomes/api-consent-final.json), [denial](outcomes/api-denied.json), [revocation](captures/api-revoked.txt). |
| Settings | [Current access setting](captures/settings-120x40.txt) | The existing direct on/off setting remains. The request lifecycle changes that same setting. It introduces no configurable API role. |
| Credential use/delegation | [Use](captures/use-reviewed-80x24.txt), [delegation](captures/delegation-80x24.txt), [independent scope](captures/delegation-anywhere-80x24.txt), [confirmation](captures/credential-confirm-80x24.txt), [CLI](captures/credential-cli.txt) | Accepting use left the real request pending until delegation and final confirmation. Credential/duration were locked on delegation. Final consent records use `routes`, delegation `anywhere`, and the independent secret ceiling. [Receipt](outcomes/credential-consent.json). |
| Duration picker | [Open at 80×24](captures/duration-open-80x24.txt) | Options are windowed; keyboard and wheel can reach all choices while decisions remain visible. Model tests also cover fifty credentials and clicking the last choice. |
| Forever addition | [Choice and preview](captures/addition-forever-150x45.txt), [held-grant confirmation](captures/addition-confirm-150x45.txt), [CLI receipt](captures/add-approved-cli.txt) | The real request was approved as `add`, with no expiry. The existing use binding remained beside the addition. [Receipt](outcomes/add-consent.json), [bound grants](outcomes/bound-after-add.json). |
| New credential | [Compact editor](captures/new-credential-form-80x24.txt), [masked token field](captures/new-credential-token-80x24.txt) | Created a dummy token through the console, using the mouse-clickable save hint. The viewport kept the active input and save controls inside 80×24. A real new secret and request grant were persisted. [Receipt](outcomes/new-consent.json). |
| Adoption | [80×24](captures/adoption-80x24.txt), [150×45](captures/adoption-150x45.txt), [read-only authority](captures/adoption-readonly-150x45.txt) | A sandbox-created child was released and adopted through mouse consent; creator stayed unchanged and parent became the requester. A second request with a restricted requester showed read-only authority for the wider-egress child and was denied. [Records](outcomes/adoption-consent.json). Historical adoption reads project current egress authority. |
| Host trust | [80×24](captures/trust-80x24.txt), [key selection](captures/trust-key-80x24.txt), [reviewed details](captures/trust-reviewed-80x24.txt), [CLI](captures/trust-cli.txt) | A local self-signed TLS server in the dev pool produced a real request. Changing the pin updated certificate details and reset review. Mouse acceptance persisted the selected public-key pin, finite expiry and sandbox scope. [Trust](outcomes/host-trusts.json). |
| Refresh | [80×24](captures/refresh-80x24.txt), [command/session permission](captures/refresh-command-session-150x45.txt), [masked manual entry](captures/refresh-manual-80x24.txt), [no saved command](captures/refresh-no-command-150x45.txt), [CLI](captures/refresh-cli.txt) | Expiring a bound dummy token and using it from the box created real requests. The saved `printf` command ran locally, with session permission automatically answering the next request. A fresh console asked again; manual entry was masked and persisted as `entered`. Removing the command offered manual entry only; Dismiss retained the value. [Decision events](outcomes/refresh-events.json). |
| Large route request | [Compact fallback](captures/large-route-fallback-initial-150x45.txt) | A real 50-host × 50-path request used faithful compact route notation instead of allocating 2,500 table rows. The console stayed responsive; the request was denied after reviewing it. Tests also cover 2,000 × 2,000 and long repeated protocol constraints. |

No real credentials were used. Dummy values were sent only to the requested
verification endpoints. Manually entered token values stayed masked in the console captures and are
absent from the decision records. The saved synthetic `printf dummy-renewed-635`
command and its dummy literal are intentionally visible in command-consent
captures and audit records, since the exact command must be shown before it
runs. No real credential value is present. The generated
TLS key was local test material and is not included. Both boxes, dummy secrets,
trust listener and isolated tmux server were removed after capture.

## Verification limits

Runtime verification was on Linux. macOS and Windows runtime behavior was not
exercised; the repository's cross-platform checks cover compilation. OAuth and
exchange-account renewal were not exercised against external accounts; the
shared refresh value/command paths and existing authorization checks remain.
The request protocol still commits combined credential answers atomically;
Accept on the first part is staged, which the delegation card explicitly says.

Review found and fixed picker overflow, an invalid-duration/JSON round trip,
missing server labels, stale agent/docs guidance, unbounded table expansion,
and field-viewport hover alignment. Runtime verification additionally found the
new-credential editor's compact overflow and drove its viewport fix.

The complete TUI and CLI package suites, server model/store/auth/handler/router
and SSH packages, `ci:check`, and repository hooks passed during implementation.
One unrelated intermittent shell Ctrl-V test timeout was recorded separately as
[#711](https://github.com/discobox-ai/discobox/issues/711); subsequent full TUI
runs passed. The final check results and CI head are recorded on the delivery PR.
