# 26-11-18-174 — Live provider checks run on an interval and at release, in a box the manager launches with test keys

- **Status**: Accepted
- **Date**: 2026-11-18
- Accepted by the owner by merging #1382, 2026-11-19
- **Replaces** (not an ADR): the remote-drivers plan's
  (`docs/tasks/947-remote-drivers-boxd-and-archil.md`) Q1 answer, "the live
  run happens once … it is not automated", and three of its
  [Credentials](../tasks/947-remote-drivers-boxd-and-archil.md#credentials)
  rules for the keys this ADR names: "no agent while the key is there",
  "egress narrowed before the key", and "the manager hands a key out only for
  the one live run". It is the plan's row R5 (#1047).

## Context

A remote driver (boxd today, archil once
[26-11-16-619](26-11-16-619-a-pool-on-a-driver-with-no-networks-fences-each-sandbox-at-the-providers-edge-and-starts-systemd-in-the-guest.md)
lands) can only be shown working against the provider itself. Nothing on
the fake APIs catches a provider changing under us. B6's live runs on boxd
(#1050, 2026-11-14 to 15) found nine bugs in three sessions, none of which a
fake run had shown: #1197, #1198, #1199, #1200, #1214, #1215, #1216, #1217
and #1218.

Each of those sessions was assembled by hand:
- the manager made a one-off box and briefed it;
- the owner approved the key going in;
- the owner pasted a temporary harness token at the box's prompt;
- an agent ran `scripts/live-run.sh` and did by hand the steps the script
  lacks (snapshot and second box, sentinels, the model's answer).

None of that repeats unless a person starts it, so a regression found by a
release's users goes unseen until someone does.

What B6 settled about what a run needs:
- **The provider key** goes to the box as a raw binding
  ([26-10-27-840](26-10-27-840-a-binding-may-hand-over-the-raw-value-held-in-memory-and-watched-for-at-the-proxy.md)).
  boxd's token exchange carries it in a body (`roles/boxd-key.json`: `boxd.sh`,
  `*.boxd.sh`, and the body place `/api_key`). The value never reaches the
  agent's context, the box's audit or a command line.
- **The run's server** is the box's own, from the pinned build, on a local
  socket (the `scripts/live-run.sh` header). The outer server never gets the
  key.
- **A harness credential** must be a project secret on the run's server. A
  box's own harness login is a sentinel that only the box's egress swaps, and
  a boxd sandbox's egress leaves from boxd (#1050, 2026-11-14 and 15).
- **A harness secret bound alone may not log the harness in.** For Claude
  Code's OAuth token, the harness config also needed a credential document
  (#1050, "Snag 1").
- **Teardown is checkable.** `ListVms` and `ListSnapshots` on every region
  endpoint read 0. The script's `left-pool-scope` and `left-provider-scope`
  checks read empty.

## Decision

### 1. One provider test procedure, versioned in this repository

`scripts/live-run.sh` (`go tool task live-run`) is **the** provider test
procedure. It runs per driver and lands through review like any code. It is
the only thing a live run executes against a provider. It grows to cover
everything B6 did by hand, so that a run with no person present checks all of
it:

| Check | Passes when |
|---|---|
| preflight | tools, the run's server, the provider's reachability (`admin provider check`), and the harness and its credential are present |
| pool | the pool is `active` within its timeout |
| provision | a box running the harness reaches `running` |
| harness | the harness answers one prompt (`claude -p` with a fixed prompt) |
| egress | the proxy answers for the allowed host; an unlisted host and a bare IP do not; nothing is reached around the proxy, judged as #1215 settles |
| sentinels | a test secret is a sentinel in the box and in a box from the snapshot, and its value is on neither box nor the pool machine; the pool machine holds no provider key |
| ingress | the driver reports no ingress URL for a sandbox machine, and its default proxy does not answer as the guest |
| snapshot | a snapshot build succeeds, and a second box starts from it |
| attach | attach streams the terminal and ends when told to |
| channel | the control-plane channel's cuts and redials over the run are counted and reported (#1218) |
| region | a provider with a region makes its machines there |
| teardown | everything the run made is gone, read back per region endpoint |

Adding a check here is how a found bug gets a regression test. When a run's
finding is fixed, the fix adds or tightens the check that would have caught
it.

### 2. Two triggers: an interval, and release validation

- **On an interval**, against the head of `main`, for every driver whose
  pools are accepted. The manager owns the schedule. The cadence is the
  manager's configuration, not this ADR's (Open).
- **At release validation**, against the release's tagged commit. The run is
  part of validating that release, and its verdict is part of the release's
  verdict on its tracking issue.

Never on a pull request: a run only uses code that has landed through review.

### 3. The manager holds the keys, and they are dedicated to testing

The manager's credential store holds, for each driver, one provider key
**that is used for nothing but these runs**:
- **boxd:** a key of an org dedicated to testing, with a payment method or a
  quota for the run's machines. Neither the org nor the key's owner holds
  host-bound secrets, since a boxd machine reaches those hosts whatever its
  fence says (the plan's "Other facts").
- **archil:** a key of an account that holds nothing else.

It also holds **one Anthropic API key used for nothing else**, with a spend
limit set at Anthropic, for the harness check.

A key never enters the manager's agent context. It goes to a validation box
only as a raw binding, routed to its provider's hosts alone, with the body
places its driver needs (`roles/boxd-key.json` for boxd). The owner rotates
these keys; a key that leaks is revoked, and the dedicated account bounds
what it reached.

### 4. The manager launches a validation box, and an agent drives it

For each run the manager launches a fresh box:
- **pinned** to the run's commit (`main`'s head, or the release's tag);
- **bound** to the driver's key and the harness key, raw, each held to its
  own hosts;
- **given** a GitHub credential that can comment on and file issues in this
  repository, as a worker's;
- **running** one narrow skill, `validate-provider` (in `discobox-ai/skills`,
  alongside `validate-release`).

The skill builds the pinned revision (`live-run:build`), starts the box's own
server, and sets the harness key as a project secret there:
- routed to the harness's own hosts, and no `*`;
- bound to the harness;
- with whatever the harness needs to log in from that secret alone (Open).

It then runs §1's procedure, reports (§5), deletes the harness secret, tears
down, and ends. The manager archives the box once it is `ready` or
`blocked`.

**An agent runs while the keys are present.** What keeps the keys in is
discobox, not the absence of an agent:
- a raw binding's value is held in the box proxy's memory;
- the proxy refuses it on any host the binding's routes do not name, and
  redacts it from every audit row and stored body (840);
- the keys reach only test accounts (§3);
- the skill does only this job.

The box's egress is **not narrowed** before the key arrives: the run builds
and reports from inside it.

### 5. A run reports on a standing issue, and files what it finds

- **An interval run** posts its report as one comment on a standing issue for
  that driver ("Live provider runs: boxd").
- **A release run** posts it on the release's tracking issue.

The report is §1's table, PASS, FAIL or CHECK per row, with timings, the
commit, and the teardown read-back. Each new failure is filed as its own
issue, after a search for a duplicate; a known one is commented on with the
new evidence. A run that leaves anything on the provider is `blocked`,
not `ready`, and says what is left.

### 6. One run per key at a time

A key's account has a machine quota and one key's scope, so the manager runs
at most one validation box per provider key at a time. A run that finds
machines under the live-run prefix from an earlier run reports them as left
over and removes them before it starts.

## Consequences

- `scripts/live-run.sh` grows the checks of §1 it lacks today: harness
  answer, sentinels, snapshot and second box, channel, region, and the
  per-region read-back. Its header and the 947 plan's Credentials section
  are updated to this ADR. The `--egress-narrowed` flag stays, and the report
  says `no`.
- **`discobox-ai/skills`:** a `validate-provider` skill. `validate-release`
  gains a step that asks for, or reads, the release's provider runs.
- **`discobox-ai/manager`:** a schedule for interval runs, a provider run at
  release validation, a store entry per key with its binding role, and the
  one-run-per-key rule.
- **The owner** provides the dedicated keys of §3, and rotates or revokes
  them.
- The harness's model call costs a few cents a run, bounded by the key's
  spend limit. A boxd run costs a pool and two or three sandbox machines for
  about half an hour.
- archil joins the interval when its pools are accepted (619's
  implementation). Until then its live steps (the plan's A1, A8 and A9) run
  this way on demand.

## Open

- **Interval cadence**, per driver: set in the manager's configuration.
  Daily is the starting suggestion.
- **Logging a harness in from its secret alone.** B6 needed a credential
  document added to the harness config by hand for an OAuth token. Whether
  an `ANTHROPIC_API_KEY` bound alone logs Claude Code in is checked first. If
  it does not, a CLI or default-document path is filed and built; the skill
  does not hand-edit harness config.
- **Whether disco-vm's `TestConformance` against the provider** (the plan's
  B1) becomes a row of §1 or stays disco-vm's own.
- **A release run's failure**: whether it blocks marking the release stable,
  or is recorded as a known issue. The release skill and the owner decide.

## Alternatives rejected

- **A GitHub Actions environment secret with required reviewers.**
  - A hosted runner's egress cannot be held to the provider's hosts, and
    there is no proxy that refuses the key elsewhere or redacts it.
  - The harness credential would need the same treatment, with no sentinel.
  - The manager already holds the keys and launches the boxes that validate
    releases.
- **A box with no agent, running the script alone** (the plan's rule). This
  keeps an agent away from the key. But someone must still read the
  evidence, file what failed, and do the steps a script does badly (a
  partial failure's teardown, a judgment on a CHECK row). The raw binding
  already keeps the value out of the agent's reach, and the keys are test
  accounts.
- **The owner's own temporary token per run** (as B6 did). Every run would
  wait on the owner.
- **A harness stub with no model call.** It shows the harness launches and
  holds a sentinel, but not that a request leaves through the pool's proxy
  and gets its answer, which is the check B6 had left open.
- **A run on a pull request that touches a provider.** It would run
  unreviewed code next to the keys.
