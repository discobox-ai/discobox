"use strict";
// Deliberately small portable-settings grammar. Rich TOML/YAML constructs are
// diagnosed, never copied as text (they can contain credentials or commands).
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
function scalar(s, yaml) {
  s = s.trim();
  if (
    /^(true|false|-?\d+(\.\d+)?)$/.test(s) ||
    s.startsWith('"') ||
    s.startsWith("[")
  )
    return JSON.parse(s);
  if (/^'[^']*'$/.test(s)) return s.slice(1, -1);
  if (
    yaml &&
    /^[a-zA-Z0-9_./:@ -]+$/.test(s) &&
    !/^(null|yes|no|on|off)$/i.test(s)
  )
    return s;
  throw Error("unsupported scalar");
}
function parse(text, format) {
  if (format === "json" || (format === "yaml" && text.trim().startsWith("{")))
    return JSON.parse(text);
  if (format === "jsonc") return jsonc(text);
  const result = {};
  let section = [];
  const stack = [];
  for (const line of text.split("\n")) {
    if (!line.trim() || /^\s*#/.test(line)) continue;
    let m;
    if (
      format === "toml" &&
      (m = line.match(/^\s*\[([\w.-]+)\]\s*(?:#.*)?$/))
    ) {
      section = m[1].split(".");
      continue;
    }
    if (format === "toml") {
      m = line.match(/^\s*([\w.-]+)\s*=\s*(.+)$/);
      if (!m) throw Error("unsupported TOML");
      put(result, [...section, ...m[1].split(".")], scalar(m[2], false));
    } else {
      m = line.match(/^( *)([\w.-]+):\s*(.*)$/);
      if (!m) throw Error("unsupported YAML");
      while (stack.length && stack.at(-1).indent >= m[1].length) stack.pop();
      const keys = [...stack.map((x) => x.key), m[2]];
      if (!m[3]) stack.push({ indent: m[1].length, key: m[2] });
      else put(result, keys, scalar(m[3], true));
    }
  }
  return result;
}
function put(obj, keys, value) {
  for (const k of keys)
    if (["__proto__", "constructor", "prototype"].includes(k))
      throw Error("unsafe key");
  for (const k of keys.slice(0, -1)) {
    if (!obj[k]) obj[k] = {};
    if (typeof obj[k] !== "object" || Array.isArray(obj[k]))
      throw Error("duplicate");
    obj = obj[k];
  }
  obj[keys.at(-1)] = value;
}
function project(text, spec) {
  const source = parse(text, spec.format),
    out = {},
    fields = {},
    unsupported = [];
  if (!source || typeof source !== "object" || Array.isArray(source))
    throw Error("object required");
  function walk(obj, prefix = "", keys = []) {
    for (const [key, v] of Object.entries(obj)) {
      const field = prefix ? prefix + "." + key : key;
      if (v && typeof v === "object" && !Array.isArray(v))
        walk(v, field, [...keys, key]);
      else {
        const t = spec.fields[field];
        const good =
          t === "string-array"
            ? Array.isArray(v) && v.every((x) => typeof x === "string")
            : t && typeof v === t;
        if (good) {
          put(out, [...keys, key], v);
          fields[field] = v;
        } else {
          if (t) throw Error("invalid portable field type");
          unsupported.push(field);
        }
      }
    }
  }
  walk(source);
  let content;
  if (spec.format === "toml")
    content =
      Object.entries(fields)
        .map(([k, v]) => `${k} = ${JSON.stringify(v)}`)
        .join("\n") + "\n";
  else content = JSON.stringify(out, null, 2) + "\n"; // JSON is a YAML 1.2 subset.
  return { content, unsupported, empty: Object.keys(fields).length === 0 };
}
module.exports = { parse, jsonc, project };
