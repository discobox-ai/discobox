// pi's vendor operations for the shared runtime (harness/runtime). See
// DESIGN.md beside this file.
//
// pi keeps every provider's login in .pi/agent/auth.json, one entry per
// provider, as an API key or an OAuth login. Background save-back normalizes
// each into a secret of its own (normalize, below).

import type {
  Adapter,
  Definition,
  File,
  OAuth,
  Plan,
  PlanItem,
  Provider,
  Sentinels,
  Vendor,
} from '../runtime/types.ts'

// The previous run's delivered files and secrets, as normalize reads them.
export interface Previous {
  files?: File[]
  secrets?: { envName: string; type?: string; host?: string }[]
}

// normalize decides each login's secret: kept, when it still holds the
// sentinel delivered for a previous one, or new.
export function normalize(
  auth: Vendor,
  previous: Previous = {},
  sentinels: Sentinels = {},
  definition: Definition,
): Plan {
  const farFuture = 4102444800000
  const placeholder = 'discobox-refresh-happens-in-the-control-plane'
  const copilotAccess = 'discobox-copilot-token-is-exchanged-in-the-sandbox'

  // The previous run's entries and secrets, by the env name each entry's
  // secret reference names.
  const action = /"\$secret"\s*:\s*"([A-Za-z_][A-Za-z0-9_]*)"/
  const previousEntries = new Map<string, Vendor>()
  const kept = (previous.files || []).find((f) => f && f.path === '.pi/agent/auth.json')
  try {
    for (const entry of Object.values(JSON.parse(kept ? kept.content : '{}') || {})) {
      const match = JSON.stringify(entry).match(action)
      if (match) previousEntries.set(match[1], entry)
    }
  } catch (err) {
    // A previous file that does not parse keeps nothing.
  }
  const previousSecrets = new Map<string, { type?: string; host?: string }>(
    (previous.secrets || []).map((s): [string, { type?: string; host?: string }] => [
      s && s.envName,
      s || {},
    ]),
  )

  // oauthRefresh: the providers whose OAuth login the control plane can
  // renew, and how. Each is a property of that provider's public OAuth
  // client — its token endpoint, client id, and request encoding — not of
  // any one sign-in, so they are fixed here rather than read from the
  // file. pi signs in to these with the same clients the claude-code image
  // (and opencode's xAI) are refreshed for. A provider not listed is
  // delivered as a plain token.
  const oauthRefresh = Object.fromEntries(
    definition.providers
      .filter((p): p is Provider & { oauth: OAuth } => !!p.oauth)
      .map((p) => [p.id, { ...p.oauth, tokenRequestEncoding: p.oauth.encoding }]),
  )

  const envNameFor = (provider: string) =>
    'PI_' +
    provider
      .toUpperCase()
      .replace(/[^A-Z0-9]+/g, '_')
      .replace(/^_+|_+$/g, '') +
    '_CREDENTIAL'

  // notice says when a login nothing renews stops working, or that it
  // already has: a kept one as much as a new one, since a save
  // about something else would otherwise carry a dead login on in
  // silence.
  const notice = (provider: string, credential: Vendor) => {
    if (!credential || credential.type !== 'oauth') return undefined
    const expires = Number(credential.expires) || 0
    if (expires <= 0 || expires >= farFuture) return undefined
    const day = new Date(expires).toISOString().slice(0, 10)
    if (expires <= Date.now()) {
      return `${provider}'s sign-in expired ${day}, and Discobox cannot renew it. Sign in to it again, or with an API key.`
    }
    return `${provider}'s sign-in expires ${day}, and Discobox cannot renew it. Sign in again and save the change after that, or sign in with an API key.`
  }

  // copilotHost is the GitHub API host a Copilot login's GitHub token is
  // exchanged at, and so the one host its secret is sent to:
  // api.github.com, or api.<domain> for GitHub Enterprise.
  const copilotHost = (credential: Vendor) => {
    const domain =
      typeof credential.enterpriseUrl === 'string' && credential.enterpriseUrl
        ? credential.enterpriseUrl.replace(/^[a-z]+:\/\//i, '').replace(/\/.*$/, '')
        : 'github.com'
    return `api.${domain}`
  }

  const plan: PlanItem[] = []
  const skipped: string[] = []
  for (const provider of Object.keys(auth).sort()) {
    const value = auth[provider]
    if (!value || typeof value !== 'object') continue
    const copilot = provider === 'github-copilot' && value.type === 'oauth'
    // What the secret is: an API key, an OAuth login's access token, or
    // a Copilot login's GitHub token, which pi keeps as its refresh
    // token.
    const material =
      value.type === 'api_key'
        ? value.key
        : copilot
          ? value.refresh
          : value.type === 'oauth'
            ? value.access
            : undefined
    if (typeof material !== 'string' || !material) {
      // A sign-in with no key or token of its own — Bedrock's AWS
      // profile or credential chain — uses credentials this machine
      // keeps, which no sandbox has.
      skipped.push(
        `${provider} has no key or token of its own (it uses credentials kept on the machine pi runs on), so it is not kept. Sign in to it with a key instead.`,
      )
      continue
    }
    const envName = envNameFor(provider)
    const sentinel = sentinels[envName]
    if (sentinel && material === sentinel && previousEntries.has(envName)) {
      const secret = previousSecrets.get(envName) || {}
      const entry = previousEntries.get(envName)
      const type = secret.type || 'token'
      const item: PlanItem = { envName, provider, kept: true, type, entry }
      if (secret.host) item.host = secret.host
      if (type === 'token' && !copilot) item.notice = notice(provider, entry)
      plan.push(item)
      continue
    }
    if (value.type === 'api_key' && material.startsWith('!')) {
      skipped.push(
        `${provider}'s key is a command (${material.split(/\s/)[0]}…), which runs where pi runs, so it is not kept. Sign in with the key itself.`,
      )
      continue
    }
    if (provider === 'openai-codex' && value.type === 'oauth') {
      skipped.push(
        `openai-codex (a ChatGPT subscription) is not kept: pi reads the account from the sign-in's own token, which Discobox never hands a sandbox. Sign in to OpenAI with an API key instead.`,
      )
      continue
    }
    // Explicit JSON data binds this credential to its sandbox sentinel.
    const reference = { $secret: envName }
    const item: PlanItem = { envName, provider, kept: false, type: '' }
    if (value.type === 'api_key') {
      item.type = 'token'
      item.value = { token: value.key }
      item.entry = { ...value, key: reference }
    } else if (copilot) {
      // The GitHub token alone is the secret, bound to the one host it
      // is exchanged at. The Copilot token it buys lasts half an hour,
      // so pi exchanges the sentinel for a fresh one inside the
      // sandbox, through the proxy, before its first request.
      item.type = 'token'
      item.host = copilotHost(value)
      item.value = { token: value.refresh }
      item.entry = {
        ...value,
        access: copilotAccess,
        refresh: reference,
        expires: 0,
      }
    } else if (value.type === 'oauth') {
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
        item.entry = {
          ...value,
          access: reference,
          refresh: placeholder,
          expires: farFuture,
        }
      } else {
        item.type = 'token'
        item.value = { token: value.access }
        item.entry = {
          ...value,
          access: reference,
          refresh:
            value.refresh === value.access
              ? reference
              : value.refresh
                ? placeholder
                : value.refresh,
        }
        item.notice = notice(provider, value)
      }
    } else {
      continue
    }
    plan.push(item)
  }
  return { plan, skipped }
}

const AUTH = '.pi/agent/auth.json'

export default {
  entries: (doc) =>
    Object.entries(doc).map(([provider, value]) => ({
      provider,
      value,
      localId: provider,
      raw: value,
    })),
  // A Copilot login's secret is its GitHub token, which pi keeps as the
  // refresh token.
  material: (e) =>
    e.provider === 'github-copilot' && e.value.type === 'oauth'
      ? e.value.refresh
      : e.value.key || e.value.access,
  // pi reads a ChatGPT subscription's account from the sign-in's own token,
  // which no sandbox is given.
  unsupported: (e) => e.provider === 'openai-codex' && e.value.type === 'oauth',
  // pi's first-run release marker is bookkeeping, not user intent.
  settings(path, body) {
    if (path !== '.pi/agent/settings.json') return body
    const settings = JSON.parse(body)
    if (settings && typeof settings === 'object' && !Array.isArray(settings))
      delete settings.lastChangelogVersion
    return JSON.stringify(settings)
  },
  compose: (values) => Object.fromEntries(values.map((e) => [e.provider, e.raw])),

  plan(accepted, ctx) {
    const clean = Object.fromEntries(accepted.map((e) => [e.provider, ctx.cleanValue(e.value)]))
    const result = normalize(
      ctx.normalizeAliases(clean),
      ctx.previous,
      ctx.sentinels,
      ctx.definition,
    )
    for (const item of result.plan) item.entry = ctx.cleanValue(item.entry)
    return result
  },
  files: (documents) => [
    {
      path: AUTH,
      content:
        JSON.stringify(Object.fromEntries(documents.map((t) => [t.provider, t.entry])), null, 2) +
        '\n',
    },
  ],

  apply(doc, c, wanted) {
    doc[c.localId] = wanted
    return doc
  },
} satisfies Adapter
