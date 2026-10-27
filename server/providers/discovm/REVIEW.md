# vm Provider Review Notes

- Nothing is placed on a remote driver's pool machine — the agent, the
  bootstrap, the provisioning key — that was not created on the pool's own
  network, off the account's default one (ADR 26-10-23-551 §4). A machine is
  adopted by its name in the provider's project alone, and machines an older
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
- A base, or anything else the provider boots from a driver's account, is
  named by the driver's ID for it, never by a name. Why: a name can be given to
  another base after an operator chose it (a boxd snapshot's), and the next
  machine would boot a disk somebody else wrote.
