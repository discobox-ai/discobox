#!/usr/bin/env node
// Renders assets/brand/tokens.json as assets/brand/tokens.css: the dark theme
// on :root (canonical), the light theme under prefers-color-scheme and an
// explicit data-theme="light", and dark again under data-theme="dark", so an
// explicit choice wins in both directions. LANGUAGE.md says what each token is for.
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const tokens = JSON.parse(readFileSync(join(root, "assets/brand/tokens.json"), "utf8"));

const dark = [], light = [], fixed = [];
for (const [name, t] of Object.entries(tokens.color)) {
  dark.push(`  --${name}: ${t.$value};`);
  light.push(`  --${name}: ${t.$extensions["ai.discobox"].light};`);
}
const family = (list) => list.map((f) => (/\s/.test(f) && !f.startsWith("-") ? `"${f}"` : f)).join(", ");
for (const [name, t] of Object.entries(tokens.font)) fixed.push(`  --font-${name}: ${family(t.$value)};`);
for (const group of ["size", "space", "radius", "stroke"]) {
  for (const [name, t] of Object.entries(tokens[group])) fixed.push(`  --${name}: ${t.$value};`);
}

const css = `/*
 * The discobox design language's tokens, generated from tokens.json by
 * scripts/brand-tokens.mjs (go tool task brand:tokens). Do not edit; change
 * tokens.json. LANGUAGE.md says what each token is for.
 *
 * Dark is canonical and sits on :root. Light applies when the system prefers
 * it and nothing chose otherwise, or when data-theme="light" is set.
 */
:root {
  color-scheme: dark;
${dark.join("\n")}
  --glow: color-mix(in oklab, var(--magenta) 35%, transparent);
${fixed.join("\n")}
}

@media (prefers-color-scheme: light) {
  :root:not([data-theme="dark"]) {
    color-scheme: light;
${light.map((l) => "  " + l).join("\n")}
  }
}

:root[data-theme="light"] {
  color-scheme: light;
${light.join("\n")}
}

:root[data-theme="dark"] {
  color-scheme: dark;
${dark.join("\n")}
}

/*
 * A surface that depicts a terminal (a terminal window, a capture of the
 * console) stays dark in both themes: put .theme-dark on it, and everything
 * inside reads the dark roles. .theme-light is the mirror, for a light card
 * on a dark page.
 */
.theme-dark {
  color-scheme: dark;
${dark.join("\n")}
  color: var(--text);
}

.theme-light {
  color-scheme: light;
${light.join("\n")}
  color: var(--text);
}
`;
writeFileSync(join(root, "assets/brand/tokens.css"), css);
