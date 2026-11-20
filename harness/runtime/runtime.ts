// The shared capture runtime (ADR 26-11-06-596 §3): the save-back export,
// reconsideration and acknowledged handoff every coding harness's capture
// command runs, the launcher's preparation under the capture lock, and the
// system-layer trust writer. It owns the transaction, locking discipline,
// safe file access, reference resolution and baseline projection; the vendor
// semantics of a harness's credential documents and native store are its
// adapter's (harness/<directory>/adapter.ts), selected by its declaration.
//
// main.ts is the command that runs it. discobox-harness-bundle compiles both,
// and every adapter, to ES modules when it stages a bundle.
import cp from 'node:child_process'
import crypto from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'
import { pathToFileURL } from 'node:url'
import { jsonc, parse, project } from './settings.mjs'
import type {
  Accepted,
  Adapter,
  CaptureContext,
  CommandOptions,
  Credential,
  Declaration,
  Definition,
  Entry,
  Export,
  File,
  HandoffState,
  Helpers,
  Input,
  PlanItem,
  Planned,
  PrepareContext,
  SecondDocument,
  Sentinels,
  Stored,
  Vendor,
} from './types.ts'

const REFRESH = 'discobox-refresh-happens-in-the-control-plane'
const FAR_FUTURE = 4102444800000
// HANDLED is what an adapter's apply returns when it wrote its own document.
const HANDLED = Symbol('handled')

// errno is a filesystem error's code, or undefined for anything else.
const errno = (e: unknown): string | undefined =>
  e && typeof e === 'object' && 'code' in e ? String((e as { code: unknown }).code) : undefined

const canonical = (x: unknown): unknown =>
  Array.isArray(x)
    ? x.map(canonical)
    : x && typeof x === 'object'
      ? Object.fromEntries(
          Object.keys(x)
            .sort()
            .map((k) => [k, canonical((x as Record<string, unknown>)[k])]),
        )
      : x
const hash = (x: unknown): string =>
  crypto
    .createHash('sha256')
    .update(JSON.stringify(canonical(x)))
    .digest('hex')
const reference = (name: string): { $secret: string } => ({ $secret: name })
const json = (x: unknown): string => JSON.stringify(x, null, 2) + '\n'
function safePath(p: string): string {
  const home = path.resolve(process.env.HOME ?? ''),
    absolute = path.resolve(home, p)
  if (!absolute.startsWith(home + path.sep)) throw Error('path')
  let cursor = home
  for (const part of p.split('/')) {
    cursor = path.join(cursor, part)
    try {
      if (fs.lstatSync(cursor).isSymbolicLink()) throw Error('symlink')
    } catch (e) {
      if (errno(e) !== 'ENOENT') throw e
    }
  }
  return absolute
}
function read(p: string): string {
  const f = safePath(p)
  const st = fs.statSync(f)
  if (!st.isFile() || st.size > 2 * 1024 * 1024) throw Error('file')
  return fs.readFileSync(f, 'utf8')
}
function atomic(p: string, content: string, durable = false): void {
  const f = safePath(p)
  fs.mkdirSync(path.dirname(f), { recursive: true, mode: 0o700 })
  const tmp = f + '.capture-' + crypto.randomBytes(8).toString('hex')
  try {
    fs.writeFileSync(tmp, content, { mode: 0o600, flag: 'wx' })
    if (durable) {
      const fd = fs.openSync(tmp, 'r')
      try {
        fs.fsyncSync(fd)
      } finally {
        fs.closeSync(fd)
      }
    }
    fs.renameSync(tmp, f)
    if (durable) {
      const fd = fs.openSync(path.dirname(f), 'r')
      try {
        fs.fsyncSync(fd)
      } finally {
        fs.closeSync(fd)
      }
    }
  } finally {
    try {
      fs.unlinkSync(tmp)
    } catch {}
  }
}
// command runs a vendor CLI with argv, never through a shell. The agent's
// export and handoff are bounded; prepare's context runs it with no timeout,
// as the launcher always ran an import: opencode's standalone server can take
// most of a minute to start in a busy box, and an import killed halfway would
// be retried at every launch.
function command(
  bin: string,
  args: string[],
  input: string,
  { timeout = 45000 }: CommandOptions = {},
): string {
  return cp.execFileSync(bin, args, {
    input,
    encoding: 'utf8',
    timeout,
    maxBuffer: 2 * 1024 * 1024,
    stdio: ['pipe', 'pipe', 'ignore'],
  })
}
function resolveReferences(value: unknown, sentinels: Sentinels): Vendor {
  if (
    value &&
    typeof value === 'object' &&
    !Array.isArray(value) &&
    Object.keys(value).length === 1 &&
    typeof (value as { $secret?: unknown }).$secret === 'string'
  ) {
    const sentinel = sentinels[(value as { $secret: string }).$secret]
    if (!sentinel) throw Error('missing credential binding')
    return sentinel
  }
  if (Array.isArray(value)) return value.map((v) => resolveReferences(v, sentinels))
  if (value && typeof value === 'object')
    return Object.fromEntries(
      Object.entries(value).map(([k, v]) => [k, resolveReferences(v, sentinels)]),
    )
  return value
}
function cleanValue(value: Vendor): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const k of [
    'type',
    'key',
    'access',
    'refresh',
    'expires',
    'accountId',
    'email',
    'projectId',
    'methodID',
  ])
    if (
      ['string', 'number'].includes(typeof value[k]) ||
      (['key', 'access', 'refresh'].includes(k) &&
        value[k] &&
        Object.keys(value[k]).length === 1 &&
        typeof value[k].$secret === 'string')
    )
      out[k] = value[k]
  if (value.metadata && typeof value.metadata === 'object') {
    const metadata: Record<string, string> = {}
    for (const key of ['accountID', 'email'])
      if (typeof value.metadata[key] === 'string') metadata[key] = value.metadata[key]
    if (Object.keys(metadata).length) out.metadata = metadata
  }
  return out
}
// The bounded helpers an adapter is handed; never the sandbox's configuration.
const helpers: Helpers = {
  REFRESH,
  FAR_FUTURE,
  HANDLED,
  hash,
  json,
  reference,
  read,
  atomic,
  safePath,
  command,
  resolveReferences,
  cleanValue,
  parse,
}

export interface Harness {
  id: string
  declaration: Declaration
  definition: Definition
  adapter: Adapter
  store: Declaration['credentials']['store']
  // Where the installed harness's files are; only an installed one has it.
  dir?: string
}

// operations are the adapter operations each declared strategy selects; an
// adapter missing one is refused when its harness is loaded, never discovered
// mid-transaction. A name joined by | is satisfied by any of them; a flag
// (retiresLogin) by being true.
export function operations(declaration: Declaration): string[] {
  const d = declaration
  const need: string[] = []
  // A login handoff retires rather than replaces is in no delivered
  // document, so there is nothing to compose for reconsideration.
  if (!('none' in d.capture)) need.push('entries', 'compose|retiresLogin', 'files')
  if (d.credentials.store.strategy === 'native') need.push('exportNative', 'importNative')
  if (d.credentials.store.strategy !== 'none' && !('none' in d.capture))
    need.push(d.credentials.store.strategy === 'native' ? 'handoffNative|apply' : 'apply')
  if (d.localState.trust.strategy === 'local-state') need.push('prepareLocalState')
  if (d.localState.trust.strategy === 'system-layer') need.push('systemTrust')
  if (d.environment.fromSettings) need.push('launchEnvironment')
  return need
}
// harness assembles one harness from its declaration, save-back definition
// and adapter.
export function harness(
  id: string,
  declaration: Declaration,
  definition: Definition,
  adapter: Adapter,
): Harness {
  const offered = adapter as Record<string, unknown>
  const missing = operations(declaration).filter(
    (op) =>
      !op.split('|').some((name) => typeof offered[name] === 'function' || offered[name] === true),
  )
  if (missing.length) throw Error('adapter lacks ' + missing.join(', '))
  return { id, declaration, definition, adapter, store: declaration.credentials.store }
}
// installed reads the harness whose label is metadata, beside its declaration
// and adapter.
export async function installed(id: string, metadata: string): Promise<Harness> {
  const dir = path.dirname(metadata)
  const definition: Definition = JSON.parse(fs.readFileSync(metadata, 'utf8')).harness.saveBack
  const declaration: Declaration = JSON.parse(
    fs.readFileSync(path.join(dir, id + '-harness.json'), 'utf8'),
  )
  const module: { default: Adapter } = await import(
    pathToFileURL(path.join(dir, id + '-adapter.mjs')).href
  )
  return { ...harness(id, declaration, definition, module.default), dir }
}
const native = (h: Harness): boolean => h.store.strategy === 'native'
const entries = (h: Harness, doc: Vendor): Entry[] => {
  if (!h.adapter.entries) throw Error('adapter lacks entries')
  return h.adapter.entries(doc)
}
const material = (h: Harness, e: Entry): unknown =>
  h.adapter.material ? h.adapter.material(e) : e.value.key || e.value.access

function load(h: Harness, input: Input): Vendor {
  const p = h.store.document
  if (input.baselineOnly)
    return resolveReferences(
      JSON.parse(
        (input.baseline?.files || []).find((f) => f.path === p)?.content ||
          h.adapter.emptyDocument ||
          '{}',
      ),
      input.sentinels || {},
    )
  if (native(h)) {
    if (!h.adapter.exportNative) throw Error('adapter lacks exportNative')
    return h.adapter.exportNative(helpers)
  }
  try {
    return jsonc(read(p))
  } catch (e) {
    // A CLI that removes its document on sign-out is signed out without one.
    if (h.adapter.optionalDocument && errno(e) === 'ENOENT') return {}
    throw e
  }
}
// Reminting changes a binding's current sentinel before every consumer has
// reread its file/store. Exact aliases remain references to that binding, never
// newly observed secret material. A collision across bindings fails closed.
function knownBindings(input: Pick<Input, 'sentinels' | 'knownSentinels'>): Map<string, string> {
  const known = new Map<string, string>()
  const add = (name: string, value: unknown) => {
    if (typeof value !== 'string' || !value) return
    if (known.has(value) && known.get(value) !== name) throw Error('ambiguous sentinel binding')
    known.set(value, name)
  }
  for (const [name, values] of Object.entries(input.knownSentinels || {}))
    for (const value of values || []) add(name, value)
  for (const [name, value] of Object.entries(input.sentinels || {})) add(name, value)
  return known
}
function previous(h: Harness, input: Input) {
  const definition = h.definition
  const names = knownBindings(input)
  const replace = (value: unknown): unknown => {
    if (typeof value === 'string' && names.has(value)) return reference(names.get(value) ?? '')
    if (Array.isArray(value)) return value.map(replace)
    if (value && typeof value === 'object')
      return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, replace(item)]))
    return value
  }
  const files = (input.baseline?.files || []).map((f) => {
    if (!definition.credentialFiles.includes(f.path)) return f
    try {
      return { ...f, content: json(replace(JSON.parse(f.content))) }
    } catch {
      return f
    }
  })
  const secrets: { envName: string; type: string; host?: string }[] = []
  for (const f of files) {
    if (!definition.credentialFiles.includes(f.path)) continue
    let rows: Entry[]
    try {
      rows = entries(h, JSON.parse(f.content))
    } catch {
      continue
    }
    for (const e of rows) {
      const name = (material(h, e) as { $secret?: unknown } | undefined)?.$secret
      if (typeof name !== 'string' || !name) continue
      const p = definition.providers.find((p) => p.id === e.provider)
      if (!p) continue
      // A login the control plane renews: the vendor's own document is
      // always one, otherwise the delivered placeholder says it is.
      const renewable =
        !!p.oauth &&
        (h.adapter.renewable
          ? h.adapter.renewable(e)
          : e.value.type === 'oauth' &&
            e.value.refresh === REFRESH &&
            e.value.expires === FAR_FUTURE)
      secrets.push({ envName: name, type: renewable ? 'oauth' : 'token', host: p.host })
    }
  }
  return { files, secrets }
}
// Reconsideration combines the immutable selected local entries with the
// current server documents. It never consults the working sandbox's files.
function reconsider(h: Harness, input: Input): Export {
  const { definition, adapter } = h
  const request = input.reconsider
  if (!request) throw Error('reconsider')
  const selected = new Map(
    request.candidate.credentials
      .filter((c) => !c.keep && request.replaceCredentials.includes(c.envName))
      .map((c) => [c.envName, c]),
  )
  const current = request.current.files || []
  // A replacement login usually keeps the existing delivery reference byte-for-
  // byte, so it is absent from the candidate's file diff. Reconstruct the
  // immutable local projection from delivered provenance plus that diff.
  const localByPath = new Map((request.candidate.baseline?.files || []).map((f) => [f.path, f]))
  for (const change of request.candidate.files || []) {
    if (change.remove) localByPath.delete(change.path)
    else if (change.file) localByPath.set(change.path, change.file)
  }
  const local = [...localByPath.values()]
  const combined: File[] = []
  const virtual: Sentinels = {}
  const found = new Set<string>()
  const envOf = (raw: unknown) => JSON.stringify(raw).match(/"\$secret"\s*:\s*"(\w+)"/)?.[1]
  // A harness whose handoff retires the local login keeps no delivered
  // document to reconsider.
  const documents = !adapter.retiresLogin
  for (const p of definition.credentialFiles) {
    if (!documents) continue
    const rows = (file: File | undefined): Stored[] => {
      if (!file) return []
      const doc = JSON.parse(file.content)
      return adapter.documentEntries ? adapter.documentEntries(doc, p) : entries(h, doc)
    }
    const byEnv = new Map<string, Stored>()
    for (const e of rows(current.find((f) => f.path === p))) {
      const name = envOf(e.raw)
      if (name) byEnv.set(name, e)
    }
    for (const e of rows(local.find((f) => f.path === p))) {
      const name = envOf(e.raw)
      if (!name || !selected.has(name)) continue
      if (adapter.singleCredential) byEnv.clear()
      byEnv.set(name, e)
      found.add(name)
    }
    if (!byEnv.size) continue
    for (const name of byEnv.keys()) virtual[name] = 'capture-reconsider-' + hash(name)
    if (!adapter.compose) throw Error('adapter lacks compose')
    combined.push({
      path: p,
      content: json(resolveReferences(adapter.compose([...byEnv.values()], p), virtual)),
    })
  }
  if (documents && [...selected.keys()].some((name) => !found.has(name)))
    throw Error('selected credential has no captured reference')
  const out = capture(h, {
    version: 1,
    baselineOnly: true,
    baseline: { files: combined },
    sentinels: virtual,
  })
  if (out.protectedPaths.length || out.diagnostics.length)
    throw Error('current credentials cannot be safely projected')
  out.files = out.files.filter((f) => definition.credentialFiles.includes(f.path))
  if (adapter.reconsidered) adapter.reconsidered(out, current)
  out.credentials = out.credentials.filter((c) => !selected.has(c.envName))
  out.credentials.push(...selected.values())
  return out
}
export function capture(h: Harness, input: Input): Export {
  if (input.reconsider) return reconsider(h, input)
  const { id, definition, adapter } = h
  const output: Export = {
    version: 1,
    files: [],
    credentials: [],
    diagnostics: [],
    protectedPaths: [],
  }
  const diagnostic = (p: string, code: string) => {
    output.diagnostics.push({
      path: p,
      code,
      message:
        code === 'partial'
          ? 'Input is incomplete or unreadable; retained locally.'
          : 'Unsupported fields or login retained locally.',
    })
  }
  const protect = (...paths: string[]) => output.protectedPaths.push(...paths)
  const baselineFile = (p: string) =>
    (input.baseline?.files || []).find((f) => f.path === p)?.content
  for (const spec of definition.settings) {
    let content: string | undefined
    try {
      content = input.baselineOnly ? baselineFile(spec.path) : read(spec.path)
      if (content === undefined) continue
      const normalize = (body: string) => {
        // The vendor's own bookkeeping in a settings file is not user intent.
        if (adapter.settings) body = adapter.settings(spec.path, body)
        // The bundled harness settings explain their editor in `about`.
        // Only this known metadata path and string field are descriptive;
        // Other ordinary preferences remain intact.
        if (spec.path === `.config/discobox/${id}-harness.json`) {
          const settings = JSON.parse(body)
          if (
            settings &&
            typeof settings === 'object' &&
            !Array.isArray(settings) &&
            typeof settings.about === 'string'
          )
            delete settings.about
          body = JSON.stringify(settings)
        }
        return project(body, spec)
      }
      const got = normalize(content)
      // Known credential/local exclusions are normalization, not warnings.
      // Their mere presence must not create a perpetual pending candidate.
      // Creating a vendor state file containing only changelog/cache fields is
      // not a portable settings edit. An empty projection is meaningful only
      // when it clears recognized settings from the delivered baseline.
      if (got.empty) {
        const baseline = baselineFile(spec.path)
        if (input.baselineOnly || baseline === undefined || normalize(baseline).empty) continue
      }
      output.files.push({ path: spec.path, content: got.content, createOnly: !!spec.createOnly })
    } catch (e) {
      if (errno(e) !== 'ENOENT') diagnostic(spec.path, 'partial')
    }
  }
  const p = h.store.document
  let doc: Vendor
  try {
    doc = load(h, input)
  } catch (e) {
    if (errno(e) !== 'ENOENT') {
      diagnostic(p, 'partial')
      protect(...definition.credentialFiles)
    }
    return output
  }
  let source: Entry[]
  try {
    source = entries(h, doc)
  } catch {
    diagnostic(p, 'partial')
    protect(...definition.credentialFiles)
    return output
  }
  // A login the vendor keeps in a second document.
  if (adapter.moreEntries) {
    const context: CaptureContext = { ...helpers, input, baselineFile, diagnostic, protect }
    adapter.moreEntries(source, context)
  }
  const known = knownBindings(input)
  const currentSentinels: Sentinels = { ...input.sentinels }
  for (const [value, name] of known) if (!currentSentinels[name]) currentSentinels[name] = value
  const normalizeAliases = (value: unknown): unknown => {
    if (typeof value === 'string' && known.has(value))
      return currentSentinels[known.get(value) ?? '']
    if (Array.isArray(value)) return value.map(normalizeAliases)
    if (value && typeof value === 'object')
      return Object.fromEntries(
        Object.entries(value).map(([key, item]) => [key, normalizeAliases(item)]),
      )
    return value
  }
  const accepted: Accepted[] = []
  for (const e of source) {
    const prov = definition.providers.find((p) => p.id === e.provider),
      token = material(h, e)
    // A login the CLI signed out of leaves nothing to preserve; the file
    // installer restores delivery.
    if (adapter.signedOut && adapter.signedOut(e, token)) continue
    if (prov?.manualRoutes && !known.has(token as string)) {
      diagnostic(p, 'endpoint-policy')
      protect(...definition.credentialFiles)
      continue
    }
    if (
      !prov ||
      typeof token !== 'string' ||
      !token ||
      token.startsWith('!') ||
      token.includes('{{') ||
      token.startsWith('discobox-') ||
      e.value.env ||
      e.value.enterpriseUrl ||
      e.value.apiEndpoint ||
      e.value.metadata?.enterpriseUrl ||
      !!(adapter.unsupported && adapter.unsupported(e))
    ) {
      diagnostic(p, 'unsupported')
      protect(...definition.credentialFiles)
      continue
    }
    const knownName = known.get(token)
    if (
      knownName &&
      knownName !== prov.envName &&
      !(prov.multipleAccounts && knownName.startsWith(prov.envName + '_'))
    ) {
      diagnostic(p, 'unsupported')
      protect(...definition.credentialFiles)
      continue
    }
    accepted.push({ ...e, prov, token, knownName })
  }
  let plans: PlanItem[] = []
  if (adapter.plan) {
    // A store of several providers' logins: the adapter decides each
    // login's secret, kept or new.
    const result = adapter.plan(accepted, {
      ...helpers,
      normalizeAliases: <T>(value: T): T => normalizeAliases(value) as T,
      previous: previous(h, input),
      sentinels: currentSentinels,
      definition,
    })
    plans = result.plan
    if (result.skipped.length) {
      diagnostic(p, 'unsupported')
      protect(...definition.credentialFiles)
    }
  } else
    for (const e of accepted) {
      const envName = e.prov.envName
      const item: PlanItem = {
        provider: e.provider,
        envName,
        type: e.prov.types[0],
        kept: !!e.knownName,
      }
      try {
        // A login the control plane renews is the vendor's to describe;
        // anything else is its token, delivered as a reference.
        const login = adapter.login && adapter.login(e, doc, envName)
        if (login) {
          item.value = login.value
          item.entry = login.entry
        } else {
          item.type = 'token'
          item.value = { token: e.token }
          item.entry = reference(envName)
        }
        plans.push(item)
      } catch {
        diagnostic(p, 'partial')
        protect(...definition.credentialFiles)
      }
    }
  const documents: Planned[] = []
  for (const item of plans) {
    const e = accepted.find(
      (e) =>
        e.provider === item.provider &&
        (item.kept ? e.knownName === item.envName : item.value?.token === e.token),
    )
    if (!e) continue
    const keep = !!e.knownName || !!input.baselineOnly
    const credential: Credential = {
      provider: item.provider,
      envName: item.envName,
      type: item.type,
      localId: e.localId,
      generation: hash(e.raw),
      account: e.value.accountId || e.value.metadata?.accountID || e.value.email || '',
      keep,
    }
    if (!keep) credential.value = item.value
    output.credentials.push(credential)
    documents.push({ ...item, source: e })
  }
  if (documents.length) {
    if (!adapter.files) throw Error('adapter lacks files')
    output.files.push(...adapter.files(documents, p))
  }
  output.protectedPaths = [...new Set(output.protectedPaths)]
  return output
}
// live reports whether the CLI runs in this home: a native store is in use,
// and replacing a login under it would race the CLI.
function live(h: Harness): boolean {
  const bin = h.declaration.install.executable
  for (const d of fs.readdirSync('/proc'))
    if (/^\d+$/.test(d) && +d !== process.pid)
      try {
        if (
          !fs
            .readFileSync('/proc/' + d + '/environ', 'utf8')
            .split('\0')
            .includes('HOME=' + process.env.HOME)
        )
          continue
        const args = fs.readFileSync('/proc/' + d + '/cmdline', 'utf8').split('\0')
        if (args.some((a) => path.basename(a) === bin)) return true
      } catch {}
  return false
}
export function handoff(h: Harness, input: Input): { state: HandoffState } {
  const { adapter } = h
  const finish = (state: HandoffState) => ({ state })
  const pending = (input.credentials || []).filter((c) => !c.keep)
  if (!pending.length) return finish('applied')
  if (native(h) && live(h)) return finish('retryable-failure')
  let doc: Vendor
  try {
    doc = load(h, { baselineOnly: false })
  } catch {
    return finish('retryable-failure')
  }
  let have = entries(h, doc)
  let changed = false
  for (const c of pending) {
    let p = h.store.document,
      second: SecondDocument | undefined
    // A login the vendor keeps in a second document is replaced there.
    if (adapter.secondDocument) {
      try {
        second = adapter.secondDocument(c, helpers)
      } catch {
        return finish('retryable-failure')
      }
      if (second) {
        p = second.path
        have = second.have
      }
    }
    const targetFile = (input.files || []).find((f) => f.path === p)
    let wanted: Vendor
    if (!adapter.retiresLogin) {
      if (!targetFile) return finish('retryable-failure')
      const target = resolveReferences(JSON.parse(targetFile.content), input.sentinels || {})
      wanted = adapter.wanted
        ? adapter.wanted(target, c)
        : entries(h, target).find((e) => e.provider === c.provider)?.raw
      if (wanted === undefined) return finish('retryable-failure')
    }
    if (adapter.handoffNative) {
      // A native store the adapter replaces a login in, journaled.
      let state: HandoffState
      try {
        state = adapter.handoffNative(input, c, wanted, doc, helpers)
      } catch {
        return finish('retryable-failure')
      }
      if (state !== 'applied') return finish(state)
      doc = load(h, { baselineOnly: false })
      have = entries(h, doc)
      continue
    }
    const current = have.find((e) => e.localId === c.localId)
    if (!current) {
      // A retired login already gone is retired.
      if (adapter.retiresLogin) continue
      return finish('local-conflict')
    }
    if (!adapter.retiresLogin && hash(current.raw) === hash(wanted)) continue
    if (hash(current.raw) !== c.generation) return finish('local-conflict')
    if (!adapter.apply) throw Error('adapter lacks apply')
    const next = adapter.apply(doc, c, wanted, current, { ...helpers, path: p, second })
    if (next === HANDLED) continue
    doc = next
    changed = true
  }
  if (changed && !native(h)) atomic(h.store.document, json(doc))
  return finish('applied')
}
// guard reports whether the delivered document may be imported into the
// native store: there is no store yet, or every login in the document is one
// Discobox delivered, so importing it replaces no local login.
export function guard(h: Harness): boolean {
  const { definition } = h
  try {
    fs.statSync(safePath(h.store.database ?? ''))
  } catch (e) {
    if (errno(e) === 'ENOENT') return true
    throw e
  }
  const p = h.store.document
  let delivered: string
  try {
    delivered = read(p)
  } catch (e) {
    if (errno(e) === 'ENOENT') return false
    throw e
  }
  // This user-readable context belongs only to launcher convergence. Capture
  // candidates never read it: their Keep authority comes from private input.
  let context: { sentinels: Sentinels; knownSentinels: Record<string, string[]> } = {
    sentinels: {},
    knownSentinels: {},
  }
  try {
    const raw = read('.config/discobox/harness-sentinels.json')
    if (Buffer.byteLength(raw) > 256 * 1024) return false
    context = JSON.parse(raw)
    if (context && context.knownSentinels == null) context.knownSentinels = {}
    const object = (x: unknown) => !!x && typeof x === 'object' && !Array.isArray(x)
    if (!object(context) || !object(context.sentinels) || !object(context.knownSentinels))
      return false
    if (
      Object.keys(context.sentinels).length > 128 ||
      Object.keys(context.knownSentinels).length > 128
    )
      return false
    if (Object.values(context.sentinels).some((v) => typeof v !== 'string' || !v)) return false
    if (
      Object.values(context.knownSentinels).some(
        (v) => !Array.isArray(v) || v.length > 128 || v.some((s) => typeof s !== 'string' || !s),
      )
    )
      return false
    knownBindings(context) // Reject alias collisions between bindings.
  } catch (err) {
    if (errno(err) !== 'ENOENT') return false
  }
  const doc = JSON.parse(delivered),
    sentinels: Sentinels = {},
    aliases: Record<string, string[]> = {}
  const allowed = (spec: Definition['providers'][number], name: string) =>
    name === spec.envName ||
    (spec.multipleAccounts &&
      name.startsWith(spec.envName + '_') &&
      /^(?:[2-9]|[1-9][0-9]+)$/.test(name.slice(spec.envName.length + 1)))
  for (const e of entries(h, doc)) {
    const spec = definition.providers.find((p) => p.id === e.provider)
    if (!spec) return false
    const value = material(h, e)
    let name = Object.keys(context.sentinels).find(
      (name) =>
        allowed(spec, name) &&
        (context.sentinels[name] === value ||
          (context.knownSentinels[name] || []).includes(value as string)),
    )
    if (!name) {
      name = spec.envName
      for (let n = 2; sentinels[name]; n++) name = spec.envName + '_' + n
      sentinels[name] = value as string
    } else {
      if (sentinels[name]) return false
      sentinels[name] = context.sentinels[name]
      aliases[name] = context.knownSentinels[name] || []
    }
  }
  const result = capture(h, {
    baseline: { files: [{ path: p, content: delivered }] },
    sentinels,
    knownSentinels: aliases,
  })
  return !result.protectedPaths.length && result.credentials.every((c) => c.keep)
}
// prepare runs before the CLI, under the capture lock, in its working
// directory: local trust and onboarding, the guarded import of a delivered
// login into a native store, and environment derived from the harness's own
// settings, which it returns for the launcher to export. Everything it runs is
// unbounded, as the launcher's steps always were.
export function prepare(h: Harness): string {
  const { declaration: d, adapter } = h
  const context: PrepareContext = {
    ...helpers,
    command: (bin, args, input, options) => command(bin, args, input, { timeout: 0, ...options }),
    cwd: process.cwd(),
    store: h.store,
  }
  if (d.localState.trust.strategy === 'local-state') {
    if (!adapter.prepareLocalState) throw Error('adapter lacks prepareLocalState')
    adapter.prepareLocalState(context)
  }
  if (native(h)) {
    // A store that cannot be shown to hold only what Discobox delivered is
    // left as it is: a local login is never replaced by an older document.
    let importing = false
    try {
      importing = (!adapter.importWanted || adapter.importWanted(context)) && guard(h)
    } catch {}
    if (importing)
      try {
        if (!adapter.importNative) throw Error('adapter lacks importNative')
        adapter.importNative(context)
      } catch {
        process.stderr.write('discobox: ' + h.store.importFailure + '\n')
      }
  }
  const env: Record<string, string> = {}
  if (d.environment.fromSettings) {
    let settings: Vendor
    try {
      settings = JSON.parse(read(d.environment.fromSettings))
    } catch {}
    if (!adapter.launchEnvironment) throw Error('adapter lacks launchEnvironment')
    Object.assign(env, adapter.launchEnvironment(settings))
  }
  let out = ''
  for (const [name, value] of Object.entries(env)) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name) || /[\n\0]/.test(value)) throw Error('environment')
    out += name + '=' + value + '\n'
  }
  return out
}
// trust rewrites the CLI's system layer, as root, to trust cwd beside every
// folder it already trusts, from the baseline the layer was installed from.
// The caller holds the layer's lock.
export function trust(h: Harness, cwd: string): void {
  const t = h.declaration.localState.trust
  const layer = t.layer ?? ''
  const policy = (h.declaration.settings.policy || []).find((p) => p.path === layer)
  if (!policy || !h.dir || !h.adapter.systemTrust) throw Error('trust')
  const baseline = fs.readFileSync(
    path.join(h.dir, h.id + '-system-config' + path.extname(policy.asset)),
    'utf8',
  )
  const previous = fs.existsSync(layer) ? fs.readFileSync(layer, 'utf8') : undefined
  const content = h.adapter.systemTrust({
    ...helpers,
    previous,
    baseline,
    cwd: fs.realpathSync(cwd),
  })
  if (previous !== content) {
    const temporary = layer + '.tmp'
    fs.writeFileSync(temporary, content, { mode: 0o644 })
    fs.renameSync(temporary, layer)
  }
}
export { errno, helpers, resolveReferences }
