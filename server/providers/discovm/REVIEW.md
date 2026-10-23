# vm Provider Review Notes

- Nothing is placed on a remote driver's pool machine — the pool-agent
  bundle, the bootstrap, the provisioning key — until that machine is fenced
  from the rest of the provider's account: on boxd, by the network policy
  that lets only the pool's own machines reach it (ADR 26-10-23-551 §4). A
  pool machine is reused while it runs and repaired in place, so one created
  before it could be fenced (every machine this provider made on disco-vm
  f4aaa46, whose create sets no policy) is fenced or replaced first. Why: an
  unfenced boxd machine is reachable, exec and copy included, by the org's
  other machines.
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
