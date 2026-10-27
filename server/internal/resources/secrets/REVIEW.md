# secrets review notes

- Nothing here judges (ADR 26-10-27-880). A check that needs to read what a request means, rather than where it goes, belongs in the pool's static routes, never in a model asked from here.
- A binding's route lists include its parent's: anything that changes a binding also marks the holders of its children.
- Every binding write — create, update, delete, cascade, expiry — records its event and marks the holders (`MarkBindingHoldersDirtyTx`) in the same transaction. A new path that writes a binding through the store without both leaves the history short or a discobox holding what it no longer may.
- A binding a discobox hands on — by approving a request or on a create — is a child of a live delegate binding covering it, of the same secret, no longer-lived than it, read again in the write's transaction (`heldParent`, `fitToParent`). A new path that hands one on goes through both and sets `ParentID`.
- Only a person makes a binding through the binding routes, makes a delegate binding, or answers an ask to delegate; a discobox never hands on the power to hand on.
- A resolve hands a value out only from a standing binding (`standingBinding`) and only for a host every route list allows. A sentinel with no binding resolves to nothing and opens no request.
- A variable a discobox holds with another secret is a 409 on approval, never taken over.
- A route's `blocked` widens nothing when it is removed only if the person removing it means to: a migration or a default never unblocks a route.
