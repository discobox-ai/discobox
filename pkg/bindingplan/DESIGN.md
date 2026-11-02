# Binding plan

What approving one part of a credential request does to the bindings the
asking discobox already holds of the secret (ADR 26-11-02-022 §5). One rule,
two readers:

- **The server** (`resources/secrets.replaceBindings`) applies it after
  checking every binding it touches against the raw and parent refusals.
- **The CLI and the console** show it before a person approves: which binding
  is kept and updated, which go, which lose their use side or are split.

`Answer` plans a whole answer the way the server applies it: the use part,
then the delegate part against what the use part leaves (`After`). A preview
of two parts planned from one snapshot would show a split and a second new
use binding that the server never makes. `Delegate` relies on a discobox
holding at most one delegate binding of a secret, which the server's store
enforces (`idx_secret_binding_delegate`).

It decides fates only, from IDs, flags, variables and age; refusals, writes
and sentinels stay with the server. Standard library only, so the CLI's
module graph gains nothing. Change the rule here, never in a reader.
