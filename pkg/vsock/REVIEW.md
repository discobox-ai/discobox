# pkg/vsock review notes

- **Keep yamux's stream close timeout off in `frameConfig`.** Its default
  resets a stream five minutes after this side's `CloseWrite` if the peer has
  not ended its own side, which cuts off a container's output streamed after
  its client closed stdin (ADR 26-11-18-067). The session ends when its
  connection is closed instead.
- **A framed connection's `CloseWrite` is the stream's half-close; never make
  it a no-op.** A caller that finds the method believes the peer saw EOF.
  Both the guest's services and the vz host use this package, so a change to
  the framing here is a guest-image change: it makes the pinned guest stale
  (`internal/cmd/discobox-vm-pins`) until a new one is published and pinned.
