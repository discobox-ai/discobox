// opencode's vendor operations for the shared runtime (harness/runtime). See
// DESIGN.md beside this file.
//
// opencode 2 keeps credentials as rows in its SQLite database, which only
// opencode writes (ADR 26-10-15-848 §3). What a harness config delivers is
// opencode's own interchange format, `opencode auth export` with each secret
// a reference, and the launcher imports it through opencode's supported
// commands; never SQL.

import fs from 'node:fs'
import path from 'node:path'

import type {
  Adapter,
  CommandOptions,
  Credential,
  Definition,
  File,
  HandoffState,
  Helpers,
  Input,
  OAuth,
  Plan,
  PlanItem,
  PrepareContext,
  Provider,
  Sentinels,
  Vendor,
} from '../runtime/types.ts'

// The previous run's delivered files and secrets, as normalize reads them.
export interface Previous {
  files?: File[]
  secrets?: { envName: string; type?: string; host?: string }[]
}

// One credential as opencode's interchange format holds it.
interface Stored {
  id: string
  integrationID: string
  label?: string
  active?: boolean
  value: Vendor
}

// normalize decides each stored credential's secret: kept or new.
export function normalize(
  stored: Vendor,
  previous: Previous = {},
  sentinels: Sentinels = {},
  definition: Definition,
): Plan {
  const farFuture = 4102444800000
  const placeholder = 'discobox-refresh-happens-in-the-control-plane'

  // The previous run's entries and secret types, by the env name each
  // entry's secret reference names.
  const action = /"\$secret"\s*:\s*"([A-Za-z_][A-Za-z0-9_]*)"/
  const previousEntries = new Map<string, Vendor>()
  try {
    for (const entry of JSON.parse(
      (previous.files || []).find((f) => f.path === '.config/discobox/opencode-credentials.json')
        ?.content || '[]',
    )) {
      const match = JSON.stringify(entry).match(action)
      if (match) previousEntries.set(match[1], entry)
    }
  } catch (err) {
    // A previous file that does not parse keeps nothing.
  }
  const previousTypes = new Map((previous.secrets || []).map((s) => [s && s.envName, s && s.type]))

  // oauthRefresh: the providers whose OAuth sign-in the control plane can
  // renew, and how. Each is a property of that provider's public OAuth
  // client — its token endpoint, client id, and request encoding — not of
  // any one sign-in, so they are fixed here rather than read from the
  // store. opencode 2 signs in with the same clients opencode 1 did
  // (measured on 2.0.29). A provider not listed is delivered as a plain
  // token.
  const oauthRefresh = Object.fromEntries(
    definition.providers
      .filter((p): p is Provider & { oauth: OAuth } => !!p.oauth)
      .map((p) => [p.id, { ...p.oauth, tokenRequestEncoding: p.oauth.encoding }]),
  )

  const byIntegration = new Map<string, Vendor[]>()
  for (const entry of stored) {
    if (!entry || typeof entry.integrationID !== 'string' || !entry.value) continue
    if (!byIntegration.has(entry.integrationID)) byIntegration.set(entry.integrationID, [])
    byIntegration.get(entry.integrationID)!.push(entry)
  }
  const materialOf = (value: Vendor) => (value.type === 'key' ? value.key : value.access)

  const envNames = new Set<string>()
  const envNameFor = (provider: string) => {
    const base =
      'OPENCODE_' +
      provider
        .toUpperCase()
        .replace(/[^A-Z0-9]+/g, '_')
        .replace(/^_+|_+$/g, '') +
      '_CREDENTIAL'
    let name = base
    for (let n = 2; envNames.has(name); n++) name = `${base}_${n}`
    envNames.add(name)
    return name
  }

  const plan: PlanItem[] = []
  for (const provider of [...byIntegration.keys()].sort()) {
    const envName = envNameFor(provider)
    const sentinel = sentinels[envName]
    // One credential per provider: the one opencode uses, which is the
    // active one when the user connected more than one account. A new
    // sign-in beats the seeded sentinel if both read as active, since
    // it is the one the user just made.
    const rank = (entry: Vendor) =>
      (entry.active ? 2 : 0) + (sentinel && materialOf(entry.value) === sentinel ? 0 : 1)
    const chosen = byIntegration
      .get(provider)!
      .reduce((best, entry) => (rank(entry) > rank(best) ? entry : best))
    const value = chosen.value
    const material = materialOf(value)
    if (typeof material !== 'string' || !material) {
      envNames.delete(envName)
      continue
    }
    if (sentinel && material === sentinel && previousEntries.has(envName)) {
      plan.push({
        envName,
        provider,
        kept: true,
        type: previousTypes.get(envName) || 'token',
        entry: previousEntries.get(envName),
      })
      continue
    }
    // Explicit JSON data binds this credential to its sandbox sentinel.
    const reference = { $secret: envName }
    const item: PlanItem = { envName, provider, kept: false, type: '' }
    let delivered
    if (value.type === 'key') {
      item.type = 'token'
      item.value = { token: value.key }
      delivered = { ...value, key: reference }
    } else {
      const spec = oauthRefresh[provider]
      if (spec && value.refresh && value.refresh !== value.access) {
        item.type = 'oauth'
        item.value = {
          token: value.access,
          refreshToken: value.refresh,
          tokenUrl: spec.tokenUrl,
          clientId: spec.clientId,
        }
        if (spec.tokenRequestEncoding) item.value.tokenRequestEncoding = spec.tokenRequestEncoding
        if (Number(value.expires) > 0) item.value.accessTokenExpiresAt = Number(value.expires)
        // The refresh token stays in the control plane, so a refresh
        // from inside a sandbox could not succeed — and does not need
        // to, since a sentinel does not go stale.
        delivered = {
          ...value,
          access: reference,
          refresh: placeholder,
          expires: farFuture,
        }
      } else {
        item.type = 'token'
        item.value = { token: value.access }
        // A provider that authenticates with the refresh field itself
        // (GitHub Copilot stored its token in both under opencode 1)
        // gets the sentinel there too; a real refresh token becomes
        // the placeholder.
        delivered = {
          ...value,
          access: reference,
          refresh:
            value.refresh === value.access
              ? reference
              : value.refresh
                ? placeholder
                : value.refresh,
        }
        const expires = Number(value.expires) || 0
        if (expires > 0 && expires < farFuture) {
          item.notice = `${provider}'s sign-in expires ${new Date(expires).toISOString().slice(0, 10)}, and Discobox cannot renew it. Connect it again and save the change after that, or connect it with an API key.`
        }
      }
    }
    // Made the image's own entry, under the id that tells it from one
    // the user connects by hand, as own() below makes it.
    item.entry = { integrationID: provider, value: delivered }
    plan.push(item)
  }
  return { plan, skipped: [] }
}

const DELIVERED = '.config/discobox/opencode-credentials.json'
const LEGACY_AUTH = '.local/share/opencode/auth.json'
// Every opencode command here runs its own standalone server, never the
// per-user background service.
const opencode = (ctx: Helpers, args: string[], input = '', options?: CommandOptions) =>
  ctx.command('opencode', [...args.slice(0, 2), '--standalone', ...args.slice(2)], input, options)

// own stamps each entry as the image's: a fixed id per provider, so a
// delivered credential is told apart from one the user connected by hand,
// and active, so it takes over from whatever its provider used before. This
// is the one place that id is made.
const ownedId = (integrationID: string) =>
  'cred_discobox_' + integrationID.replace(/[^A-Za-z0-9_-]/g, '_')
const own = (entries: { integrationID: string; value: Vendor }[]): Stored[] =>
  entries.map((e) => ({
    id: ownedId(e.integrationID),
    integrationID: e.integrationID,
    label: 'Discobox',
    active: true,
    value: e.value,
  }))

// jq's truth: everything but null and false.
const truthy = (x: unknown) => x !== null && x !== undefined && x !== false

// fromAuthJSON is opencode's own legacy import, entry for entry (measured on
// 2.0.29): opencode 2 runs the same mapping when it first opens a database
// opencode 1 made beside an auth.json, and not for a database it creates
// fresh. An OAuth sign-in's method is ChatGPT's browser login for openai, a
// device code for github-copilot, opencode and xai, and opencode's generic
// "oauth" for any other. A wellknown entry's token becomes a key; the URL
// opencode fetched configuration from is not carried.
function fromAuthJSON(auth: Vendor): Stored[] {
  const out: { integrationID: string; value: Vendor }[] = []
  for (const [key, v] of Object.entries(auth)) {
    if (!v || typeof v !== 'object' || Array.isArray(v)) continue
    const id = key.replace(/\/+$/, '')
    if (id === '') continue
    const e: Vendor = v
    let value: Vendor
    if (e.type === 'api') {
      value = { type: 'key', key: e.key ?? null }
      if (truthy(e.metadata)) value.metadata = e.metadata
    } else if (e.type === 'wellknown') {
      value = { type: 'key', key: e.token ?? null }
    } else if (e.type === 'oauth') {
      value = {
        type: 'oauth',
        methodID:
          id === 'openai'
            ? 'chatgpt-browser'
            : ['github-copilot', 'opencode', 'xai'].includes(id)
              ? 'device'
              : 'oauth',
        refresh: e.refresh ?? null,
        access: e.access ?? null,
        expires: e.expires ?? null,
      }
      if (truthy(e.accountId) || truthy(e.enterpriseUrl)) {
        value.metadata = {}
        if (truthy(e.accountId)) value.metadata.accountID = e.accountId
        if (truthy(e.enterpriseUrl)) value.metadata.enterpriseUrl = e.enterpriseUrl
      }
    } else continue
    out.push({ integrationID: id, value })
  }
  return own(out)
}

// delivered is the import entries the configuration delivered into this home:
// the interchange file, or a configuration made for opencode 1, which
// delivered its auth.json instead.
function delivered(ctx: PrepareContext): Stored[] {
  const exists = (p: string) => {
    try {
      return fs.statSync(ctx.safePath(p)).isFile()
    } catch {
      return false
    }
  }
  if (exists(DELIVERED)) {
    const doc = JSON.parse(ctx.read(DELIVERED))
    return Array.isArray(doc) ? doc : []
  }
  if (exists(LEGACY_AUTH)) return fromAuthJSON(JSON.parse(ctx.read(LEGACY_AUTH)))
  return []
}

// OpenCode import does not replace an existing native ID. Journal the
// acknowledged transition before logout so retry can distinguish our own
// destructive midpoint from an unrelated local logout. No secret enters the
// journal: its payload is only IDs, digests and the original active choice.
function handoffNative(
  input: Input,
  credential: Credential,
  wanted: Vendor,
  doc: Stored[],
  ctx: Helpers,
): HandoffState {
  const c = credential
  const { hash, json, read, atomic } = ctx
  const journal =
    '.config/discobox/opencode-handoff/' +
    hash([input.candidateId || c.generation, c.localId]) +
    '.json'
  const valueHash = hash(wanted.value)
  let progress
  try {
    progress = JSON.parse(read(journal))
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== 'ENOENT') return 'retryable-failure'
  }
  if (
    progress &&
    (progress.generation !== c.generation ||
      progress.valueHash !== valueHash ||
      progress.localId !== c.localId ||
      progress.provider !== c.provider)
  )
    return 'local-conflict'
  const others = (rows: Stored[]) =>
    hash(
      rows
        .filter((e) => e.integrationID === c.provider && e.id !== c.localId)
        .map((e) => {
          const { active, ...rest } = e
          return rest
        })
        .sort((a, b) => a.id.localeCompare(b.id)),
    )
  const refresh = () => JSON.parse(opencode(ctx, ['auth', 'export']))
  let current = doc.find((e) => e.id === c.localId && e.integrationID === c.provider)
  const isDelivered = () => current && hash(current.value) === valueHash
  if (isDelivered() && !progress) return 'applied'
  if (!progress) {
    if (!current || hash(current) !== c.generation) return 'local-conflict'
    progress = {
      generation: c.generation,
      valueHash,
      localId: c.localId,
      provider: c.provider,
      wasActive: !!current.active,
      others: others(doc),
    }
    atomic(journal, json(progress), true)
  }
  if (others(doc) !== progress.others) return 'local-conflict'
  if (current && !isDelivered()) {
    if (hash(current) !== c.generation) return 'local-conflict'
    opencode(ctx, ['auth', 'logout', c.provider, c.localId])
    doc = refresh()
    current = doc.find((e) => e.id === c.localId)
  }
  if (others(doc) !== progress.others) return 'local-conflict'
  if (!current) {
    opencode(
      ctx,
      ['auth', 'import'],
      json([{ ...wanted, id: c.localId, active: progress.wasActive }]),
    )
    doc = refresh()
    current = doc.find((e) => e.id === c.localId)
  }
  if (!isDelivered())
    return current && hash(current) !== c.generation ? 'local-conflict' : 'retryable-failure'
  if (others(doc) !== progress.others) return 'local-conflict'
  if (progress.wasActive && !current!.active) {
    opencode(ctx, ['auth', 'switch', c.provider, c.localId])
    doc = refresh()
    current = doc.find((e) => e.id === c.localId)
    if (!isDelivered() || !current!.active) return 'retryable-failure'
  }
  fs.unlinkSync(ctx.safePath(journal))
  return 'applied'
}

// importNative makes opencode's store hold exactly the delivered credentials
// among the ones the image owns (ADR 26-10-15-848 §3, §4). Every opencode
// command starts a standalone server of its own, which took 10 to 40 seconds
// each in a busy sandbox, so it runs only the ones that change something:
//
//   - What it last synced is kept beside the store. While that is still what
//     is delivered and the database is still there, a launch runs nothing. A
//     credential removed by hand in the sandbox therefore stays removed until
//     the configuration changes.
//   - A database not created yet holds nothing, so a first launch only
//     imports.
//   - A credential that differs is logged out and imported again, since
//     import skips an id it already has.
//   - Import makes a credential active only for a provider that has no active
//     one, and logging out a provider's active credential promotes another.
//     So a delivered credential beside any other of its provider's — opencode's
//     own import of the same auth.json in a sandbox that ran opencode 1, or one
//     connected by hand — is switched to, and one that is the provider's only
//     credential is not.
//
// Every step's failure is counted rather than fatal, so one credential that
// could not be changed does not stop the rest; any failure leaves what was
// synced unrecorded, so the next launch tries again.
function importNative(ctx: PrepareContext): void {
  const home = process.env.HOME!
  const data = path.join(process.env.XDG_DATA_HOME || path.join(home, '.local/share'), 'opencode')
  const synced = path.join(
    process.env.XDG_STATE_HOME || path.join(home, '.local/state'),
    'discobox/opencode-credentials.synced',
  )
  const database = path.join(data, 'opencode.db')
  const want = delivered(ctx)
  const wanted = JSON.stringify(want)
  const exists = (p: string) => {
    try {
      return fs.statSync(p).isFile()
    } catch {
      return false
    }
  }
  if (exists(database) && exists(synced) && fs.readFileSync(synced, 'utf8') === wanted) return
  const have: Stored[] = exists(database) ? JSON.parse(opencode(ctx, ['auth', 'export'])) : []
  const same = (a: unknown, b: unknown) => ctx.hash(a) === ctx.hash(b)
  let failed = false
  const run = (args: string[], input?: string) => {
    try {
      opencode(ctx, args, input)
    } catch {
      failed = true
    }
  }
  const stale = have.filter(
    (h) =>
      h.id.startsWith('cred_discobox_') &&
      !want.some((w) => w.id === h.id && same(w.value, h.value)),
  )
  for (const h of stale) run(['auth', 'logout', h.integrationID, h.id])
  const gone = new Set(stale.map((h) => h.id))
  const missing = want.filter((w) => !have.some((h) => h.id === w.id && !gone.has(h.id)))
  if (missing.length) run(['auth', 'import'], JSON.stringify(missing) + '\n')
  // Every delivered credential not already active whose provider has another
  // credential: just imported beside one, or left inactive by a switch that
  // failed at an earlier launch. Whether that other one is active is not
  // asked: logging out a provider's active credential makes opencode promote
  // another (measured on 2.0.29), so one re-imported in its place arrives
  // inactive.
  const kept = have.filter((h) => !gone.has(h.id))
  for (const w of want) {
    if (kept.some((h) => h.id === w.id && h.active)) continue
    if (!kept.some((h) => h.integrationID === w.integrationID && h.id !== w.id)) continue
    run(['auth', 'switch', w.integrationID, w.id])
  }
  if (failed) {
    fs.rmSync(synced, { force: true })
    throw Error('sync')
  }
  fs.mkdirSync(path.dirname(synced), { recursive: true })
  fs.writeFileSync(synced, wanted)
}

export default {
  emptyDocument: '[]',
  exportNative: (ctx) => JSON.parse(opencode(ctx, ['auth', 'export'])),
  entries: (doc) =>
    doc.map((e: Stored) => ({
      provider: e.integrationID,
      value: e.value,
      localId: e.id,
      raw: e,
      active: e.active,
    })),
  compose: (values) => values.map((e) => e.raw),

  plan(accepted, ctx) {
    const clean = accepted.map((e) => ({ ...e.raw, value: ctx.cleanValue(e.value) }))
    const result = normalize(
      ctx.normalizeAliases(clean),
      ctx.previous,
      ctx.sentinels,
      ctx.definition,
    )
    for (const item of result.plan)
      item.entry = {
        integrationID: item.provider,
        value: ctx.cleanValue(item.entry.value),
      }
    return result
  },
  files: (documents) => [
    {
      path: DELIVERED,
      content:
        JSON.stringify(
          documents.map((t) => ({
            ...t.entry,
            id: 'cred_discobox_' + t.provider,
            active: true,
            label: 'Discobox',
          })),
          null,
          2,
        ) + '\n',
    },
  ],

  handoffNative,
  importNative,

  // Web search is Discobox's setting, edited in the box and saved as a
  // harness change like opencode's own. opencode 2 searches the web with
  // every provider by default, so only "off" needs saying, and a permission
  // rule denying the tool is what removes it from what the model is offered.
  launchEnvironment: (settings): Record<string, string> =>
    settings && settings.webSearch === false
      ? { OPENCODE_CONFIG_CONTENT: '{"permission":{"websearch":"deny"}}' }
      : {},
} satisfies Adapter
