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
- Do not pace anything that spans clean passes with the engine's failure
  count: a pass that settles without error resets it. Bootstrap attempts keep
  their own count (`PoolBootstrap.Failed`).
- Bootstrap lifecycle changes update all four places that describe it: this
  package's `DESIGN.md` ("Initial bootstrap waits"), `server/internal/model`
  and `server/internal/store` `DESIGN.md`, and `server/providers/DESIGN.md`
  ("Pool Failure and Recovery").
