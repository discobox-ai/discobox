// Codex's vendor operations for the shared runtime (harness/runtime). See
// DESIGN.md beside this file.
//
// Codex keeps one login in .codex/auth.json: an API key (OPENAI_API_KEY) or a
// ChatGPT sign-in (tokens), never both. Folder trust is its system layer's
// (/etc/codex/config.toml), so a later direct `codex` start sees it too.
import type {
  Adapter,
  Entry,
  File,
  OAuth,
  PlanItem,
  Provider,
  Stored,
  Vendor,
} from '../runtime/types.ts'

// A ChatGPT sign-in's account, as its delivered id_token carries it.
export interface Account {
  account: Record<string, string | boolean>
  accountId: string | null
}

// oauth is the secret a ChatGPT sign-in is saved as, and the account facts
// its delivered document needs.
export function oauth(
  auth: Vendor,
  provider: Provider,
): { secret: PlanItem['value'] & { subscriptionType?: string }; tokens: Account } {
  const tokens = auth.tokens || {}
  const accessToken = tokens.access_token
  const refreshToken = tokens.refresh_token
  if (
    typeof accessToken !== 'string' ||
    !accessToken ||
    typeof refreshToken !== 'string' ||
    !refreshToken
  ) {
    throw new Error('incomplete credential')
  }
  const decodeClaims = (jwt: unknown): Vendor => {
    if (typeof jwt !== 'string') return null
    const parts = jwt.split('.')
    if (parts.length !== 3 || !parts[1]) return null
    try {
      return JSON.parse(Buffer.from(parts[1], 'base64url').toString('utf8'))
    } catch (err) {
      return null
    }
  }
  const client = provider.oauth as OAuth
  const secret: { [key: string]: unknown; token: string; subscriptionType?: string } = {
    token: accessToken,
    refreshToken,
    tokenUrl: client.tokenUrl,
    clientId: client.clientId,
  }
  // The access token is a JWT, so its own expiry is on hand. Recording it
  // is what lets the control plane refresh just ahead of expiry instead of
  // discovering it from a 401.
  const accessClaims = decodeClaims(accessToken)
  if (accessClaims && typeof accessClaims.exp === 'number') {
    secret.accessTokenExpiresAt = accessClaims.exp * 1000
  }
  const idClaims = decodeClaims(tokens.id_token) || {}
  const authClaims = idClaims['https://api.openai.com/auth'] || {}
  if (typeof authClaims.chatgpt_plan_type === 'string' && authClaims.chatgpt_plan_type) {
    secret.subscriptionType = authClaims.chatgpt_plan_type
  }
  const account: Account['account'] = {}
  const email = idClaims.email || (idClaims['https://api.openai.com/profile'] || {}).email
  if (typeof email === 'string' && email) account.email = email
  for (const claim of ['chatgpt_plan_type', 'chatgpt_user_id', 'chatgpt_account_id']) {
    if (typeof authClaims[claim] === 'string' && authClaims[claim])
      account[claim] = authClaims[claim]
  }
  if (authClaims.chatgpt_account_is_fedramp === true) account.chatgpt_account_is_fedramp = true
  const accountId =
    typeof tokens.account_id === 'string' && tokens.account_id
      ? tokens.account_id
      : account.chatgpt_account_id || null
  return { secret, tokens: { account, accountId } }
}
// oauthAuthFile is the delivered .codex/auth.json for a saved ChatGPT sign-in.
export function oauthAuthFile(tokens: Account, envName = 'CODEX_OAUTH_TOKEN'): File {
  const b64url = (value: unknown) =>
    Buffer.from(JSON.stringify(value), 'utf8').toString('base64url')
  const claims: Record<string, unknown> = {}
  if (tokens.account.email) claims.email = tokens.account.email
  const authClaims: Record<string, unknown> = {}
  for (const claim of ['chatgpt_plan_type', 'chatgpt_user_id', 'chatgpt_account_id']) {
    if (tokens.account[claim]) authClaims[claim] = tokens.account[claim]
  }
  if (tokens.account.chatgpt_account_is_fedramp) authClaims.chatgpt_account_is_fedramp = true
  claims['https://api.openai.com/auth'] = authClaims
  const idToken = `${b64url({ alg: 'none', typ: 'JWT' })}.${b64url(claims)}.discobox`
  return {
    path: '.codex/auth.json',

    content:
      JSON.stringify(
        {
          OPENAI_API_KEY: null,
          tokens: {
            id_token: idToken,
            access_token: { $secret: envName },
            refresh_token: 'discobox-refresh-happens-in-the-control-plane',
            account_id: tokens.accountId,
          },
          last_refresh: '2100-01-01T00:00:00Z',
        },
        null,
        2,
      ) + '\n',
  }
}

const entries = (doc: Vendor): Entry[] =>
  doc.OPENAI_API_KEY
    ? [
        {
          provider: 'openai-key',
          value: { key: doc.OPENAI_API_KEY },
          localId: 'OPENAI_API_KEY',
          raw: doc.OPENAI_API_KEY,
        },
      ]
    : doc.tokens
      ? [
          {
            provider: 'openai',
            value: doc.tokens,
            localId: 'tokens',
            raw: doc.tokens,
          },
        ]
      : []
const document = (e: Pick<Stored, 'provider'>, raw: Vendor): Vendor =>
  e.provider === 'openai-key'
    ? { OPENAI_API_KEY: raw }
    : {
        OPENAI_API_KEY: null,
        tokens: raw,
        last_refresh: '2100-01-01T00:00:00Z',
      }

export default {
  entries,
  material: (e) => e.value.key || e.value.access_token,
  // A ChatGPT sign-in is always one the control plane renews.
  renewable: () => true,
  // One login at a time: a selected one replaces whatever the file held.
  singleCredential: true,
  compose: (values) => document(values[0], values[0].raw),

  login(e, doc, envName) {
    if (e.provider !== 'openai') return undefined
    const got = oauth(doc, e.prov)
    const entry = JSON.parse(oauthAuthFile(got.tokens, envName).content).tokens
    if (entry.account_id === null) delete entry.account_id
    return { value: got.secret, entry }
  },

  files: (documents, path) => {
    const t = documents[0]
    return [
      {
        path,
        content: JSON.stringify(document(t, t.entry), null, 2) + '\n',
      },
    ]
  },

  apply(doc, c, wanted) {
    if (c.provider === 'openai-key') return { OPENAI_API_KEY: wanted }
    doc.tokens = wanted
    doc.OPENAI_API_KEY = null
    doc.last_refresh = '2100-01-01T00:00:00Z'
    return doc
  },

  // The system layer owns folder trust, so a later direct `codex` start sees
  // it too. User preferences and portable capture never carry paths. Projects
  // go before any table header, so they stay a top-level inline TOML value.
  systemTrust(ctx) {
    const previous = ctx.previous === undefined ? {} : ctx.parse(ctx.previous, 'toml')
    const paths = new Set(Object.keys(previous.projects || {}))
    paths.add(ctx.cwd)
    const projects = [...paths]
      .sort()
      .map((path) => `${JSON.stringify(path)}={trust_level="trusted"}`)
      .join(',')
    return `projects={${projects}}\n${ctx.baseline}`
  },
} satisfies Adapter
