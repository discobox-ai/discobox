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
- An issue that fails is retried, never answered with the provider's own
  key, and the provider's key is placed only with `deliverProviderKey`. A
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
- The checked dial never hands a name it checked to anything that resolves
  it again: through a proxy, it CONNECTs to the address it resolved and
  checked, and refuses a name it cannot resolve, but for an endpoint the
  operator allows or the upgrade carried. Why: a checked dial that hands a
  name to someone else to resolve has checked nothing; a rebinding name
  answers public here and inward at the proxy (#1006 review).
