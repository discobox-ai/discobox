# Providers Design

`internal/resources/providers` owns provider-instance API behavior, the
provider-type catalog, and startup reconciliation. A provider instance is
backend identity only — type, name, connection config, its credentials, and a
disabled flag (`model.SandboxProviderInstance`). Capacity, sharing policy, and
observed runtime status belong to `Pool` (`internal/resources/pools`).

## Credentials

A config field the definition types `secret` (`sandbox.ProviderConfigFieldSecret`)
holds a credential, and is sealed in the instance's own row, apart from the
rest of the config (ADR 26-10-31-694):

- **Write.** The service splits a written config by the definition's secret
  fields: they go to `encrypted_config`, which the store seals, and `config`
  keeps the rest. On update, a secret field the body leaves out keeps its
  sealed value, a value replaces it, and null clears it. The update reads
  the row locked and writes it in one transaction. The config is validated whole,
  secret fields joined in, as create's is.
- **Read.** Every answer carries `config` without secret fields and
  `secretFields`, the names of those with a value; no value is ever returned,
  not even one still in a row's stored `config`. A row whose sealed fields do
  not open is logged and answered without `secretFields`, so it does not take
  the listing down.
- **Resolve.** `sandbox.ProviderManager` opens the sealed fields through the
  store and joins them into the config the provider's factory and
  `Initialize` decode; a provider imports nothing of the store. A secret
  field still in `config` fails the resolve and never reaches the provider.
- **Rotation** is an update: it moves `UpdatedAt`, on which the manager's
  cache is keyed. No secret field is `Immutable`.
- **No project secret** is named, so no binding, request, delegation or raw
  delivery reaches a provider's credential. An environment reference
  (`tokenEnv`, `sshPrivateKeyEnv`) is refused by name (400).

## Boundaries

```mermaid
flowchart LR
    handlers[internal/handlers] --> contract[internal/services.SandboxProviderInstanceService]
    contract --> service[Service]
    projects[internal/resources/projects copy] --> service
    startup[internal/service.Service.Start] --> service
    service --> store[internal/store]
    service --> sandboxCatalog[internal/resources/sandboxes.Service]
    service --> pools[internal/resources/pools.ControlPlane]
```

- `Service` validates provider instance API requests and coordinates provider
  runtime ensure behavior. It reaches the provider registry through
  `SandboxCatalogService` (catalog plus `sandbox.ProviderManager`).
- Simple provider CRUD may call store directly.
- Create requires a type and validates it and its config with
  `ProviderManager.ValidateProviderConfig`, which refuses a type the manager
  cannot resolve (no factory and no registered provider; a definition alone
  does not count). Either refusal is a 400 before anything is written. It then
  persists and `ResolveInstance`s so the new instance's provider initializes
  immediately. A resolve that fails is the backend not coming up (the config
  already passed validation), so it answers 502 and deletes the row again, on
  a context the request's cancellation does not reach: a failed create leaves
  nothing behind. The row is written first because the provider cache is keyed
  on its ID, and resolving runs provider I/O that must not hold the write
  transaction. Create refuses such an instance while startup keeps one (below)
  because create is the one moment the caller can still be told; a persisted
  instance was already accepted. Update validates config against the stored
  type (400 on refusal) and persists name, config, and `Disabled`; type is
  immutable. Update does not re-resolve: the manager rebuilds a cached
  provider on the next resolve because the cache is keyed by `UpdatedAt`.
- Project copy creates the copied instances through this service
  (`CreateSandboxProviderInstance`), so they get the same validation and
  resolve. It hands the create the source's config with its sealed fields
  joined in, so the copy seals them again in its own row.
- Provider instance deletion must refuse deletion (409) while pools are still
  bound to the instance: pools bind immutably at create, and sandboxes hang off
  the pools. Delete the pools first.
- For the same reason, update refuses (409, naming the fields) a config that
  changes a field the provider's definition marks `Immutable` while pools are
  bound. Such a field decides what made a pool's host, as a VM driver
  does, so a new value would leave hosts nothing can remove. Fields compare by
  compacted JSON value, and an absent field differs from a present one.
- Startup reconciliation (`EnsureExistingSandboxProviderInstances`), run from
  `internal/service.Service.Start` once the reconcile engine is up, resolves
  every enabled instance in every project. Resolving runs the provider's
  factory and `Initialize`, and a pool-backed provider's `Initialize` schedules
  each of its pools' reconciles. An instance that does not resolve is logged
  and does not fail server start — it belongs to one project, and a stopped
  server cannot serve the API that would disable or delete it. Its pools are
  marked dirty instead, so their reconciles retry the resolve with the
  engine's backoff and carry the error. Only a store error fails the start.
- `EnqueueProviderPools` marks every pool bound to one instance dirty through
  `pools.ControlPlane.SchedulePoolReconciliation`.
- Provider status reports availability only (`sandbox.ProviderStatus`:
  available, state, message, details, from the required `Provider.Status`). It
  is per registered provider type and surfaced only in the catalog, under the
  item's `capabilities` field; the instance API returns the stored instance
  with no status. Host readiness, capacity, and convergence are pool status,
  owned by `internal/resources/pools`. Do not infer or expose sandbox
  "capabilities" from optional interface assertions; callers that need a
  feature-specific provider operation should depend on that operation
  directly.
