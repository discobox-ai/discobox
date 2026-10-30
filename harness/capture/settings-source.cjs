"use strict";
// Harness settings preserve ordinary fields; each manifest names the known
// credential and machine-local paths omitted by normalization.
function jsonc(text) {
  let out = "",
    quoted = false,
    escaped = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i],
      n = text[i + 1];
    if (quoted) {
      out += c;
      if (escaped) escaped = false;
      else if (c === "\\") escaped = true;
      else if (c === '"') quoted = false;
      continue;
    }
    if (c === '"') {
      quoted = true;
      out += c;
    } else if (c === "/" && n === "/") {
      while (i < text.length && text[i] !== "\n") i++;
      out += "\n";
    } else if (c === "/" && n === "*") {
      i += 2;
      while (i < text.length && !(text[i] === "*" && text[i + 1] === "/")) i++;
      if (i >= text.length) throw Error("comment");
      i++;
    } else out += c;
  }
  // Strip trailing commas only outside strings.
  let clean = "";
  quoted = false;
  escaped = false;
  for (let i = 0; i < out.length; i++) {
    const c = out[i];
    if (!quoted && c === "," && /^\s*[}\]]/.test(out.slice(i + 1))) continue;
    clean += c;
    if (escaped) escaped = false;
    else if (quoted && c === "\\") escaped = true;
    else if (c === '"') quoted = !quoted;
  }
  return JSON.parse(clean);
}
// Patterns name object paths, with * matching one segment and ** any depth.
// Literal dots in keys are not path separators: compare the actual key chain.
function matches(pattern, keys) {
  const parts = pattern.split(".");
  function match(i, j) {
    if (i === parts.length) return j === keys.length;
    if (parts[i] === "**")
      return match(i + 1, j) || (j < keys.length && match(i, j + 1));
    return j < keys.length && (parts[i] === "*" || parts[i] === keys[j]) && match(i + 1, j + 1);
  }
  return match(0, 0);
}
function parse(text, format) {
  if (format === "json") return JSON.parse(text);
  if (format === "jsonc") return jsonc(text);
  if (format === "toml") return require("smol-toml").parse(text);
  if (format === "yaml") return require("yaml").parse(text, { maxAliasCount: 100, uniqueKeys: true });
  throw Error("unsupported settings format");
}
function project(text, spec) {
  if (spec.fields && Object.keys(spec.fields).length)
    throw Error("legacy settings schema: refresh harness metadata");
  const source = parse(text, spec.format), unsupported = [];
  if (!source || typeof source !== "object" || Array.isArray(source))
    throw Error("object required");
  function walk(value, keys = []) {
    if (Array.isArray(value)) return value.map((v, i) => walk(v, [...keys, String(i)]));
    if (!value || typeof value !== "object" || value instanceof Date) return value;
    const out = Object.create(null);
    for (const [key, v] of Object.entries(value)) {
      if (["__proto__", "constructor", "prototype"].includes(key)) throw Error("unsafe key");
      const next = [...keys, key];
      if ((spec.exclude || []).some((pattern) => matches(pattern, next))) {
        // Keep only manifest-authored patterns in diagnostics, never arbitrary
        // local key names: even an object key could itself contain a secret.
        unsupported.push(...spec.exclude.filter((pattern) => matches(pattern, next)));
        continue;
      }
      out[key] = walk(v, next);
    }
    return out;
  }
  const out = walk(source);
  const content = spec.format === "toml"
    ? require("smol-toml").stringify(out)
    : JSON.stringify(out, null, 2) + "\n"; // JSON is also valid YAML 1.2.
  return { content, unsupported, empty: Object.keys(out).length === 0 };
}
module.exports = { parse, jsonc, project };
