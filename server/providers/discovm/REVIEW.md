# disco-vm Provider Types Review Notes

- Nothing is placed on a remote driver's pool machine — the agent, the
  bootstrap, the provisioning key — that was not created on the pool's own
  network, off the account's default one (ADR 26-10-23-551 §4). A machine is
  adopted by its name in the provider's scope alone, and machines an older
  provider made (named otherwise, on no network of the pool's) are never
  adopted. Why: an unfenced boxd machine is reachable, exec and copy
  included, by the org's other machines.
- A key's secret is never in the pool's record, its `HostCredential`, a log
  or an error: the record keeps its ID, name, scope, expiry, and for the
  provider's own key a fingerprint. Why: the pool's row is read back by the
  API and the server's logs leave the host.
- The provider's own key is never placed on a pool's machine (ADR
  26-11-11-080 §9): in-machine, or an issued key, or the pool is blocked.
  An issue that fails is retried, never answered with a wider key. A
  renewal never revokes the key it replaces. Why: a transient error must not
  hand a pool a wider key that nothing expires (551 §4), and a revoked key
  fails the calls already in flight on it.
- What a pool's machine was placed with that the provider's configuration
  decides (the bootstrap, the policy) is recorded and compared on every
  reconcile, as its bundles are, so a moved configuration is placed again in
  a running machine, and the agent and proxy, which read it once at start,
  are restarted (`restartPoolUnits`): `apply` restarts only what moved
  bundles move. Why: only a pool that never registered is repaired, so a
  registered pool given a wrong `controlPlaneUrl` would otherwise stay
  offline for good.
- A remote engine's `Unknown` state is not a network fault until the error of
  the call that failed says so. Map only an unreachable service
  (`unreachable`) to `ErrPoolNotReachable`; a missing, expired, refused or too
  narrow credential is an error the pool shows. A remote driver on a
  transport `unreachable` does not read (it knows the network and gRPC) is
  added to it in the same change that links the driver. Why: the reconciler
  waits on `ErrPoolNotReachable` forever, so a server started without its key
  would leave every pool waiting with the cause hidden.
- Every call that asks a remote driver's service about the pool's machine or
  its keys — `engine.Get`, `Guest`, `URL`, `Create`, `Bases`, a key's issue,
  list or revoke, and any new one — wraps its error with `notReachable`, not
  only the call the last incident hit. Why: one left plain fails a running
  pool's reconcile on a moment's network fault, which repairs a pool with
  sandboxes and ends a create waiting for its agent (#454: the lookup was
  left plain after `state` and `URL` were mapped). Never `Start` or `Stop`:
  their errors carry the boot's or shutdown's own deadline, which
  `unreachable` reads as a fault, so a guest that never boots would be
  waited on forever. Nor an exec or file placement in the guest: its error
  is the guest's.
- The provider's `base` is disco-vm's (`machine.Config.Base`), passed to the
  driver as it is, never resolved, listed or matched here: what it names is
  the driver's to say. Do not grow per-driver handling of it in this package.
  Why: a lookup of the driver's bases here was a second owner of what disco-vm
  owns, and went when disco-vm stopped listing bases (disco-vm ADR
  26-11-03-606). Its description says it is trusted with the pool: a base a
  name can be repointed under boots whatever it is repointed to, with the
  pool's keys in it, so an operator names one only their account controls.
- When the disco-vm pin moves, compare each linked driver's definition
  (`machine.DefinitionOf`: fields, secrets and their sends, base) with the
  old pin's, and say in the change what a provider now offers and runs
  with. Why: a provider's catalog, validation and secret are the driver's,
  so a field the new pin adds is offered with no change here, and one it
  marks `Privileged` fails `TestPrivilegedFieldsAreTheCheckedEndpoints` until
  it is checked (ADR 26-11-11-080 §3).
- When the disco-vm pin moves, also compare what a driver's `Start` waits
  for. Since be1c6f74 boxd's returns once boxd says the machine runs, not
  once systemd has settled, so the guest commands that follow it here (key
  placement, `guest.Info`, the fence, `provision apply`) may run mid-boot;
  they must not assume a settled system, and a pin that waits for less
  again is checked on a live boxd pool, whose fresh machine and out-of-band
  stop converge on the first pass (done for #1238). The pool agent's side
  is in `pool-agent/REVIEW.md`. Why: the fake driver boots instantly, so CI
  cannot show it.
- Map a disco-vm sentinel in `unreachable` only when a call that goes
  through `notReachable` can return it. `machine.ErrUnreachable` is a guest
  operation's, which `engine.Start` retries itself and this package keeps
  out of the mapping (above). Why: a branch no caller reaches reads as
  handling a case it does not.
- A linked driver has no `Retired` field. disco-vm decodes one into its
  `ReplacedBy`, and the catalog lists it nowhere, so the providers service's
  per-key checks of a `Privileged` or `Immutable` field never see a write of
  the old name: `grpcAddr` retired onto `addr` would send the sealed key to
  any host, with no key demanded and pools standing. Why: the checks compare
  catalog keys, not the configuration disco-vm makes of them;
  `TestPrivilegedFieldsAreTheCheckedEndpoints` fails on one until they do.
- Nothing that carries the driver's configuration carries its secret: the
  bootstrap, the placement fingerprint, a shim's arguments, a log or an
  error. `decode` takes the secret out of `driver` before anything else sees
  it, and it reaches disco-vm only as `machine.Config.Keys`. Why: disco-vm
  refuses a configuration that names its secret, and everything that carries
  the configuration is read by more than the server (ADR 26-11-11-080 §2).
- The server reads no driver's environment variable but in the vm upgrade's
  migration (`store.MigrateVMProviders`), and every engine connection goes
  through the provider's checked dialer (`EndpointPolicy.Dial`). Do not give
  a driver a config from the environment, nor a nil `Dial`. Why: a project
  member writes the provider, and the server's network is not theirs (ADR
  26-11-11-080 §§2–3).
- Without a proxy the checked dial connects to the address it resolved and
  checked, never the name again. Through the server's proxy it sends the
  name and leaves resolving to the proxy: what an operator's proxy reaches
  is its policy, not the server's (owner, #1006). Why: resolving here and
  connecting by IP through a proxy breaks a TLS-intercepting or
  hostname-only proxy, and a name dialed straight but resolved twice could
  rebind inward between the check and the dial.
- A driver's in-machine auth is read from disco-vm's declaration
  (`machine.Definition.InMachine`: its field and its endpoints), never from
  a field's name, here or in the pool agent. Why: a field's name is no
  promise across drivers, and the declaration is the contract disco-vm keeps
  (vm#140); a new driver that declares it is picked up with no change here.
- A credential a running pool agent reads (its provisioning key file) is
  taken off its machine, and revoked, only after the placement whose agent
  no longer needs it has applied (`poolKeys.retire` after `ProvisionBox`).
  Why: the agent rereads the file on every call, and a placement that fails
  on a running pool keeps the old agent, which would then authenticate
  nowhere while the pool reads as converged.
- An adopted pool machine is not a created one, though `converge` calls both
  `made`: only a machine this pass created from the base, or started, may
  report a box start (`sandbox.AgentStartBox`). Any other machine reports an
  in-place start (`sandbox.AgentStartInPlace`) before the apply, and so does
  one it booted unless `replacesAgent` says nothing will restart (a repair
  with nothing moved). Decide that from what is known before the apply
  (bundles and placement fingerprint against the record), never "any machine
  not created". Why: an adopted running machine, or one recorded
  unprovisioned, may run an older agent, and a box start counts that agent's
  heartbeats as the new one's; and a repair's in-place start would replace
  its box start with no first report to follow, holding the pool a heartbeat
  after the settle (ADR 26-11-15-294 §1, 26-11-15-602 §1).
- Every pool host's `removePoolHost` empties the pool's own disco-vm scope
  (`machinePool`), not only the host: `poolMachine` does (`emptyPoolScope`),
  and `hostAgent`, which removes only its directory while its served engine
  is not staged (#64), must when it is, through whatever engine the server
  then opens on that scope. Why: the pool agent makes its snapshots and its
  sandboxes' machines there, the server's engine sees only the provider's
  scope, and once the pool's host is gone nothing else removes them (#1208).
- A provider call added to `converge` runs once a minute for every pool,
  not only on a change: the pool reconciler's scan marks every pool dirty
  each `ScanInterval`. Bound one that costs the provider anything (an
  interval kept in the pool's record, as the machine limit's `Check` is) and
  give it its own timeout. Why: the machine limit's `Check` was first added
  on the premise that passes run only on intent and drift, which would have
  been several boxd API calls per pool per minute, and a home endpoint that
  hung would have stalled every pool's pass.
