# policy review notes

- Every slot that takes a type refuses the reserved words that do not fit it: `anything` is only a resource type, `anywhere` only a deny's scope type. A reserved word needs no definition, so one accepted in the wrong slot is an unread widening — `allow … in anything *` once validated as an allow with no scope.
- `Evaluate` is the only place parts are combined. A caller that cannot decide a part passes `Unknown`, never `No`, and never fakes a part to force a verdict: `Unknown` on a deny's part also keeps that deny from refusing.
