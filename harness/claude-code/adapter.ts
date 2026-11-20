// Claude Code's vendor operations for the shared runtime (harness/runtime).
// See DESIGN.md beside this file.
//
// Claude Code keeps a subscription login in .claude/.credentials.json
// (claudeAiOauth) and an API key in .claude.json (primaryApiKey), the same
// file that holds its onboarding and folder trust.
import type { Adapter, Entry, File, OAuth, Provider, Vendor } from '../runtime/types.ts'

const LOCAL_STATE = '.claude.json'

// A subscription login as the control plane saves it.
export interface Login {
  token: string
  refreshToken: string
  tokenUrl: string
  clientId: string
  accessTokenExpiresAt?: number
  scopes?: string[]
  subscriptionType?: string
}

// oauth is the secret a subscription login is saved as: renewable by the
// control plane at the provider's token endpoint.
export function oauth(login: Vendor, provider: Provider): Login {
  if (
    typeof login.accessToken !== 'string' ||
    !login.accessToken ||
    typeof login.refreshToken !== 'string' ||
    !login.refreshToken
  ) {
    throw new Error('incomplete credential')
  }
  const client = provider.oauth as OAuth
  const out: Login = {
    token: login.accessToken,
    refreshToken: login.refreshToken,
    tokenUrl: client.tokenUrl,
    clientId: client.clientId,
  }
  if (typeof login.expiresAt === 'number') {
    out.accessTokenExpiresAt = login.expiresAt
  }
  // Copied verbatim, never defaulted: an absent scope list has to stay
  // absent so nothing downstream can mistake a guess for what the
  // authorization server actually granted.
  if (Array.isArray(login.scopes)) {
    const scopes = login.scopes.filter((s: unknown) => typeof s === 'string' && s)
    if (scopes.length) {
      out.scopes = scopes
    }
  }
  if (typeof login.subscriptionType === 'string' && login.subscriptionType) {
    out.subscriptionType = login.subscriptionType
  }
  return out
}

// credentialsFile is the delivered .claude/.credentials.json for a saved
// subscription login.
export function credentialsFile(
  login: Pick<Login, 'scopes' | 'subscriptionType'>,
  envName = 'CLAUDE_CODE_OAUTH_TOKEN',
): File {
  return {
    path: '.claude/.credentials.json',
    content: JSON.stringify(
      {
        claudeAiOauth: {
          // Only this credential leaf is bound to a sentinel.
          accessToken: { $secret: envName },
          refreshToken: 'discobox-refresh-happens-in-the-control-plane',
          expiresAt: 4102444800000,
          scopes: login.scopes,
          subscriptionType: login.subscriptionType,
        },
      },
      null,
      2,
    ),
  }
}

const entries = (doc: Vendor): Entry[] =>
  doc.claudeAiOauth
    ? [
        {
          provider: 'anthropic',
          value: doc.claudeAiOauth,
          localId: 'claudeAiOauth',
          raw: doc.claudeAiOauth,
        },
      ]
    : []

export default {
  // Signing out removes the credentials file.
  optionalDocument: true,
  entries,
  material: (e) => e.value.key || e.value.accessToken,
  // A subscription login is always one the control plane renews.
  renewable: () => true,

  // The API key is the second document's.
  moreEntries(source, ctx) {
    try {
      const state = ctx.input.baselineOnly
        ? ctx.resolveReferences(
            JSON.parse(ctx.baselineFile(LOCAL_STATE) || '{}'),
            ctx.input.sentinels || {},
          )
        : JSON.parse(ctx.read(LOCAL_STATE))
      if (state.primaryApiKey)
        source.push({
          provider: 'anthropic-key',
          value: { key: state.primaryApiKey },
          localId: 'primaryApiKey',
          raw: state.primaryApiKey,
        })
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== 'ENOENT') {
        ctx.diagnostic(LOCAL_STATE, 'partial')
        ctx.protect(LOCAL_STATE)
      }
    }
  },
  documentEntries: (doc, path) =>
    path === LOCAL_STATE
      ? doc.primaryApiKey
        ? [{ provider: 'anthropic-key', raw: doc.primaryApiKey }]
        : []
      : entries(doc),
  compose: (values, path) =>
    path === LOCAL_STATE
      ? { primaryApiKey: values[0].raw, hasCompletedOnboarding: true }
      : { claudeAiOauth: values[0].raw },
  // Preserve a literal onboarding-only document without inventing a login.
  reconsidered(out, current) {
    if (!out.files.some((f) => f.path === LOCAL_STATE)) {
      const seed = current.find((f) => f.path === LOCAL_STATE)
      if (seed) out.files.push(seed)
    }
  },

  // Claude Code clears both tokens after signing out. There is no local
  // login to preserve, so let the file installer restore delivery.
  signedOut: (e, token) =>
    e.provider === 'anthropic' &&
    (token === '' || token == null) &&
    (e.value.refreshToken === '' || e.value.refreshToken == null),

  login(e, _doc, envName) {
    if (e.provider !== 'anthropic') return undefined
    const value = oauth(e.value, e.prov)
    return {
      value: { ...value },
      entry: JSON.parse(credentialsFile(value, envName).content).claudeAiOauth,
    }
  },

  files: (documents, path) =>
    documents.map((t) => ({
      path: t.provider === 'anthropic' ? path : LOCAL_STATE,
      ...(t.provider === 'anthropic-key' ? { createOnly: true } : {}),
      content:
        JSON.stringify(
          t.provider === 'anthropic'
            ? { claudeAiOauth: t.entry }
            : { primaryApiKey: t.entry, hasCompletedOnboarding: true },
          null,
          2,
        ) + '\n',
    })),

  secondDocument(c, ctx) {
    if (c.provider !== 'anthropic-key') return undefined
    const doc = JSON.parse(ctx.read(LOCAL_STATE))
    return {
      path: LOCAL_STATE,
      doc,
      have: [
        {
          localId: 'primaryApiKey',
          raw: doc.primaryApiKey,
          value: { key: doc.primaryApiKey },
          provider: c.provider,
        },
      ],
    }
  },
  wanted: (target, c) =>
    c.provider === 'anthropic-key'
      ? target.primaryApiKey
      : entries(target).find((e) => e.provider === c.provider)?.raw,
  apply(doc, _c, wanted, _current, ctx) {
    if (ctx.second) {
      ctx.second.doc.primaryApiKey = wanted
      ctx.atomic(ctx.path, ctx.json(ctx.second.doc))
      return ctx.HANDLED
    }
    doc.claudeAiOauth = wanted
    return doc
  },

  // Trust and onboarding are sandbox-local state. Merge only owned fields;
  // user preferences and a newer local login remain untouched.
  prepareLocalState(ctx) {
    let state: Vendor = {}
    try {
      state = JSON.parse(ctx.read(LOCAL_STATE))
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== 'ENOENT') throw e
    }
    if (!state || typeof state !== 'object' || Array.isArray(state))
      throw Error('invalid local state')
    // Install the API-key sentinel from the same current context the agent
    // publishes for credential import guards, preserving a newer local login.
    let context: Vendor = {}
    try {
      context = JSON.parse(ctx.read('.config/discobox/harness-sentinels.json'))
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== 'ENOENT') throw e
    }
    const key = context.sentinels?.ANTHROPIC_API_KEY
    if (
      key &&
      (!state.primaryApiKey ||
        (context.knownSentinels?.ANTHROPIC_API_KEY || []).includes(state.primaryApiKey))
    )
      state.primaryApiKey = key
    state.hasCompletedOnboarding = true
    state.hasSeenAutoDefaultNudge = true
    state.projects ||= {}
    state.projects[ctx.cwd] = {
      ...state.projects[ctx.cwd],
      hasTrustDialogAccepted: true,
    }
    ctx.atomic(LOCAL_STATE, ctx.json(state))
  },
} satisfies Adapter
