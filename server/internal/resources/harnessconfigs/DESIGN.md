# Harness Configs Design

`internal/resources/harnessconfigs` owns project-scoped harness config behavior.

A harness config is the **only** harness concept — there is no separate
definition. Every harness in the registry is seeded as a built-in config
(`SeedBuiltIns`); everything else is a user-registered bundle.

`shell` is one of those registry harnesses, not a different kind of thing
(ADR 0043): `harness/shell`, a bundle like its siblings, inspected and seeded
by the same code path with no branch of its own. What is
true of it, and true *by rule* rather than by slug:

- Its slug is reserved so nothing else can claim it (ADR 0032 §3). It is not a
  fallback: create resolves an explicit harness or the project default and
  refuses when it has neither (ADR 0048). `shell` is reached by being named or
  by being the default, like any other harness. The one path that ends at it is
  the upgrade of a legacy sandbox that carries no harness config, which adopts
  it (`resources/sandboxes.fallbackHarnessConfig`).
- It carries **no run or relaunch command**, which is a declaration and not a
  gap: the sandbox resolves the run user's login shell, the only place that
  knows whether that is bash, zsh, or fish. `sandbox-agent`'s terminal layer
  treats a declared harness with no command as that shell, keeping the declared
  harness identity.
- Like every registered harness it can run as soon as it is seeded: there is
  no enable flag, and a harness that needs a credential starts signed out and
  is signed in inside the box (ADR 26-11-02-152 §1). Deleting it is refused by
  the built-in rule.

## Registration and bundle metadata

- A harness is registered as its **bundle** (ADR 26-10-16-423 §2), the
  `FROM scratch` image of its features whose labels are its manifest, which
  every box on the harness applies: no harness is an image (§10), so every box
  runs its provider's image — on the Docker driver, the sandbox base image —
  and its agent applies its platform and harness bundles at every start (§7).
  A config records it with its digest (`Bundle`/`BundleDigest`), and nothing
  else: there is no other registration (ADR 26-11-01-122), and the create
  body has no field for one.
- The bundle's manifest **labels** are authoritative. `resolveHarness`
  inspects the bundle **once** (local Docker daemon first, registry fallback;
  a development bundle from its OCI image layout, which no daemon holds) and
  composes its labels on the platform bundle's through
  `harness.ComposeBundleLabels` — the platform's `10-platform` layer, then
  every `io.discobox.image.v1.<NN>-<name>` layer the bundle carries, then its
  own `io.discobox.image.v1` — before validating the merged result and
  snapshotting the run/relaunch argv, save-back definition, files, secret
  declarations, env, volumes, additional groups, and declared egress hosts
  onto the config (`snapshotImageMetadata`). Nothing re-reads the labels
  afterward. A bundle that still declares the retired configure command or
  ports is refused when its manifest is decoded (`harness`), naming
  ADR 26-11-02-152.
- The **platform bundle** is the one the server's release pins
  (`harnessdefs.References.PlatformBundle`, which server configuration
  resolves: the release manifest's `Bundles.Platform`, `platformBundle` in
  `server.yaml`, or the compiled-in `harness.PlatformBundleRef`; none
  configured is an error), inspected once per registration or seeding pass
  (`platformBundle`). A harness bundle carrying a `10-platform`
  or `10-sandbox-base` layer of its own was built `FROM` the platform, and is
  refused: a harness extends a harness, never the platform. A harness names
  the `platformAPIVersion` it needs, which must be the one the platform's
  layer provides.
- **A bundle carrying a whiteout is refused**, read from its layers
  (`refuseWhiteouts`): from the registry, the layers of its manifest (the
  platform bundle's for this machine's platform, of its index); from the local
  daemon, what it saves, for its own platform where the daemon can be asked;
  from a development bundle's layout, the layers its manifest lists.
  A bundle's tree is its layers side by side, so a deletion has nothing to
  mean. The verdict is its digest's, so each digest's layers are read once
  per process (`verifiedBundles`): the platform bundle's are the agent's
  binaries, and seeding runs on every project and every start. The layers
  are read only once the labels admit the bundle (`InspectBundle`'s
  `admit`): a harness bundle once its manifest composes, the platform once it
  carries its layer — and, first, either one only if it carries no
  `ReclaimLabel`, which every image built `FROM` `discobox-sandbox-base`
  inherits and no `FROM scratch` bundle has. So an image of several GB named
  as either bundle — the pool agent's, a snapshot, a release from before
  bundles' runnable harness image — is refused for its labels and never
  saved.
- **The composed manifest has a digest of its own**, `ManifestDigest`: of
  what the config snapshots from the composition, so it moves when the
  harness bundle's labels or the platform's layer do and with nothing else —
  not with the agent, which a box's pool places from its agent pin at its
  next start. It is what a sandbox's container is built with — its env,
  files and volumes reach a container only when it is created — so a sandbox
  pins it beside the bundle and its rebuild keys on it (`sandboxes/DESIGN.md`):
  a platform-layer edit moves no bundle digest, and without it would reach no
  existing sandbox. Seeding compares it, and `applyResolvedPins` fans it out.
- The harness's egress level is two fields: `ImageEgressHosts`, the hosts the
  bundle declares, replaced by every re-snapshot, and `Egress`, what a person
  set, which a re-snapshot never touches (ADR 26-10-01-032 §3) and only
  `store.SetHarnessConfigEgress` writes. A change to either marks the proxy policy
  push of the config's discoboxes in the transaction that saves it
  (`updateHarnessConfig`), through `SandboxRuntime.MarkProxyPolicyChangedTx`.
- The recorded digest is the one its registry serves the bundle under, fetched
  with one `remote.Get` — an index digest for the multi-platform platform
  bundle; from the local daemon, the `RepoDigests` entry, or the image ID for
  a never-pushed local build; for a development bundle, the manifest its
  layout's index lists (`devimage.Bundle`).
- A harness names no **platform** and no **image kind** (ADR 26-10-16-423 §2,
  ADR 26-11-01-122): its steps select the platform they run on, and the kind
  of box is its pool's — a Docker pool's container or a VM pool's machine —
  so a config records neither and rules no pool out. The merged manifest is
  judged as a Linux box's (`harness.ImageMetadata.ValidateFor`): a label
  naming a platform, an image kind, an account or a shell is refused, and
  the features it declares are validated and not snapshotted.
- In dev, `devImageInspector` answers first, from the development bundles
  (`SetDevelopmentBundles`, `server/internal/devbundles`): a layout the image
  watcher exported, or in build-mode one the server saved once a pool's daemon
  built it. A build-mode bundle no pool has built yet is only its build
  description, so its label is rebuilt from the dev manifest's build args — a
  harness bundle's (`bundle` target) own label from `HARNESS_METADATA`, the
  platform's (`platform` target) `10-platform` layer from
  `IMAGE_LAYER_METADATA` — and the reference itself stands in for its digest.
  No box can be pinned to that: create refuses it (`resources/sandboxes`), and
  the save re-seeds the built-ins onto the built bundle's digest.
- **A harness registered before bundles** keeps the manifest its image's
  labels gave it, and neither a bundle nor an image: the image it ran as was
  cleared (`internal/database`), since no harness is an image. It runs nowhere
  — create refuses it, saying so — until `UpdateHarnessConfig`
  with a `bundle` registers one, read before anything is saved. A refresh has
  nothing to read it again from and is refused, saying so. A built-in is
  given its bundle by its next seed.
- The rest of the manifest is **optional**. A bundle that declares only its
  `platformAPIVersion` registers under the caller's `--name`/`--slug`,
  inherits its env and volumes from the platform layer, and takes the
  harness-run convention for its commands.
- `runCommand` is **optional**, and omitting it means "type
  `discobox-harness-run`" (ADR 0086 §3); an omitted `relaunchCommand` means
  `discobox-harness-run --resume`. `conventionCommands` resolves both
  here, at registration, because this is where the reserved `shell` slug is
  known, and `shell` is the one harness that must get no command at all. A
  *blank* command is still rejected: declaring nothing and declaring an empty
  string are different.
- Built-in configs **track** their bundle: `SeedBuiltIns` clobbers `Bundle`,
  clears any image left from before bundles, and re-snapshots the labels
  whenever the resolved reference, its digest or the composed manifest's
  changes, which is how a dev rebuild (`DISCOBOX_HARNESS_<SLUG>_BUNDLE` →
  `.env` → server restart, or a build-mode save) reaches a running server.
  `NewService` receives the resolved `harnessdefs.References` from server
  configuration; an explicitly selected release manifest replaces the
  environment overrides. Seeding never touches the saved overlay. A seed whose slug
  belongs to a user-created config fails visibly before inspection or
  mutation.
- Seeding is **not** how a test gets a harness config. It reads metadata off a
  bundle's label, so it needs a daemon, layout or registry holding bundles a
  checkout may never have built. Tests that need a
  selectable harness write the config to the store directly, which is what
  ADR 0066 §7 named as the end state; CI builds no stand-in images.
- With an explicit release manifest, failed inspection of a bundle fails
  seeding; startup must not silently keep harnesses from a
  different release.
- Otherwise, seeding is best-effort per harness: an uninspectable bundle is logged
  and skipped so it cannot block startup. **Skipping all of them is not**.
  `EnsureHarnessAvailable` reports a project with no harness config at all —
  any config counts, not only a built-in — and the server process refuses to
  serve when the default project has none, because sandbox create resolves a
  harness or refuses (ADR 0048) and the causes are all outside the product: a
  bundle published without its manifest labels, a daemon that is not running, a
  registry out of reach. It re-inspects each built-in to say why, since it only
  runs on a server that is about to fail anyway.

## A resolved digest reaches the sandboxes running it

`applyResolvedPins` is the one place a newly resolved bundle reaches the
sandboxes on that config (ADR 0082, ADR 26-10-16-423 §7). The rule is stated
on the fields rather than on a list of callers: **wherever `BundleDigest` or
`ManifestDigest` is written to a value different from the one it
replaced**, the config's eligible stopped sandboxes
are re-pinned onto it through `SandboxRuntime.UpgradeHarnessConfigSandboxes`,
which does nothing when the project's automatic-upgrade policy is off (ADR
0082 §3).

Both writers funnel through it, and they are the same event to a sandbox:
`SeedBuiltIns` carries a dev rebuild of a stable tag, and
`RefreshHarnessConfigImage` (`POST .../refresh-image`) is the only trigger a
user-registered bundle has at all. `CreateHarnessConfig` also writes a digest
and is deliberately exempt — the config is new, so nothing references it. A
future writer inherits the behavior by calling this helper rather than by
being remembered.

An unchanged digest does nothing, because the fan-out costs a query per config
and seeding runs on every project ensure. Failure is logged, not returned: the
digest is already persisted, so what is lost is the upgrade rather than the
refresh, and the sandboxes it missed keep reporting an available upgrade until
this config's bundle next resolves.

## Every harness runs; credentials arrive by save-back

There is no enable flag (ADR 26-11-02-152 §1). Create, a harness change, a
transfer and project copy select any registered harness, and none refuses a
harness for a declared credential nothing binds: the box starts its harness
signed out, the person signs in there, and saving the box's harness change
(see [Saving local harness configuration](#saving-local-harness-configuration))
is what puts a credential on the config for every other box.

A saved credential gets one use binding on the harness config per variable
(`bindHarnessSecret`): no routes of its own, so it inherits the secret's, and
no expiry. A newly saved secret uses registered provider/type routes. Built-in
providers without a shared destination require explicit person-owned policy
(ADR 26-11-04-772). Legacy third-party metadata keeps its declared host/`*`
fallback (ADR 26-10-27-939 §2). It is
and named after the variable — with the config's slug before it when the
project already has a secret of that name, since a secret's name is unique in
its project (`freeSecretName`). Every binding written marks the sandboxes
running the config, which materialize it.

## OAuth (rotating) secrets

A saved credential may be of type `oauth` (the claude-code subscription login
is). Its value carries the current access token in
`SecretValue.Token` — so the proxy swap is byte-identical to a `bearer` — plus
the refresh material (`refreshToken`, `tokenUrl`, `clientId`,
`accessTokenExpiresAt`) that **never leaves the control plane**: the resolve
handler emits `Token` alone.

It may also carry `scopes` and `subscriptionType`. These are **not** credentials
— they describe what the token is allowed to do — and they are recorded because
a client may gate features on the scopes it finds recorded next to the token:
Claude Code refuses Remote Control unless the credential it reads carries
`user:profile`, so a sandbox handed a bare token is limited to inference no
matter what the token is actually good for.

They are **captured at login, never assumed.** Which scopes a login yields
depends on the account and the flow, and claiming one the token does not have
trades a clear client-side refusal for an opaque upstream 401. `/login` is the
only moment they are visible — the authorization server returns them alongside
the token and they appear nowhere else — so the harness's export copies them
out of the credentials file the login wrote. A rotation carries them forward
untouched: a refresh mints a new token for the *same* authorization, not a new one.

Refresh happens lazily inside `ResolveSandboxSecret` (`resources/secrets`), the
one place that decrypts and hands out a value:

- When the access token is within `oauthRefreshSkew` of expiry, the server
  POSTs `grant_type=refresh_token` to `tokenUrl` — as JSON, or form-encoded when
  the secret records `tokenRequestEncoding: form` — and re-encrypts the rotated
  pair before returning. The refresh token rotates on every use, so the server
  is the **single writer**: a per-secret `singleflight` collapses concurrent
  resolves onto one upstream refresh, and the persisted write is guarded by the
  row's `revision` (`UpdateSecretValueIfUnchanged`) so a refresh in another
  process cannot be clobbered.
- The rotated token's expiry comes from the response's `expires_in`, and from
  the access token's own `exp` claim when the endpoint omits it (JWT access
  tokens state it; OpenAI's are, Anthropic's are not). Recording "unknown"
  instead is not a neutral fallback: an unknown expiry means "refresh now" on
  every later resolve, and each refresh spends a refresh token that rotates on
  use. A token that states no expiry still records none — guessing one would
  defer the refresh past the point the token dies.
- The resolution's `ExpiresAt` is capped at `min(binding expiry, token expiry)`,
  so the proxy's per-`(client, sentinel, host)` cache lapses at token expiry and
  re-resolves — which is what triggers the next refresh. The **binding stays
  persistent**: consent is "the login was saved", not the 8-hour token
  TTL, so tokens rotate silently without re-approval.
- A failed refresh is not fatal while a token is still on hand: it is served and
  a use-time `401` from upstream is left as the authority on liveness.

This is why the OAuth path uses `/login` rather than `claude setup-token`: only
`/login` yields a rotating refresh token; `setup-token` mints a single
long-lived token with nothing to refresh.

## Three file sets, each with one owner

A config's files are three sets, kept **separate** so each writer can replace
its own without reconstructing another's (ADR 26-11-04-028):

| Set | Owner | Written by | Never written by |
| --- | --- | --- | --- |
| `Files` | the bundle | `snapshotImageMetadata` (create, seed, refresh) | any request |
| `ConfiguredFiles` | saved harness changes | save-back (every path), `harness edit` of a saved file; cleared by reset | seed, refresh |
| `FileOverrides` | a person | `fileOverrides` on create/update, `harness edit` of a bundle file; cleared only by `reset --include-overrides` | seed, refresh, save-back, a plain reset |

At each path a sandbox is given the override, else the saved file, else the
bundle's. A save never writes an override (ADR 26-11-04-711 §2): what a box
wrote stays undoable by a plain reset, and at a path an override holds it is
shadowed until the person removes or edits theirs. The CLI sends a person's whole set back with only the named
paths changed (`--file`, `--remove-file`, `--clear-file`), under the revision
it read.

`FileOverrides` is NULL only on a config from before the set existed, whose
person-added files may still sit in `Files`. `splitFileOverrides` moves every
`Files` path the freshly read bundle doesn't declare into it, once, at the
first re-snapshot: `SeedBuiltIns` re-snapshots a built-in whose set is NULL
even when its digests haven't moved, and a refresh splits a config registered
by hand. Until then a write of the set is refused (409, naming the refresh),
since it would end the split. Every config created since holds a non-nil set,
so code that writes one must never write nil.

## Delivering a config's files to a sandbox

This package only stores the split; a later sandbox create resolves it into
one effective set by path, mirroring ADR 0012's `Files` overlay rule (image and
runtime entries merge by path, a matching path replaces):

- `reconciler.createOptionsFromSandbox` reads all three sets off the stored
  `HarnessConfig` onto `sandbox.ResolvedHarnessConfig`'s matching fields.
- `poolruntime.agent_client` forwards them across the server→pool-agent
  boundary as three fields on the wire (`pool-agent/api/openapi/pool.yaml`'s
  `ResolvedHarnessConfig` schema), since that boundary is an independently
  generated OpenAPI contract, not a shared Go type.
- `pool-agent/sandboxruntime.buildSandboxDocument` assigns the bundle baseline to
  `sandboxconfig.Document.Image.Files`, and the saved files followed by the
  overrides to `Document.Runtime.Files`, so `sandboxconfig.Effective`'s
  existing `mergeFiles(image, runtime, project)` does the actual overlay-by-path
  — a later runtime entry wins at a path — the same function that already
  merges a project's `.discobox/settings.json` `filesAdd` in
  (`pkg/sandboxconfig/effective.go`). No merge logic lives in this package.

## Routes on saved secrets

A bundle may declare routes on a secret (ADR 26-10-02-924 §2; see
[`harness/DESIGN.md`](../../../../harness/DESIGN.md)). They are snapshotted
with the secret declarations and written onto the secrets in
`ConfiguredSecretIDs` only; a secret a person bound to the config keeps the
routes that person gave it.

- A secret a save mints is created with its routes, so the rebind that
  follows hands running sandboxes a sentinel already held to them.
- `syncConfiguredSecretRoutes` appends independent restrictions to every owned saved secret when
  the bundle is re-read (`RefreshHarnessConfigImage`, `SeedBuiltIns`). It runs in one
  transaction with the config's snapshot and marks the sentinel delivery of
  the sandboxes holding a changed secret (`MarkSecretHoldersDirtyTx`), so the
  routes, the snapshot and their delivery commit together; see
  [`sandboxes/DESIGN.md`](../sandboxes/DESIGN.md).

## Reset

Reset (`ResetHarnessConfig`, `POST .../reset`; ADR 26-11-02-152 §3) returns a
config to its bundle's defaults: it deletes the secrets saved changes created
(`ConfiguredSecretIDs`, cascading their bindings, their holders marked) and
clears `ConfiguredFiles`, leaving the bundle baseline and any secret a person
bound to the config by hand. A person's `FileOverrides` are kept too, unless
the caller asks for them to go (`includeOverrides`, `admin harness reset
--include-overrides`; ADR 26-11-04-711 §1). It claims the row's revision first, so a save in
flight against the old overlay conflicts. Nothing about whether the harness can
run changes, so reset is allowed on the project default and on a harness with
nothing saved, which it leaves as it is. The refusal to delete a built-in
points at reset only when there is something saved for it to remove.

`UpdateHarnessConfig` can replace a person's set (`fileOverrides`) or the
saved one (`configuredFiles`), never the bundle's; that is how the CLI's file
editing (`harness edit`, and `f` on the console's harnesses screen) applies
hand edits. An edited saved file stays a saved file and is cleared on reset;
an edited bundle file becomes an override, which a plain reset keeps.

Deleting a default harness is fine: `store.DeleteHarnessConfig` clears the
pointer in the same transaction, and a project with no default refuses a create
that names no harness (ADR 0048) rather than resolving to one nobody chose.
Delete is refused (409) while any sandbox still references the config.

## Boundaries

- Project bindings remain control-plane state. Image metadata declares only
  secret requirements, never secret values.
- Service methods validate project scope and config input before writing through
  `internal/store`.
- Keep transport DTO conversion in `internal/handlers`; this package may use
  generated DTO aliases from `internal/services`.
- Harness settings files are literal. Declared credential JSON documents use
  exact `{"$secret":"ENV_NAME"}` references at schema-registered secret leaves;
  only the sandbox resolves these to current sentinels. There is no executable
  file templating or public-config rendering context.
- Launchers own directory trust and onboarding using the actual working
  directory. Shared preferences never carry those local paths.
- `SandboxRuntime` is this package's whole seam onto sandboxes, and it carries
  save-back's caller-authorized access (`AcquireSandboxHTTPClient`, so a save
  only works from inside a user request); server-authorized delivery of
  committed capture receipts; and config-change fan-outs that rebind
  credentials and upgrade bundle pins.

## Saving local harness configuration

A registered bundle's `SaveBack` definition is snapshotted with its manifest.
The agent reports a redacted preview and retains an immutable, bounded candidate;
only an authenticated user can ask this service to fetch its credential payload.
Preview reads observe running boxes without starting or repairing them, including
when a stop races a console listing. Private capture and acknowledgment calls use explicit agent scopes and are not
proxied as user routes.

A preview's file changes are described from the stored config, never from the
sandbox (`describeCapturePreview`): `previous` is the file a save replaces —
the override over the saved file over the bundle's, at the current revision —
and a removal's `file` is the one beneath the topmost set holding the path
(`fileBeneath`). A save always patches `ConfiguredFiles`, never an override
(ADR 26-11-04-711 §2): at a path an override holds the saved file is shadowed,
and the preview says so. Clients diff the
two; entries already present in the current saved state are omitted, comparing
compact JSON so JSON layout alone is not a change, preserving numeric precision. At overridden paths this
comparison uses the saved file over the bundle, so an override cannot hide a
real saved-file change beneath it. File presence and create-only behavior
remain meaningful. Credentials and pending handoffs are retained. The agent's
immutable candidate and its size limits are untouched by this projection.

`commitCapture` locks the config and bound secret rows, checks the candidate's
identity, manifest and registered definition, then commits a
patch to `ConfiguredFiles`, owned secrets, harness-config bindings, the origin's
sentinels (`MaterializeSandboxSecretsTx`), dirty marks and an idempotency
receipt in one transaction. The origin receives a sentinel for each newly
captured env unless its own binding of the variable wins; other boxes running
the config are marked and materialize it. Missing fields
leave remote state alone. Removing a settings override reveals the bundle baseline;
missing local credentials never delete shared secrets. Provider endpoints and
routes come from registered metadata. Hand-bound secrets survive replacement:
the config receives a new owned secret instead of modifying the user's secret.

The receipt contains the accepted baseline, never exported credential values.
A retry after a lost reply reads the receipt and retries acknowledgment rather
than replaying a potentially rotated OAuth token. Only after that durable commit
does the agent install sentinels. An acknowledgment failure reports
`saved-handoff-pending`; the config reconciler retries the pending receipt under
an explicit server-only write scope without waking a stopped origin or asking
for its credentials again. The receipt and dirty mark survive restart. An
unrelated settings or name edit still permits the accepted handoff with
current conditional revisions and the receipt's credential documents. A changed manifest,
harness or binding identity instead sends an explicit superseded acknowledgment:
the origin releases retained accepted credentials without installing stale
sentinels. The receipt is terminal only after the origin accepts that outcome.
An OAuth refresh of the same binding is safe. Local credentials remain usable
until handoff succeeds.
The originating harness and proxy can still race upstream OAuth refresh while
the harness caches real credentials (ADR 26-10-26-573).

Every config row update uses a revision compare-and-swap. The public editor
request carries the revision originally read, so loading the latest row at
request time cannot hide an intervening edit. Save-back instead locks and reads
the current rows and applies the chosen patch regardless of the candidate's
config or credential revisions (ADR 26-11-04-538). At a shared saved path or
captured credential the last commit wins; unrelated fields and personal
overrides survive. A credential marked keep retains the current binding and
value, including a renewal since capture. Receipt replay never reapplies it.
There is one save mode, with no force parameter or revision-conflict retry.

Provider destination policy follows ADR 26-11-04-772: capture and bundle refresh
select by delivered credential type, including numbered accounts, and append
independent `Secret.RouteConstraints` for owned secrets. Previous route lists
remain enforced. Manual bindings are unchanged. A provider marked `manualRoutes`
refuses new automatic capture and adds an empty denying list to an old owned
credential on refresh. The fixed diagnostic directs the person to bind a secret
with explicit endpoint routes. See [the inventory](../../../../harness/credential-destinations.md).
