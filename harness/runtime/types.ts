// The contract between the shared runtime (runtime.ts) and a built-in's
// vendor adapter (harness/<dir>/adapter.ts), and the parts of a declaration
// (harness.json, harness.Declaration in Go) the runtime reads.

// Vendor is a document in a vendor's own shape: a CLI's credentials file or
// native export, parsed from JSON. Its shape is the adapter's to know, so the
// runtime never looks inside one.
// oxlint-disable-next-line typescript/no-explicit-any
export type Vendor = any

// A home-relative file the runtime exports or delivers.
export interface File {
  path: string
  content: string
  createOnly?: boolean
}

export interface OAuth {
  tokenUrl: string
  clientId: string
  encoding?: string
}

// A provider of the save-back definition (harnesscapture.Provider).
export interface Provider {
  id: string
  envName: string
  types: string[]
  host?: string
  oauth?: OAuth
  multipleAccounts?: boolean
  manualRoutes?: string
  routes?: unknown[]
  typeRoutes?: Record<string, unknown[]>
}

export interface SettingSpec {
  path: string
  format: 'json' | 'jsonc' | 'toml' | 'yaml'
  exclude?: string[]
  createOnly?: boolean
  fields?: Record<string, string>
}

// The save-back definition (harnesscapture.Definition).
export interface Definition {
  version: number
  watch: string[]
  providers: Provider[]
  settings: SettingSpec[]
  credentialFiles: string[]
  credentialSettings?: unknown[]
  exportCommand?: string[]
  handoffCommand?: string[]
}

export interface Store {
  strategy: 'none' | 'file' | 'native'
  none?: string
  document: string
  database?: string
  importFailure?: string
}

// The parts of a declaration the runtime reads.
export interface Declaration {
  id: string
  name: string
  install: { none?: string; executable?: string }
  environment: { fromSettings?: string }
  settings: { files?: File[]; policy?: { asset: string; path: string; layer: string }[] }
  localState: { trust: { strategy: 'none' | 'local-state' | 'system-layer'; layer?: string } }
  credentials: { store: Store }
  capture: Definition | { none: string }
}

// One login in a vendor document.
export interface Entry {
  provider: string
  value: Vendor
  localId: string
  raw: Vendor
  active?: boolean
}

// A login as a document stores it, which is all reconsideration needs.
export type Stored = Pick<Entry, 'provider' | 'raw'>

// A login the export accepted, with the provider and the token it spends.
export interface Accepted extends Entry {
  prov: Provider
  token: string
  knownName?: string
}

// A login's secret: the credential saved, and the entry delivered for it.
export interface PlanItem {
  provider: string
  envName: string
  type: string
  kept: boolean
  value?: { token?: unknown } & Record<string, unknown>
  entry?: Vendor
  host?: string
  notice?: string
}

export interface Planned extends PlanItem {
  source: Accepted
}

// A credential an export or a candidate carries.
export interface Credential {
  provider: string
  envName: string
  type: string
  localId: string
  generation: string
  account: string
  keep: boolean
  // The saved secret, in its provider's shape.
  value?: Vendor
}

export interface Sentinels {
  [envName: string]: string
}

// The input an export, a reconsideration or a handoff is given.
export interface Input {
  version?: number
  baselineOnly?: boolean
  baseline?: { files?: File[] }
  sentinels?: Sentinels
  knownSentinels?: Record<string, string[]>
  files?: File[]
  credentials?: Credential[]
  candidateId?: string
  reconsider?: Reconsider
}

export interface Reconsider {
  candidate: {
    id?: string
    credentials: Credential[]
    baseline?: { files?: File[] }
    files?: { path: string; remove?: boolean; file?: File }[]
  }
  replaceCredentials: string[]
  current: { files?: File[] }
}

export interface Diagnostic {
  path: string
  code: string
  message: string
}

export interface Export {
  version: number
  files: File[]
  credentials: Credential[]
  diagnostics: Diagnostic[]
  protectedPaths: string[]
}

export type HandoffState = 'applied' | 'local-conflict' | 'retryable-failure'

export interface CommandOptions {
  // Milliseconds; 0 is none.
  timeout?: number
}

// What the runtime hands an adapter: bounded helpers, never the sandbox's
// configuration.
export interface Helpers {
  REFRESH: string
  FAR_FUTURE: number
  HANDLED: symbol
  hash(value: unknown): string
  json(value: unknown): string
  reference(name: string): { $secret: string }
  read(path: string): string
  atomic(path: string, content: string, durable?: boolean): void
  safePath(path: string): string
  command(bin: string, args: string[], input: string, options?: CommandOptions): string
  resolveReferences(value: unknown, sentinels: Sentinels): Vendor
  cleanValue(value: Vendor): Record<string, unknown>
  parse(text: string, format: SettingSpec['format']): Vendor
}

export interface CaptureContext extends Helpers {
  input: Input
  baselineFile(path: string): string | undefined
  diagnostic(path: string, code: string): void
  protect(...paths: string[]): void
}

export interface PlanContext extends Helpers {
  normalizeAliases<T>(value: T): T
  previous: { files: File[]; secrets: { envName: string; type: string; host?: string }[] }
  sentinels: Sentinels
  definition: Definition
}

export interface ApplyContext extends Helpers {
  path: string
  second?: SecondDocument
}

export interface SecondDocument {
  path: string
  doc: Vendor
  have: Entry[]
}

export interface PrepareContext extends Helpers {
  // The working directory the launch prepares, as the kernel names it.
  cwd: string
  store: Store
}

export interface TrustContext extends Helpers {
  // The layer as it is, or undefined when it does not exist yet.
  previous: string | undefined
  baseline: string
  // The real path of the folder to trust.
  cwd: string
}

export interface Plan {
  plan: PlanItem[]
  skipped: string[]
}

// A built-in's vendor operations. Which are required follows from its
// declaration (operations in runtime.ts); the rest describe the vendor's
// documents where they differ from the default.
export interface Adapter {
  // The logins in a vendor document. Required with a capture definition.
  entries?(doc: Vendor): Entry[]
  // The secret a login spends; by default its key or access token.
  material?(entry: Entry): unknown
  // A vendor document holding these logins, for reconsideration.
  compose?(values: Stored[], path: string): Vendor
  // The delivered documents for the logins a capture saved.
  files?(documents: Planned[], path: string): File[]
  // Replace one login in the vendor document at handoff, returning the
  // document to write, or HANDLED when the adapter wrote its own.
  apply?(
    doc: Vendor,
    credential: Credential,
    wanted: Vendor,
    current: Entry,
    ctx: ApplyContext,
  ): Vendor

  // The secrets for a store of many providers' logins.
  plan?(accepted: Accepted[], ctx: PlanContext): Plan
  // The secret and delivered entry for a login the control plane renews.
  login?(
    entry: Accepted,
    doc: Vendor,
    envName: string,
  ): { value: PlanItem['value']; entry: Vendor } | undefined

  // A native store: export it, import a delivered document at launch, and
  // replace a login in it at handoff.
  exportNative?(ctx: Helpers): Vendor
  importNative?(ctx: PrepareContext): void
  importWanted?(ctx: PrepareContext): boolean
  handoffNative?(
    input: Input,
    credential: Credential,
    wanted: Vendor,
    doc: Vendor,
    ctx: Helpers,
  ): HandoffState

  // Local trust and onboarding (trust strategy local-state).
  prepareLocalState?(ctx: PrepareContext): void
  // The system layer trusting ctx.cwd (trust strategy system-layer).
  systemTrust?(ctx: TrustContext): string
  // Environment derived from Discobox's settings file (fromSettings).
  launchEnvironment?(settings: Vendor): Record<string, string>

  // Vendor document semantics.
  emptyDocument?: string
  optionalDocument?: boolean
  retiresLogin?: boolean
  singleCredential?: boolean
  renewable?(entry: Entry): boolean
  settings?(path: string, body: string): string
  signedOut?(entry: Entry, token: unknown): boolean
  unsupported?(entry: Entry): boolean
  moreEntries?(source: Entry[], ctx: CaptureContext): void
  documentEntries?(doc: Vendor, path: string): Stored[]
  reconsidered?(out: Export, current: File[]): void
  secondDocument?(credential: Credential, ctx: Helpers): SecondDocument | undefined
  wanted?(target: Vendor, credential: Credential): Vendor
}
