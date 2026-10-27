# 26-10-02-054 — Commands are judged by default, and requests by opt-in

- **Status**: Superseded by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
- **§§1–3 superseded for commands by**: [26-10-20-782](26-10-20-782-approved-access-is-a-policy-decided-at-the-network-boundary-fast-path-first.md) — `judgeCommands` goes; judging a delegation stays
- **Date**: 2026-10-02
- **Relates to**: [ADR 26-09-22-838](26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md)
  §3, which moves the command judge out of the sandbox and which this settles
  the switch for; [ADR 26-10-01-324](26-10-01-324-a-server-may-judge-with-jev-instead-of-a-judge-discobox.md)
  §1, whose backend setting both switches share; and
  [ADR 0091](0091-a-credential-is-not-issued-without-a-verdict-on-record.md),
  which a server that turns command judging off opts out of.

## Context

A server judges credential use only when `judgeCredentials` is on, and it is
off by default. That was harmless while the command judge ran in the sandbox:
`discobox-access run` asked its own `discobox-prompt` on every server, whatever
the setting said, so every credentialed command was judged, if only by the
sandbox's word about itself.

ADR 26-09-22-838 §3 moves that judge to the control plane. One switch would then
judge both commands and requests, or neither. Off by default, the move would
take command judging away from every server that never opted in. On by default,
it would hold every credential-bearing request open for a model on servers
that never asked for that.

The two are different costs:

- **A command** is judged once per `run`, before anything is minted. A slow
  verdict delays one command an agent chose to run.
- **A request** is judged at the proxy, on every request that carries a
  credential. A slow verdict holds a connection open, and a busy agent sends
  many requests per command.

## Decision

### 1. Two switches

- **`judgeCommands`** (`DISCOBOX_JUDGE_COMMANDS`), **on by default**, judges
  the command `discobox-access run` declares before the pool mints a sentinel
  for it. It also judges a discobox handing a credential on (ADR 26-09-30-782
  §3). That job is also put before anything is minted, and it is equally rare.
- **`judgeCredentials`** (`DISCOBOX_JUDGE_CREDENTIALS`), **off by default**,
  now judges only the requests the proxy observes. It keeps its key, so a server
  that opted in keeps the judging it asked for, plus commands.

`judgeBackend`, `jevApiKey`, `jevModel`, and `jevUnsure` serve both. With no Jev
key, `auto` resolves to `harness`, so by default every project keeps a judge
discobox for its commands.

A project keeps a judge discobox whenever either switch needs one: either is
on, and the backend is `harness` or sends what Jev is unsure of to one.

### 2. With command judging on, nothing is minted without a trusted allow

The pool asks the control plane and mints only on an explicit allow, as
ADR 26-09-22-838 §3 says. A project with no judge cannot run a credentialed
command, because it has no default harness or its default harness runs no
model. The refusal names the reason and the two ways out: configure a
default harness that runs a model, or turn `judgeCommands` off.

The `shell` harness's stand-in wrapper goes. It was the only way a shell
discobox could take a value, by setting `DISCOBOX_SHELL_JUDGE=allow` in the
discobox it gated. That is a sandbox approving itself. The server-wide switch
replaces it.

### 3. With command judging off, a command is not judged at all

The pool mints without asking, and records no verdict, because nothing
decided anything. That is ADR 0091's rule not holding, by the operator's
choice. The proxy still holds every sentinel to its grant's host and its
activation's window, and the proxy's audit trail still records every swap. A
delegation is refused, as it is on a server that judges nothing today, because
handing a credential on is never done without a judge.

## Alternatives rejected

- **One switch, off by default.** Every server that never opted in loses
  command judging, the one judging it had. That is the default install.
- **One switch, on by default.** Every credential-bearing request on every
  server waits on a model. That is a cost in latency and model spend that no
  operator asked for, and it would be an outage for a server whose project has
  no judge.
- **Fall back to the in-sandbox judge when the server does not judge
  commands.** It keeps `discobox-prompt` in the CLI, and the decision on the
  sandbox's side, for the case it is least trustworthy in. ADR 26-09-22-838
  §3 adds no unjudged path for old clients, and this would be one with extra
  steps.
- **Rename `judgeCredentials` to `judgeRequests`.** A configuration file that
  names a key nothing defines is refused at startup (ADR 0096). Renaming would
  turn every server that opted in into one that does not start.
- **Delegation on the request switch.** Delegation is judged before a grant is
  minted, at the rate of approvals, and holds no request open. It belongs with
  the switch that is on by default. Without a judge, handing a credential on
  stays refused.

## Consequences

- A default server runs a judge discobox per project with a configured default
  harness, and every credentialed command costs one verdict from it.
- A project whose default harness is `shell`, or that has none, can run no
  credentialed command until it configures one, or until the server sets a Jev
  key or turns `judgeCommands` off.
- A server with `judgeCommands: false` issues sentinels with no verdict on
  record. A verdict's absence is not an anomaly there.
