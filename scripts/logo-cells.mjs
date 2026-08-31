// Converts the captured TUI mark into explicit-color cell data.
//
// cli/internal/tui/logo.chars is a terminal capture, and as captured it is not
// safe to replay: it paints with 16-color indices, which every terminal theme
// redefines, and it builds its solid areas out of inverse-video runs, which
// paint the glyph in whatever background the user's terminal happens to have.
// The mark therefore came out a different color on a themed terminal and came
// out speckled on a light one.
//
// This resolves both, once, into cli/internal/tui/logo.json:
//
//   - indices become the brand's own RGB, so the mark is the same purple
//     everywhere and the renderer can downsample it per terminal;
//   - inverse runs become an explicit background plus a foreground that is the
//     terminal's own ground. That second part matters: an inverse cell's glyph
//     is drawn in the terminal's background, so it carves a notch out of the
//     cell, and the notch is shape. Dropping it fattens the mark's blocks.
//
// The capture stays as provenance. Re-run this after re-capturing the mark:
//   node scripts/logo-cells.mjs

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const source = resolve(root, 'cli/internal/tui/logo.chars');
const target = resolve(root, 'cli/internal/tui/logo.json');

// The four color roles the capture uses, as the brand's values rather than as
// whatever the capturing terminal's palette held.
const PALETTE = {
  31: '#ff3b6b',
  35: '#8b2fd6', // the mark's shadow side
  95: '#f45cff', // the mark's lit side
  105: '#f45cff', // the same, as a background
};

const art = readFileSync(source, 'utf8')
  .replaceAll('\x1b[?25l', '')
  .replaceAll('\x1b[?25h', '')
  .replace(/^\n+|\n+$/g, '');

/** Walks one row, tracking the SGR state each character is drawn under. */
function* cells(row) {
  let fg = null;
  let bg = null;
  let inverse = false;

  for (const token of row.split(/(\x1b\[[0-9;]*m)/)) {
    const sgr = /^\x1b\[([0-9;]*)m$/.exec(token);
    if (sgr) {
      for (const code of sgr[1].split(';')) {
        if (code === '' || code === '0') {
          fg = bg = null;
          inverse = false;
        } else if (code === '7') {
          inverse = true;
        } else if (Number(code) >= 100) {
          bg = code;
        } else {
          fg = code;
        }
      }
      continue;
    }
    for (const char of token) {
      const cellFg = inverse ? bg : fg;
      const cellBg = inverse ? fg : bg;
      yield {
        char,
        // An absent foreground means the terminal's own ground, which is what
        // an inverse cell draws its glyph in. A space paints nothing whatever
        // its foreground, so that color is dropped here rather than after the
        // runs are built — otherwise identical runs stay split by a color that
        // never shows.
        f: char === ' ' || cellFg === null ? null : PALETTE[cellFg] ?? null,
        b: cellBg === null ? null : PALETTE[cellBg] ?? null,
      };
    }
  }
}

let width = 0;
const rows = art.split('\n').map((line) => {
  const runs = [];
  let column = 0;
  for (const cell of cells(line)) {
    column++;
    const last = runs[runs.length - 1];
    if (last && last.f === cell.f && last.b === cell.b) {
      last.t += cell.char;
    } else {
      runs.push({ t: cell.char, f: cell.f, b: cell.b });
    }
  }
  width = Math.max(width, column);
  // Trailing blanks carry nothing and only make the file bigger.
  while (runs.length && runs[runs.length - 1].f === null && runs[runs.length - 1].b === null) {
    runs.pop();
  }
  return runs.map((run) => {
    const out = { t: run.t };
    // A run of spaces with no background paints nothing, whatever foreground
    // it was captured under.
    if (run.f && run.t.trim() !== '') out.f = run.f;
    if (run.b) out.b = run.b;
    return out;
  });
});

// One row per line: small enough to diff, structured enough to read.
const body = rows.map((row) => `  ${JSON.stringify(row)}`).join(',\n');
writeFileSync(target, `{\n  "width": ${width},\n  "rows": [\n${body}\n  ]\n}\n`);
console.log(`${source} -> ${target} (${width}x${rows.length})`);
