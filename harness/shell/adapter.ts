// The shell harness has no vendor operations: it installs no agent, so it has
// no launcher, credentials, hooks or capture for the shared runtime to run
// (harness.json says why for each). Its bundle carries no feature, and so
// not this file either.
import type { Adapter } from '../runtime/types.ts'

export default {} satisfies Adapter
