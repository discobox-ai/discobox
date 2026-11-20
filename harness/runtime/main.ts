// The capture runtime's command (runtime.ts), which every coding harness's
// generated <id>-capture runs:
//
//   node main.mjs <id> <dir>/<id>-image.json export|handoff   (stdin: JSON)
//   node main.mjs <id> <dir>/<id>-image.json guard|prepare
//   node main.mjs <id> <dir>/<id>-image.json trust <cwd>       (as root)
//
// <dir> holds the harness's installed label, declaration (<id>-harness.json)
// and adapter (<id>-adapter.mjs).
import fs from 'node:fs'
import { capture, errno, guard, handoff, installed, prepare, trust } from './runtime.ts'
import type { Input } from './types.ts'

async function main(): Promise<void> {
  const [id, metadata, mode, ...rest] = process.argv.slice(2)
  const h = await installed(id, metadata)
  if (mode === 'prepare') {
    process.stdout.write(prepare(h))
    return
  }
  if (mode === 'trust') {
    trust(h, rest[0])
    return
  }
  if (mode === 'guard') {
    process.exitCode = guard(h) ? 0 : 1
    return
  }
  const raw = fs.readFileSync(0, 'utf8')
  if (raw.length > 2 * 1024 * 1024) throw Error('input size')
  const input: Input = JSON.parse(raw)
  if (input.version !== 1) throw Error('version')
  const result =
    mode === 'export' ? capture(h, input) : mode === 'handoff' ? handoff(h, input) : null
  if (!result) throw Error('mode')
  process.stdout.write(JSON.stringify(result))
}

await main().catch((error: unknown) => {
  // Never print exception messages or stacks: parsers and child processes
  // can embed credential contents. The agent accepts only these fixed codes.
  const reason =
    error instanceof SyntaxError
      ? 'invalid-json'
      : ['ENOENT', 'EACCES', 'EPERM'].includes(errno(error) ?? '')
        ? 'file-unavailable'
        : 'operation-failed'
  process.stderr.write(`discobox-capture: ${reason}\n`)
  if (process.argv.at(-1) === 'handoff') process.stdout.write('{"state":"retryable-failure"}')
  else process.exitCode = 1
})
