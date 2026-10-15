// Writes the use-case experiment's scenarios as judge-eval cases, one
// directory per style, so discobox-judge-eval scores each style on its own:
//
//   node test/judge-evals/use-cases/generate.mjs OUT_DIR [--uses FILE]
//
// writes OUT_DIR/command/*.json, OUT_DIR/use-case/*.json and
// OUT_DIR/no-never/*.json, the same case names in each. no-never is the
// use-case style with each use's closing "Never …" sentence taken off: what a
// use says when its writer did not rule out the neighbors of its writes.
// --uses adds a fourth style, "authored", from a JSON file mapping a
// scenario's name to the uses an agent wrote for its task (see author.sh). Nothing says which authored use a job would run under, so a job
// is written once per authored use, as NAME--uN, and score.mjs counts it as
// allowed under whichever use allows it most.
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { scenarios } from './scenarios.mjs';

const args = process.argv.slice(2);
const out = args[0];
if (!out || out.startsWith('-')) {
  console.error('usage: generate.mjs OUT_DIR [--uses FILE]');
  process.exit(2);
}
const usesFlag = args.indexOf('--uses');
const authored = usesFlag >= 0 ? JSON.parse(readFileSync(args[usesFlag + 1], 'utf8')) : null;

const styles = { command: 0, 'use-case': 1, 'no-never': 1 };

// withoutNever is a use with its closing "Never …" sentence taken off.
const withoutNever = (u) => u.replace(/\s*Never [^]*$/, '');
if (authored) styles.authored = 1;

function usesOf(scenario, style) {
  if (style === 'authored') return authored[scenario.name] ?? null;
  if (style === 'no-never') return scenario.uses['use-case'].map(withoutNever);
  return scenario.uses[style];
}

// under is each use a job runs under in a style, and the suffix that names it:
// the one its scenario names for that style, or every authored use.
function under(style, uses, index) {
  if (style !== 'authored') return [[uses[index], '']];
  if (uses.length === 1) return [[uses[0], '']];
  return uses.map((u, i) => [u, `--u${i + 1}`]);
}

let written = 0;
for (const [style, slot] of Object.entries(styles)) {
  const dir = join(out, style);
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  for (const s of scenarios) {
    const uses = usesOf(s, style);
    if (!uses) continue;
    const write = (name, why, expect, job) => {
      const c = { name: `${s.name}-${name}`, why, expect, job };
      writeFileSync(join(dir, `${c.name}.json`), JSON.stringify(c, null, 2) + '\n');
      written++;
    };
    const writeUnder = (index, name, why, expect, job) => {
      for (const [purpose, suffix] of under(style, uses, index)) write(name + suffix, why, expect, { ...job, purpose });
    };
    for (const j of s.jobs ?? []) {
      if (j.wrongUse && style === 'authored') continue;
      writeUnder(j.command[slot], j.name, `${s.name}, ${style} style: ${j.expect === 'refuse' ? 'past the task\'s lines' : 'work the task needs'}.`, j.expect, {
        kind: 'command',
        host: s.host,
        credential: s.credential,
        round: 1,
        command: j.argv,
      });
    }
    for (const r of s.requests ?? []) {
      writeUnder(r.command[slot], r.name, `${s.name}, ${style} style: a request the proxy observed.`, r.expect, {
        kind: 'request',
        host: r.host,
        credential: s.credential,
        round: r.round,
        request: r.request,
      });
    }
    for (const d of s.delegations ?? []) {
      // author.sh writes a scenario's own uses, not a worker's asks against a
      // lead's delegation, so the authored style has no delegation cases.
      if (style === 'authored') continue;
      const asks = style === 'no-never' ? d.asks['use-case'].map(withoutNever) : d.asks[style];
      write(d.name, `${s.name}, ${style} style: a worker's ask against the lead's delegation.`, d.expect, {
        kind: 'delegation',
        purposes: uses,
        host: s.host,
        credential: s.credential,
        round: 1,
        uses: asks,
      });
    }
  }
}
console.log(`wrote ${written} cases under ${out}`);
