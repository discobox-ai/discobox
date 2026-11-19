// GitHub Copilot's vendor operations for the shared runtime (harness/runtime).
// See DESIGN.md beside this file.
//
// Copilot keeps its GitHub login in .copilot/config.json under authTokens,
// keyed by host and login. The saved login is delivered as an environment
// variable (COPILOT_GITHUB_TOKEN), never a file, so handoff retires the local
// token rather than replacing it, and nothing is delivered into the document.
import type { Adapter, Entry, Vendor } from '../runtime/types.ts'

export default {
  entries(state: Vendor): Entry[] {
    const user = state.lastLoggedInUser || {}
    if (typeof user.host !== 'string' || typeof user.login !== 'string') return []
    if (user.host.replace(/\/+$/, '') !== 'https://github.com') throw Error('unsupported host')
    const tokens = state.authTokens || {},
      key = `${user.host}:${user.login}`
    const localId = tokens[key] ? key : key + ':github',
      value = tokens[localId]
    if (!value || typeof value.token !== 'string' || !value.token) return []
    return [{ provider: 'github', value, raw: value, localId }]
  },
  material: (e) => e.value.token,
  retiresLogin: true,
  files: () => [],
  apply(doc, c, _wanted, current) {
    delete doc.authTokens[c.localId]
    const sibling = c.localId.endsWith(':github') ? c.localId.slice(0, -7) : c.localId + ':github'
    if (doc.authTokens[sibling]?.token === current.value.token) delete doc.authTokens[sibling]
    return doc
  },
} satisfies Adapter
