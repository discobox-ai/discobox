# Projects Review

- A new refusal in a resource's create path (`CreateSandboxProviderInstance`, `CreatePool`) is checked against project copy, which re-creates every provider and pool through those creates: a refusal of something a source project can hold fails the whole copy part-way. Why: it happened twice — retired `vm` rows (#1006, see `store/REVIEW.md`) and the 409 on a disabled provider's pools (#1556), which copy now leaves behind.
