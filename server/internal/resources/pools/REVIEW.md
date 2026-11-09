# Pool reconciliation review rules

- Re-probe every changed outbound route, including direct and trust-only modes.
  A previously successful proxy probe cannot certify a replacement direct path.
- Never route a `RepairPool` error through the `ErrPoolNotReachable` requeue.
  A repair can return it before it replaced anything (discovm's first lookup),
  and the next pass would then reconcile in place the runtime the repair was
  owed for.
- Do not pace anything that spans clean passes with the engine's failure
  count: a pass that settles without error resets it. Bootstrap attempts keep
  their own count (`PoolBootstrap.Failed`).
- Bootstrap lifecycle changes update all four places that describe it: this
  package's `DESIGN.md` ("Initial bootstrap waits"), `server/internal/model`
  and `server/internal/store` `DESIGN.md`, and `server/providers/DESIGN.md`
  ("Pool Failure and Recovery").
