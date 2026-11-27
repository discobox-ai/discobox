# Pool reconciliation review rules

- Re-probe every changed outbound route, including direct and trust-only modes.
  A previously successful proxy probe cannot certify a replacement direct path.
- Never route a `RepairPool` error through the `ErrPoolNotReachable` requeue.
  A repair can return it before it replaced anything (discovm's first lookup),
  and the next pass would then reconcile in place the runtime the repair was
  owed for.
- A provider that keeps a pool's previous box on purpose — a release that does
  not resolve, bundles that do not apply, an agent that does not restart on
  its new placement — reports it with `sandbox.ReportBoxKept` beside its log
  line. The reconcile still settles cleanly, so nothing else tells the
  upgrade watch the pool runs its previous agent, and the server would report
  itself settled over it (ADR 26-11-14-624 §1).
- A provider that makes or starts a pool's box, or may restart the agent in
  a running one (the apply and the box's units after it), reports it with
  `sandbox.ReportAgentStarted`: a box start before the make or start, an
  in-place start before the apply, never later, and never a box start for a
  box an agent may already run in. Missing one makes the upgrade watch reject
  the new agent's first report and hold health a heartbeat longer; a box
  start for a running agent counts the old agent's heartbeats
  (ADR 26-11-15-294 §1, 26-11-15-602 §1).
- After an in-place start only a report marked `firstReport` counts before
  the settle. Never count an unmarked report from after the start: the old
  agent's heartbeat can land between the record and the restart
  (ADR 26-11-15-602 §3).
- Do not pace anything that spans clean passes with the engine's failure
  count: a pass that settles without error resets it. Bootstrap attempts keep
  their own count (`PoolBootstrap.Failed`).
- Bootstrap lifecycle changes update all four places that describe it: this
  package's `DESIGN.md` ("Initial bootstrap waits"), `server/internal/model`
  and `server/internal/store` `DESIGN.md`, and `server/providers/DESIGN.md`
  ("Pool Failure and Recovery"). A new field on `PoolBootstrap` or
  `BootstrapEvidence` is such a change, even one that is display only; a
  place with nothing to add says so to its reviewer. A display-only field
  once updated this package's doc alone.
- A new writer of a pool column updates "Who owns which status field" in
  this package's `DESIGN.md`, and `server/internal/store/DESIGN.md` where it
  is a narrow store write: a mid-pass save of the provider's record
  (`RecordPoolHost`) once left the table naming the reconciler its only
  writer.
- Display state a pass writes on the attempt (`PoolBootstrap.Waiting`) is
  cleared by the pass that ends it, not only by progress: a pass can get
  past a wait and report no evidence, and an expiry would then blame the
  wait it got past.
