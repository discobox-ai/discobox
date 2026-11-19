# 26-11-19-797 — A box reprovisions its failed features on its own, a few times, and shows what still failed

- **Status**: Proposed
- **Date**: 2026-11-19
- **Issue**: [#1289](https://github.com/discobox-ai/discobox/issues/1289)
- **Would supersede**: [26-11-11-560](26-11-11-560-reprovision-reruns-a-running-boxs-failed-features-in-place.md),
  Deferred, "Reprovisioning automatically": the agent now does (§§1–3).
- **Would narrow**: [26-11-11-560](26-11-11-560-reprovision-reruns-a-running-boxs-failed-features-in-place.md)
  §5, "No console surface": the console still offers no reprovision, but it
  no longer shows a failed feature "as it does today". The row is marked
  (§5, owner, on #1289).
- **Relates to**: [26-11-11-560](26-11-11-560-reprovision-reruns-a-running-boxs-failed-features-in-place.md)
  §§1–4 and 6, the run this repeats, which stand;
  [26-11-07-519](26-11-07-519-a-running-box-keeps-its-reconcile-failure-as-a-warning.md)
  §2, the warning mark a running row already gets;
  [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §6, `provision.json`;
  [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
  §5 and Consequences, a pool's provisioning and idempotent steps.

## Context

A feature that fails in a running box stays failed until someone acts
(560, Context). The document is final for the agent's process, and the only
ways out are `discobox admin box reprovision`, `discobox-provision retry` in
the box (`sandbox-agent/provision/retry.go`), a restart, or a repair. 560
deferred the agent retrying on its own until failed features "turn out to be
mostly transient network errors that a person then has to reprovision by
hand".

That condition has been met. On #933, a box whose first start on a new
release raced the pool's container being recreated ended with 12 features
`failed` (`curl: (56) Proxy CONNECT aborted`, `GnuTLS recv error (-110)`).
They stayed failed minutes later, and every one came back on a restart.

Which failures are network errors cannot be told from where the agent
stands:

- **Bundle fetches already retry.** `applyBundles`
  (`sandbox-agent/cmd/discobox-sandbox-agent/bundles.go`) backs off from 2s
  to 30s, without limit, on a transport error or a 408, 429 or 5xx
  (`bundlestore.Retryable`). Only a non-transient fetch failure reaches
  `Tracker.Fail`.
- **Most steps are opaque.** Of the shipped features' steps, 28 are `run`,
  9 `packages` and 4 `download` (`sandbox-agent/features/*/provision.yaml`).
  A `run` step fails as `exit status N`, and most of those scripts pipe `curl`
  into a third-party installer (rustup, bun, nix, uv, NodeSource), which then
  does its own fetching. Its exit status is whatever that installer chose. Only
  `install-go.sh` retries inside its step. `apt-get` exits 100 for a fetch
  failure and for an unsatisfiable dependency alike.
- **Steps are idempotent by contract** (953, Consequences; 560 §3). Running
  a failed feature again is safe whatever failed it.

Where a failure is seen is narrow too. A failed `async` feature leaves
`provision.ready` true, so the server's display state is `running` and its
`errorMessage` is empty (`server/internal/model/model.go`, `runningDisplayState`).
`discobox ls` fills ERROR only from `sandboxMessage`
(`cli/internal/cli/output.go`), so it shows nothing. The console builds the
text, `sandboxProvisioning` (`cli/internal/cli/provisioning.go`, for example
`rust failed to provision: …`). But it shows that text only under the cursor,
and only when nothing else claims the name's place
(`cli/internal/tui/list.go`, the `s.Provisioning != ""` branch). The row's own
marks are unchanged.

## Decision

### 1. The agent reprovisions a failed box on its own, whatever failed it

When a run in a box's sandbox agent ends with a failed feature, or failed
with no feature to name (`Tracker.Fail`), the agent schedules an
**automatic reprovision**. It is 560's reprovision exactly: `Tracker.Rerun`
and the same run, with the same scope (every failed feature, and the boot's
path whole when nothing could be planned, §2), the same joining (§3), and the
same reopening of readiness for a blocking feature (§4). Nothing new runs and
nothing new is skipped.

It does not classify the error. Any failed feature is retried, blocking or
`async`, from any step kind (owner, on #1289). A real fault costs the
attempts below and then stands as it does today, while a network blip
recovers with no one acting.

It applies wherever 560 applies: a box's sandbox agent, container or machine
alike (560 §7). It does not apply to a snapshot's run, which is an image's
build and fails the build, or to a pool's `discobox-provision apply`, which its
provider's reconcile runs again (953 §5).

### 2. Three attempts, at growing intervals with jitter

- **Bound.** At most **3** automatic attempts follow a run. When the third
  fails, the failure stands until a person reprovisions, restarts or repairs
  the box.
- **Backoff.** Each attempt starts **15 seconds**, then **1 minute**, then
  **4 minutes** after the run before it ended. Every interval varies by
  ±20%. A pool coming back makes every box on it fail at once, and the
  jitter keeps them from all coming back at once too. All three are spent
  within about six minutes, which covers a pool's container being made
  again (#933) and a registry's brief outage, and does not hide a real
  fault for long.
- **What resets it.** The count belongs to the agent's process: a new start
  of the box is a new boot run, with a fresh count. A reprovision a person
  asks for (the API route, or `discobox-provision retry`) that starts a run
  also restarts the schedule. If that run fails, up to three automatic
  attempts follow it again: the person acted, perhaps after fixing what
  failed it. A run that ends with nothing failed clears the schedule.
- **A scheduled attempt is cancelled** when a run starts before it, from a
  person's request. When the agent stops, it simply never runs.

### 3. The document says an attempt is scheduled

`provision.json` gains one optional field on the document, never on a
feature:

```json
"retry": {"attempt": 2, "of": 3, "at": "2026-11-19T10:04:31Z"}
```

It is present only while an automatic attempt is scheduled and not yet
started. `attempt` is the attempt that will run at `at`, and `of` is the
bound. It is set in the same update that ends the failed run, and cleared in
the update that starts the attempt (whose features move to `pending`), or
when a person's run starts first. So `failed` with no `retry` is final for
this boot, and `failed` with one is not yet. A reader older than the field
ignores it, as any unknown field is ignored.

Its appearing and clearing coincide with a document state change, so it
needs no rule of its own for 560 §6's status wait: the pool is woken by the
state change it comes with.

Between attempts the box is as a failed run leaves it today. A failed
blocking feature keeps readiness failed, so a terminal launch or command
creation that arrives meanwhile is refused with the failure (560 §4: one
refused stays refused). One that arrives while an attempt runs joins it.
A failed `async` feature's placeholders (`discobox-provision run`) wait
while an attempt is scheduled, and say they are waiting for the automatic
retry. They wait for the next attempt only, and report the failure if it
fails.

### 4. `discobox ls` shows a running box's provisioning in ERROR

ERROR keeps what it shows today first (`sandboxMessage`: the error the box
settled on, or an automatic repair's cause). When that is empty, it shows
`sandboxProvisioning`'s text, the same one the console already builds. While
an automatic attempt is scheduled, that text says so (for example
`rust failed to provision (retrying)`). JSON output is unchanged: it already
carries `agentStatus.provision`, and so `retry`.

### 5. The console marks the row once the retries are spent

A running row whose box has a failed feature, or failed readiness, and no
automatic attempt scheduled (§3), gets the `⚠` mark a running row with a
reconcile warning gets (519 §2), in the same place and colour. The state
stays running. While an attempt is scheduled the row is unmarked, and the
text under the cursor says it is retrying (owner, on #1289). The text
under the cursor keeps its order of precedence; only the mark is new.

The console still offers no reprovision and binds no key to it (560 §5 and
its Deferred "Reprovision in the console" stand).

## Alternatives rejected

- **Retry only errors classified as transient.** The Go-level kinds could
  classify their own failures (`download`'s transport errors, stalls and
  5xx, as `bundlestore.Retryable` does), and `run` scripts could opt in by
  exiting 75 (`EX_TEMPFAIL`). But a script that pipes `curl` into a
  third-party installer cannot keep that contract, because the installer's
  own fetch picks the exit status. `apt-get`'s 100 means both kinds. So it
  would miss most of what #933 saw, which is the case this ADR exists for.
  Matching the log's text (`curl: (56)`) is a guess that breaks with the
  tool's wording.
- **Retry only `async` features.** A blocking feature's failure (core, a
  harness's CLI from npm) blocks every terminal, so it is where a person
  most needs the box to recover by itself, and it fetches over the same
  proxy. Reopened readiness (560 §4) already gives a terminal arriving
  mid-attempt the right answer.
- **Retry without a bound, as bundle fetches do.** A bundle fetch is
  classified and is the box's only way to have any features at all. A
  feature's real fault, retried forever, would rerun a failing installer
  every few minutes for the box's life. It would also keep the row from
  ever being marked, since the failure would never be final.
- **Retry inside the step**, as `install-go.sh` does. It needs every
  script, and every step kind, to carry its own loop. And it holds the
  feature's place in requires order for the whole wait. A whole-feature
  rerun is the unit the runner already knows how to repeat.
- **Wait for a signal that the network is back** (a new runtime-config
  document, the pool's channel coming up) rather than a schedule. It only
  covers the pool's own outages, not an upstream's (a registry, sh.rustup.rs).
  It also couples provisioning to the intake's lifecycle. Three timed attempts
  cover both.
- **A per-feature retry field.** Every failed feature runs in the same
  attempt, since a reprovision's scope is all failed features (560 §2). So
  one schedule per document is what exists.
- **Mark the row on any failure, retries pending included.** It would flag
  every box for the seconds a blip lasts, which trains people to ignore the
  mark. The text under the cursor already says what is happening.

## Consequences

- A box that hits a network blip while provisioning recovers in seconds to
  minutes with no one acting. A real fault is reported up to about six
  minutes later than today as final, but is visible as failed (with
  `retrying`) from the first failure.
- A failing installer runs up to four times per boot, and each attempt
  appends to the feature's log.
- A failed blocking feature reopens readiness on each attempt. A terminal
  launch that arrives during one waits for it instead of being refused.
- `provisionspec.Status` gains `retry`. The tracker owns the schedule, beside
  the run state it already holds.
- `discobox ls` and the console show a failed feature without anyone moving
  the cursor to it.

## Open, for implementation

- Whether the schedule lives in `provision.Tracker` or in the agent's
  `reprovisioner`, which already holds the run's context. The document's
  field is the tracker's either way.
- The exact wording of `ls`'s and the placeholder's "retrying" text.
