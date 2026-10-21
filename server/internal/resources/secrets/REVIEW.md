# secrets review notes

- A rule never answers, is proposed from, or is made from a request that carries a policy: `ruleTestable`, `ruleProposable`, and the console's `CredentialRequest.Ruleable` say so together, and change together. A rule is read against uses alone, so one made from a policy request would approve later asks by their uses, without the policy a person read (ADR 26-10-20-782, Deferred).
- A policy handed on by a discobox goes through `delegationsCovering` before `judgeDelegations`, on every path that hands one on — an approval and a create. A path that judges only the uses hands on whatever policy came with them.
