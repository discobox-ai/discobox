// Validate the Mermaid diagrams embedded in Markdown files.
//
// mermaid.parse() runs the same grammars the renderer uses, so it catches
// syntax errors without a headless browser. Rendering is not attempted.
//
// Usage: node scripts/validate-mermaid.mjs [file.md ...]
// With no arguments every tracked Markdown file is checked.

import { readFile } from "node:fs/promises";
import { glob } from "node:fs/promises";

import { JSDOM } from "jsdom";

// mermaid sanitizes diagram text through DOMPurify, which needs a DOM. jsdom
// supplies one at import time; nothing is rendered.
const dom = new JSDOM("", { url: "http://localhost/" });
globalThis.window = dom.window;
globalThis.document = dom.window.document;
globalThis.DOMParser = dom.window.DOMParser;
globalThis.Node = dom.window.Node;

const { default: mermaid } = await import("mermaid");

const SKIP_DIRS = new Set([".git", "node_modules", "build"]);

async function markdownFiles(args) {
  if (args.length > 0) {
    return args;
  }
  const files = [];
  for await (const file of glob("**/*.md")) {
    if (file.split("/").some((part) => SKIP_DIRS.has(part))) {
      continue;
    }
    files.push(file);
  }
  return files.sort();
}

function diagrams(text) {
  const blocks = [];
  const pattern = /^[ \t]*```+[ \t]*mermaid[^\n]*\n([\s\S]*?)^[ \t]*```+[ \t]*$/gm;
  for (const match of text.matchAll(pattern)) {
    const line = text.slice(0, match.index).split("\n").length;
    blocks.push({ line, source: match[1] });
  }
  return blocks;
}

// No mermaid.initialize() call: configuring mermaid pulls in DOMPurify, which
// needs a DOM. Parsing alone runs fine on the defaults.
const failures = [];
let count = 0;

for (const file of await markdownFiles(process.argv.slice(2))) {
  const text = await readFile(file, "utf8");
  for (const { line, source } of diagrams(text)) {
    count++;
    try {
      await mermaid.parse(source);
    } catch (err) {
      failures.push(`${file}:${line}: ${err.message ?? err}`);
    }
  }
}

for (const failure of failures) {
  console.error(failure);
}

console.log(
  `checked ${count} Mermaid diagram(s), ${failures.length} invalid`,
);

if (failures.length > 0) {
  process.exit(1);
}
