# secrets review notes

- Nothing here judges (ADR 26-10-27-880). A check that needs to read what a request means, rather than where it goes, belongs in the pool's static routes, never in a model asked from here.
- A grant a discobox hands on — by approving a request or on a create — is made under a live delegation grant it holds of the same secret, covering every host and outlasting the grant (`delegationsFor`, `delegationsFitting`), and is held to it again in the write's transaction (`delegatedTTL`, `HoldDelegations`). A new path that hands a grant on goes through both, and records the delegation as `SecretGrant.DelegationID`; one that leaves it empty leaves that grant untraceable to what allowed it.
- Only a person approves a request to delegate, or creates a delegation grant; a discobox never hands on the power to hand on (`refuseDelegatedApproval`).
- Approving an agent's use binds its variable as an ordinary injected binding and marks that sandbox's sentinel delivery in the same transaction. A variable bound to another secret is a 409, never taken over: swapping the credential under a variable something already uses is the surprise this must not spring.
- A route's `blocked` widens nothing when it is removed only if the person removing it means to: a migration or a default never unblocks a route.
