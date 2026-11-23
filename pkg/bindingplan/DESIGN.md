# Binding plan

What approving one part of a credential request does to the bindings the
asking discobox already holds of the secret (ADR 26-11-02-022 §5). One rule,
two readers:

- **The server** (`resources/secrets.replaceBindings`) applies it after
  checking every binding it touches against the raw and parent refusals.
- **The CLI and the console** show it before a person approves: which binding
  is kept and updated, which go, which lose their use side or are split.

`Answer` plans a whole answer the way the server applies it: the use part as
it is granted (`As`), then the delegate part against what the use part leaves
(`After`). A use part granted as an addition (`AddBeside`, ADR 26-11-03-329
§4) touches nothing; one granted to replace never keeps an addition, and
deletes every one.

The preview of the use part's variable is the same rule seen from the grants
applying there, from whichever level (ADR 26-11-03-329 §5): `Replacing` says
what a replacement does to each — an addition goes, the discobox's own binding
is cut short, a harness config's or project's is shadowed — and `NewRoutes`
which asked routes no grant names, compared as written. A preview
of two parts planned from one snapshot would show a split and a second new
use binding that the server never makes. `Delegate` relies on a discobox
holding at most one delegate binding of a secret, which the server's store
enforces (`idx_secret_binding_delegate`).

A person may rebind the variable from another secret (ADR 26-11-19-927):
`Rebind` plans what the replaced secret's bindings lose — the one named loses
its use (split when it also delegates), its additions in the variable go —
and `Replacing` reports the discobox's own binding of another secret as
`rebound`, never `shortened`; a harness config's or project's of another
secret stays `shadowed`. `Held.SecretID` is what tells them apart, so every
caller fills it. `ApprovalRefusal` is the approval path's refusal of a
binding a rebind touches, and `KeepsSentinel` whether the variable keeps its
sentinel: the server applies both, and the CLI and console say them first.

It decides fates, and the two rebind rules above, from IDs, flags, variables,
formats and age; writes, refusal wording and minting stay with the server. Standard library only, so the CLI's
module graph gains nothing. Change the rule here, never in a reader.
