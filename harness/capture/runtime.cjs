"use strict";
const fs = require("fs"),
  path = require("path"),
  crypto = require("crypto"),
  cp = require("child_process");
const { project, jsonc } = require("./settings.cjs");
const REFRESH = "discobox-refresh-happens-in-the-control-plane";
const canonical = (x) =>
  Array.isArray(x)
    ? x.map(canonical)
    : x && typeof x === "object"
      ? Object.fromEntries(
          Object.keys(x)
            .sort()
            .map((k) => [k, canonical(x[k])]),
        )
      : x;
const hash = (x) =>
  crypto
    .createHash("sha256")
    .update(JSON.stringify(canonical(x)))
    .digest("hex");
const template = (name) => `{{ .secrets.${name} }}`;
const json = (x) => JSON.stringify(x, null, 2) + "\n";
const credentialPath = {
  pi: ".pi/agent/auth.json",
  omp: ".config/discobox/omp-credentials.json",
  opencode: ".config/discobox/opencode-credentials.json",
  codex: ".codex/auth.json",
  "claude-code": ".claude/.credentials.json",
  copilot: ".copilot/config.json",
};
function safePath(p) {
  const home = path.resolve(process.env.HOME),
    absolute = path.resolve(home, p);
  if (!absolute.startsWith(home + path.sep)) throw Error("path");
  let cursor = home;
  for (const part of p.split("/")) {
    cursor = path.join(cursor, part);
    try {
      if (fs.lstatSync(cursor).isSymbolicLink()) throw Error("symlink");
    } catch (e) {
      if (e.code !== "ENOENT") throw e;
    }
  }
  return absolute;
}
function read(p) {
  const f = safePath(p);
  const st = fs.statSync(f);
  if (!st.isFile() || st.size > 2 * 1024 * 1024) throw Error("file");
  return fs.readFileSync(f, "utf8");
}
function atomic(p, content, durable = false) {
  const f = safePath(p);
  fs.mkdirSync(path.dirname(f), { recursive: true, mode: 0o700 });
  const tmp = f + ".capture-" + crypto.randomBytes(8).toString("hex");
  try {
    fs.writeFileSync(tmp, content, { mode: 0o600, flag: "wx" });
    if (durable) {
      const fd = fs.openSync(tmp, "r");
      try {
        fs.fsyncSync(fd);
      } finally {
        fs.closeSync(fd);
      }
    }
    fs.renameSync(tmp, f);
    if (durable) {
      const fd = fs.openSync(path.dirname(f), "r");
      try {
        fs.fsyncSync(fd);
      } finally {
        fs.closeSync(fd);
      }
    }
  } finally {
    try {
      fs.unlinkSync(tmp);
    } catch {}
  }
}
function command(bin, args, input) {
  return cp.execFileSync(bin, args, {
    input,
    encoding: "utf8",
    timeout: 45000,
    maxBuffer: 2 * 1024 * 1024,
    stdio: ["pipe", "pipe", "ignore"],
  });
}
function load(id, input) {
  const p = credentialPath[id];
  if (input.baselineOnly)
    return JSON.parse(
      (input.baseline?.files || []).find((f) => f.path === p)?.content ||
        (id === "opencode" ? "[]" : "{}"),
    );
  if (id === "omp") return JSON.parse(command("omp", ["auth", "export"], ""));
  if (id === "opencode")
    return JSON.parse(
      command("opencode", ["auth", "export", "--standalone"], ""),
    );
  try {
    return jsonc(read(p));
  } catch (e) {
    if (id === "claude-code" && e.code === "ENOENT") return {};
    throw e;
  }
}
function entries(id, doc, normalizer) {
  if (id === "pi")
    return Object.entries(doc).map(([provider, value]) => ({
      provider,
      value,
      localId: provider,
      raw: value,
    }));
  if (id === "omp")
    return (doc.credentials || []).map((e) => ({
      provider: e.provider,
      value: e.credential,
      localId: JSON.stringify([
        e.provider,
        e.credential?.accountId || "",
        e.credential?.email || "",
      ]),
      raw: e,
    }));
  if (id === "opencode")
    return doc.map((e) => ({
      provider: e.integrationID,
      value: e.value,
      localId: e.id,
      raw: e,
      active: e.active,
    }));
  if (id === "claude-code")
    return doc.claudeAiOauth
      ? [
          {
            provider: "anthropic",
            value: doc.claudeAiOauth,
            localId: "claudeAiOauth",
            raw: doc.claudeAiOauth,
          },
        ]
      : [];
  if (id === "codex")
    return doc.OPENAI_API_KEY
      ? [
          {
            provider: "openai-key",
            value: { key: doc.OPENAI_API_KEY },
            localId: "OPENAI_API_KEY",
            raw: doc.OPENAI_API_KEY,
          },
        ]
      : doc.tokens
        ? [
            {
              provider: "openai",
              value: doc.tokens,
              localId: "tokens",
              raw: doc.tokens,
            },
          ]
        : [];
  const selected = normalizer.entry(doc);
  return selected ? [selected] : [];
}
function material(id, e) {
  const v = e.value;
  return id === "claude-code"
    ? v.key || v.accessToken
    : id === "codex"
      ? v.key || v.access_token
      : id === "copilot"
        ? v.token
        : id === "pi" && e.provider === "github-copilot" && v.type === "oauth"
          ? v.refresh
          : v.key || v.access;
}
function cleanValue(value) {
  const out = {};
  for (const k of [
    "type",
    "key",
    "access",
    "refresh",
    "expires",
    "accountId",
    "email",
    "projectId",
    "methodID",
  ])
    if (["string", "number"].includes(typeof value[k])) out[k] = value[k];
  if (value.metadata && typeof value.metadata === "object") {
    const metadata = {};
    for (const key of ["accountID", "email"])
      if (typeof value.metadata[key] === "string")
        metadata[key] = value.metadata[key];
    if (Object.keys(metadata).length) out.metadata = metadata;
  }
  return out;
}
// Reminting changes a binding's current sentinel before every consumer has
// reread its file/store. Exact aliases remain references to that binding, never
// newly observed secret material. A collision across bindings fails closed.
function knownBindings(input) {
  const known = new Map();
  const add = (name, value) => {
    if (typeof value !== "string" || !value) return;
    if (known.has(value) && known.get(value) !== name)
      throw Error("ambiguous sentinel binding");
    known.set(value, name);
  };
  for (const [name, values] of Object.entries(input.knownSentinels || {}))
    for (const value of values || []) add(name, value);
  for (const [name, value] of Object.entries(input.sentinels || {}))
    add(name, value);
  return known;
}
function previous(input, definition, id, normalizer) {
  const names = knownBindings(input);
  const replace = (value) => {
    if (typeof value === "string" && names.has(value))
      return template(names.get(value));
    if (Array.isArray(value)) return value.map(replace);
    if (value && typeof value === "object")
      return Object.fromEntries(
        Object.entries(value).map(([key, item]) => [key, replace(item)]),
      );
    return value;
  };
  const files = (input.baseline?.files || []).map((f) => {
    if (!definition.credentialFiles.includes(f.path)) return f;
    try {
      return { ...f, content: json(replace(JSON.parse(f.content))) };
    } catch {
      return f;
    }
  });
  const secrets = [];
  for (const f of files) {
    if (!definition.credentialFiles.includes(f.path)) continue;
    let rows;
    try {
      rows = entries(id, JSON.parse(f.content), normalizer);
    } catch {
      continue;
    }
    for (const e of rows) {
      const match = String(material(id, e)).match(
        /\{\{\s*\.secrets\.([\w]+)\s*\}\}/,
      );
      if (!match) continue;
      const p = definition.providers.find((p) => p.id === e.provider);
      if (!p) continue;
      const renewable =
        !!p.oauth &&
        (id === "claude-code" ||
          id === "codex" ||
          (e.value.type === "oauth" &&
            e.value.refresh === REFRESH &&
            e.value.expires === 4102444800000));
      secrets.push({
        envName: match[1],
        type: renewable ? "oauth" : "token",
        host: p.host,
      });
    }
  }
  return { files, secrets };
}
// Reconsideration combines the immutable selected local entries with the
// current server templates. It never consults the working sandbox's files.
function reconsider(id, definition, normalizer, input) {
  const request = input.reconsider;
  const selected = new Map(
    request.candidate.credentials
      .filter((c) => !c.keep && request.replaceCredentials.includes(c.envName))
      .map((c) => [c.envName, c]),
  );
  const current = request.current.files || [];
  // A replacement login usually keeps the existing delivery template byte-for-
  // byte, so it is absent from the candidate's file diff. Reconstruct the
  // immutable local projection from delivered provenance plus that diff.
  const localByPath = new Map(
    (request.candidate.baseline?.files || []).map((f) => [f.path, f]),
  );
  for (const change of request.candidate.files || []) {
    if (change.remove) localByPath.delete(change.path);
    else if (change.file) localByPath.set(change.path, change.file);
  }
  const local = [...localByPath.values()];
  const combined = [];
  const virtual = {};
  const found = new Set();
  const envOf = (raw) =>
    JSON.stringify(raw).match(/\{\{\s*\.secrets\.(\w+)\s*\}\}/)?.[1];
  for (const p of definition.credentialFiles) {
    if (id === "copilot") continue;
    const rows = (file, baseline) => {
      if (!file) return [];
      // This bundle seed is a Go template, including conditionals and a
      // working-directory key. Only its API-key binding participates here.
      if (id === "claude-code" && p === ".claude.json" && file.template) {
        const binding = file.content.match(
          /"primaryApiKey"\s*:\s*\{\{\s*\.secrets\.(\w+)\s*\|\s*json\s*\}\}/,
        );
        if (binding)
          return baseline.secretRevisions?.[binding[1]]
            ? [{ provider: "anthropic-key", raw: template(binding[1]) }]
            : [];
      }
      const doc = JSON.parse(file.content);
      if (id === "claude-code" && p === ".claude.json")
        return doc.primaryApiKey
          ? [{ provider: "anthropic-key", raw: doc.primaryApiKey }]
          : [];
      return entries(id, doc, normalizer);
    };
    const byEnv = new Map();
    for (const e of rows(current.find((f) => f.path === p), request.current)) {
      const name = envOf(e.raw);
      if (name) byEnv.set(name, e);
    }
    for (const e of rows(local.find((f) => f.path === p), request.candidate.baseline || {})) {
      const name = envOf(e.raw);
      if (!selected.has(name)) continue;
      if (id === "codex") byEnv.clear();
      byEnv.set(name, e);
      found.add(name);
    }
    if (!byEnv.size) continue;
    for (const name of byEnv.keys())
      virtual[name] = "capture-reconsider-" + hash(name);
    const values = [...byEnv.values()];
    let document;
    if (id === "pi")
      document = Object.fromEntries(values.map((e) => [e.provider, e.raw]));
    else if (id === "omp")
      document = { version: 1, credentials: values.map((e) => e.raw) };
    else if (id === "opencode") document = values.map((e) => e.raw);
    else if (id === "claude-code")
      document =
        p === ".claude.json"
          ? { primaryApiKey: values[0].raw, hasCompletedOnboarding: true }
          : { claudeAiOauth: values[0].raw };
    else if (id === "codex")
      document =
        values[0].provider === "openai-key"
          ? { OPENAI_API_KEY: values[0].raw }
          : {
              tokens: values[0].raw,
              OPENAI_API_KEY: null,
              last_refresh: "2100-01-01T00:00:00Z",
            };
    combined.push({
      path: p,
      content: json(document).replace(
        /\{\{\s*\.secrets\.(\w+)\s*\}\}/g,
        (_, name) => virtual[name],
      ),
    });
  }
  if (id !== "copilot" && [...selected.keys()].some((name) => !found.has(name)))
    throw Error("selected credential has no captured template");
  const out = capture(id, definition, normalizer, {
    version: 1,
    baselineOnly: true,
    baseline: { files: combined },
    sentinels: virtual,
  });
  if (out.protectedPaths.length || out.diagnostics.length)
    throw Error("current credentials cannot be safely projected");
  out.files = out.files.filter((f) =>
    definition.credentialFiles.includes(f.path),
  );
  // An OAuth-only config still carries the conditional API-key seed. Keep
  // that file verbatim without inventing a credential for an unbound branch.
  if (id === "claude-code" && !out.files.some((f) => f.path === ".claude.json")) {
    const seed = current.find((f) => f.path === ".claude.json");
    if (seed) out.files.push(seed);
  }
  out.credentials = out.credentials.filter((c) => !selected.has(c.envName));
  out.credentials.push(...selected.values());
  return out;
}
function capture(id, definition, normalizer, input) {
  if (input.reconsider) return reconsider(id, definition, normalizer, input);
  const output = {
    version: 1,
    files: [],
    credentials: [],
    diagnostics: [],
    protectedPaths: [],
  };
  const diagnostic = (p, code) => {
    output.diagnostics.push({
      path: p,
      code,
      message:
        code === "partial"
          ? "Input is incomplete or unreadable; retained locally."
          : "Unsupported fields or login retained locally.",
    });
  };
  for (const spec of definition.settings) {
    let content;
    try {
      content = input.baselineOnly
        ? (input.baseline?.files || []).find((f) => f.path === spec.path)
            ?.content
        : read(spec.path);
      if (content === undefined) continue;
      const normalize = (body, delivered = false) => {
        if (id === "codex" && spec.path === ".codex/config.toml") {
          // Normal exports receive unrendered delivery baselines. Remove only
          // the exact generated trust block, never evaluate template syntax or
          // accept it from a local file. Saved configs use the registered suffix
          // directly; the initial bundled config wraps it in this conditional.
          if (delivered && spec.templateSuffix) {
            const conditional =
              "{{- if .workingDir }}\n\n" +
              spec.templateSuffix +
              "{{- end }}\n";
            for (const suffix of [conditional, spec.templateSuffix]) {
              if (body.endsWith(suffix)) {
                body = body.slice(0, -suffix.length);
                break;
              }
            }
          }
        }
        // pi's first-run release marker is bookkeeping, not user intent.
        if (id === "pi" && spec.path === ".pi/agent/settings.json") {
          const settings = JSON.parse(body);
          if (
            settings &&
            typeof settings === "object" &&
            !Array.isArray(settings)
          )
            delete settings.lastChangelogVersion;
          body = JSON.stringify(settings);
        }
        // The bundled harness settings explain their editor in `about`.
        // Only this known metadata path and string field are descriptive;
        // Other ordinary preferences remain intact.
        if (spec.path === `.config/discobox/${id}-harness.json`) {
          const settings = JSON.parse(body);
          if (
            settings &&
            typeof settings === "object" &&
            !Array.isArray(settings) &&
            typeof settings.about === "string"
          )
            delete settings.about;
          body = JSON.stringify(settings);
        }
        return project(body, spec);
      };
      const got = normalize(content, !!input.baselineOnly);
      // Known credential/local exclusions are normalization, not warnings.
      // Their mere presence must not create a perpetual pending candidate.
      // Creating a vendor state file containing only changelog/cache fields is
      // not a portable settings edit. An empty projection is meaningful only
      // when it clears recognized settings from the delivered baseline.
      if (got.empty) {
        const baseline = (input.baseline?.files || []).find(
          (f) => f.path === spec.path,
        );
        if (
          input.baselineOnly ||
          !baseline ||
          normalize(baseline.content, true).empty
        )
          continue;
      }
      output.files.push({
        path: spec.path,
        content: got.content + (spec.templateSuffix || ""),
        ...(spec.templateSuffix ? { template: true } : {}),
        createOnly: !!spec.createOnly,
      });
    } catch (e) {
      if (e.code !== "ENOENT") diagnostic(spec.path, "partial");
    }
  }
  const p = credentialPath[id];
  let doc;
  try {
    doc = load(id, input);
  } catch (e) {
    if (e.code !== "ENOENT") {
      diagnostic(p, "partial");
      output.protectedPaths.push(...definition.credentialFiles);
    }
    return output;
  }
  let source;
  try {
    source = entries(id, doc, normalizer);
  } catch {
    diagnostic(p, "partial");
    output.protectedPaths.push(...definition.credentialFiles);
    return output;
  }
  if (id === "claude-code")
    try {
      const state = input.baselineOnly
        ? JSON.parse(
            (input.baseline?.files || []).find((f) => f.path === ".claude.json")
              ?.content || "{}",
          )
        : JSON.parse(read(".claude.json"));
      if (state.primaryApiKey)
        source.push({
          provider: "anthropic-key",
          value: { key: state.primaryApiKey },
          localId: "primaryApiKey",
          raw: state.primaryApiKey,
        });
    } catch (e) {
      if (e.code !== "ENOENT") {
        diagnostic(".claude.json", "partial");
        output.protectedPaths.push(".claude.json");
      }
    }
  const known = knownBindings(input);
  const currentSentinels = { ...input.sentinels };
  for (const [value, name] of known)
    if (!currentSentinels[name]) currentSentinels[name] = value;
  const normalizeAliases = (value) => {
    if (typeof value === "string" && known.has(value))
      return currentSentinels[known.get(value)];
    if (Array.isArray(value)) return value.map(normalizeAliases);
    if (value && typeof value === "object")
      return Object.fromEntries(
        Object.entries(value).map(([key, item]) => [
          key,
          normalizeAliases(item),
        ]),
      );
    return value;
  };
  const accepted = [];
  for (const e of source) {
    const prov = definition.providers.find((p) => p.id === e.provider),
      token = material(id, e);
    // Claude Code clears both tokens after signing out. There is no local
    // login to preserve, so let the file installer restore delivery.
    if (
      id === "claude-code" &&
      e.provider === "anthropic" &&
      (token === "" || token == null) &&
      (e.value.refreshToken === "" || e.value.refreshToken == null)
    )
      continue;
    const unsupported =
      !prov ||
      typeof token !== "string" ||
      !token ||
      token.startsWith("!") ||
      token.includes("{{") ||
      token.startsWith("discobox-") ||
      e.value.env ||
      e.value.enterpriseUrl ||
      e.value.apiEndpoint ||
      e.value.metadata?.enterpriseUrl ||
      (id === "pi" &&
        e.provider === "openai-codex" &&
        e.value.type === "oauth");
    if (unsupported) {
      diagnostic(p, "unsupported");
      output.protectedPaths.push(...definition.credentialFiles);
      continue;
    }
    const knownName = known.get(token);
    if (
      knownName &&
      knownName !== prov.envName &&
      !(prov.multipleAccounts && knownName.startsWith(prov.envName + "_"))
    ) {
      diagnostic(p, "unsupported");
      output.protectedPaths.push(...definition.credentialFiles);
      continue;
    }
    accepted.push({ ...e, prov, token, knownName });
  }
  let plans = [];
  if (["pi", "omp", "opencode"].includes(id)) {
    let clean;
    if (id === "pi")
      clean = Object.fromEntries(
        accepted.map((e) => [e.provider, cleanValue(e.value)]),
      );
    if (id === "omp")
      clean = {
        version: 1,
        credentials: accepted.map((e) => ({
          provider: e.provider,
          credential: cleanValue(e.value),
        })),
      };
    if (id === "opencode")
      clean = accepted.map((e) => ({ ...e.raw, value: cleanValue(e.value) }));
    const result = normalizer.normalize(
      normalizeAliases(clean),
      previous(input, definition, id, normalizer),
      currentSentinels,
      definition,
    );
    plans = result.plan;
    for (const item of plans) {
      if (id === "pi") item.entry = cleanValue(item.entry);
      if (id === "omp")
        item.entry = {
          provider: item.provider,
          credential: cleanValue(item.entry.credential),
        };
      if (id === "opencode")
        item.entry = {
          integrationID: item.provider,
          value: cleanValue(item.entry.value),
        };
    }
    if (result.skipped.length) {
      diagnostic(p, "unsupported");
      output.protectedPaths.push(...definition.credentialFiles);
    }
  } else
    for (const e of accepted) {
      const envName = e.prov.envName,
        t = template(envName);
      let item = {
        provider: e.provider,
        envName,
        type: e.prov.types[0],
        kept: !!e.knownName,
      };
      try {
        if (id === "claude-code" && e.provider === "anthropic") {
          item.value = normalizer.oauth(e.value, e.prov);
          item.entry = JSON.parse(
            normalizer.credentialsFile(item.value, envName).content,
          ).claudeAiOauth;
        } else if (id === "codex" && e.provider === "openai") {
          const got = normalizer.oauth(doc, e.prov);
          item.value = got.secret;
          item.entry = JSON.parse(
            normalizer.oauthAuthFile(got.tokens, envName).content,
          ).tokens;
          if (item.entry.account_id === null) delete item.entry.account_id;
        } else {
          item.type = "token";
          item.value = { token: e.token };
          item.entry = t;
        }
        plans.push(item);
      } catch {
        diagnostic(p, "partial");
        output.protectedPaths.push(...definition.credentialFiles);
      }
    }
  const templates = [];
  for (const item of plans) {
    const e = accepted.find(
      (e) =>
        e.provider === item.provider &&
        (item.kept
          ? e.knownName === item.envName
          : item.value?.token === e.token),
    );
    if (!e) continue;
    const keep = !!e.knownName || !!input.baselineOnly;
    const credential = {
      provider: item.provider,
      envName: item.envName,
      type: item.type,
      localId: e.localId,
      generation: hash(e.raw),
      account:
        e.value.accountId || e.value.metadata?.accountID || e.value.email || "",
      keep,
    };
    if (!keep) credential.value = item.value;
    output.credentials.push(credential);
    templates.push({ ...item, source: e });
  }
  if (templates.length) {
    if (id === "pi")
      output.files.push({
        path: p,
        template: true,
        content: json(
          Object.fromEntries(templates.map((t) => [t.provider, t.entry])),
        ),
      });
    if (id === "omp")
      output.files.push({
        path: p,
        template: true,
        content: json({
          version: 1,
          credentials: templates.map((t) => t.entry),
        }),
      });
    if (id === "opencode")
      output.files.push({
        path: p,
        template: true,
        content: json(
          templates.map((t) => ({
            ...t.entry,
            id: "cred_discobox_" + t.provider,
            active: true,
            label: "Discobox",
          })),
        ),
      });
    if (id === "claude-code")
      for (const t of templates)
        output.files.push({
          path: t.provider === "anthropic" ? p : ".claude.json",
          template: true,
          ...(t.provider === "anthropic-key" ? { createOnly: true } : {}),
          content: json(
            t.provider === "anthropic"
              ? { claudeAiOauth: t.entry }
              : { primaryApiKey: t.entry, hasCompletedOnboarding: true },
          ),
        });
    if (id === "codex") {
      const t = templates[0];
      output.files.push({
        path: p,
        template: true,
        content: json(
          t.provider === "openai-key"
            ? { OPENAI_API_KEY: t.entry }
            : {
                OPENAI_API_KEY: null,
                tokens: t.entry,
                last_refresh: "2100-01-01T00:00:00Z",
              },
        ),
      });
    }
  }
  output.protectedPaths = [...new Set(output.protectedPaths)];
  return output;
}
function live(id) {
  const bin = id === "codex" ? "codex" : id === "claude-code" ? "claude" : id;
  for (const d of fs.readdirSync("/proc"))
    if (/^\d+$/.test(d) && +d !== process.pid)
      try {
        if (
          !fs
            .readFileSync("/proc/" + d + "/environ", "utf8")
            .split("\0")
            .includes("HOME=" + process.env.HOME)
        )
          continue;
        const args = fs
          .readFileSync("/proc/" + d + "/cmdline", "utf8")
          .split("\0");
        if (args.some((a) => path.basename(a) === bin)) return true;
      } catch {}
  return false;
}
// OpenCode import does not replace an existing native ID. Journal the
// acknowledged transition before logout so retry can distinguish our own
// destructive midpoint from an unrelated local logout. No secret enters the
// journal: its payload is only IDs, digests and the original active choice.
function openCodeHandoff(input, credential, wanted, doc) {
  const c = credential;
  const journal =
    ".config/discobox/opencode-handoff/" +
    hash([input.candidateId || c.generation, c.localId]) +
    ".json";
  const valueHash = hash(wanted.value);
  let progress;
  try {
    progress = JSON.parse(read(journal));
  } catch (err) {
    if (err.code !== "ENOENT") return "retryable-failure";
  }
  if (
    progress &&
    (progress.generation !== c.generation ||
      progress.valueHash !== valueHash ||
      progress.localId !== c.localId ||
      progress.provider !== c.provider)
  )
    return "local-conflict";
  const others = (rows) =>
    hash(
      rows
        .filter((e) => e.integrationID === c.provider && e.id !== c.localId)
        .map((e) => {
          const { active, ...rest } = e;
          return rest;
        })
        .sort((a, b) => a.id.localeCompare(b.id)),
    );
  const refresh = () =>
    JSON.parse(command("opencode", ["auth", "export", "--standalone"], ""));
  let current = doc.find(
    (e) => e.id === c.localId && e.integrationID === c.provider,
  );
  const delivered = () => current && hash(current.value) === valueHash;
  if (delivered() && !progress) return "applied";
  if (!progress) {
    if (!current || hash(current) !== c.generation) return "local-conflict";
    progress = {
      generation: c.generation,
      valueHash,
      localId: c.localId,
      provider: c.provider,
      wasActive: !!current.active,
      others: others(doc),
    };
    atomic(journal, json(progress), true);
  }
  if (others(doc) !== progress.others) return "local-conflict";
  if (current && !delivered()) {
    if (hash(current) !== c.generation) return "local-conflict";
    command(
      "opencode",
      ["auth", "logout", "--standalone", c.provider, c.localId],
      "",
    );
    doc = refresh();
    current = doc.find((e) => e.id === c.localId);
  }
  if (others(doc) !== progress.others) return "local-conflict";
  if (!current) {
    command(
      "opencode",
      ["auth", "import", "--standalone"],
      json([{ ...wanted, id: c.localId, active: progress.wasActive }]),
    );
    doc = refresh();
    current = doc.find((e) => e.id === c.localId);
  }
  if (!delivered())
    return current && hash(current) !== c.generation
      ? "local-conflict"
      : "retryable-failure";
  if (others(doc) !== progress.others) return "local-conflict";
  if (progress.wasActive && !current.active) {
    command(
      "opencode",
      ["auth", "switch", "--standalone", c.provider, c.localId],
      "",
    );
    doc = refresh();
    current = doc.find((e) => e.id === c.localId);
    if (!delivered() || !current.active) return "retryable-failure";
  }
  fs.unlinkSync(safePath(journal));
  return "applied";
}
function handoff(id, input, normalizer) {
  const finish = (state) => ({ state });
  const pending = (input.credentials || []).filter((c) => !c.keep);
  if (!pending.length) return finish("applied");
  if (["omp", "opencode"].includes(id) && live(id))
    return finish("retryable-failure");
  let doc;
  try {
    doc = load(id, { baselineOnly: false });
  } catch {
    return finish("retryable-failure");
  }
  let have = entries(id, doc, normalizer);
  let changed = false;
  for (const c of pending) {
    let p = credentialPath[id],
      targetDoc;
    if (id === "claude-code" && c.provider === "anthropic-key") {
      p = ".claude.json";
      try {
        targetDoc = JSON.parse(read(p));
      } catch {
        return finish("retryable-failure");
      }
      have = [
        {
          localId: "primaryApiKey",
          raw: targetDoc.primaryApiKey,
          value: { key: targetDoc.primaryApiKey },
          provider: c.provider,
        },
      ];
    }
    const targetFile = (input.files || []).find((f) => f.path === p);
    let wanted;
    if (id !== "copilot") {
      if (!targetFile) return finish("retryable-failure");
      const target = JSON.parse(targetFile.content);
      wanted =
        id === "claude-code" && c.provider === "anthropic-key"
          ? target.primaryApiKey
          : entries(id, target, normalizer).find(
              (e) =>
                e.provider === c.provider &&
                (id !== "omp" ||
                  (e.value.accountId || e.value.email || "") === c.account),
            )?.raw;
      if (wanted === undefined) return finish("retryable-failure");
    }
    if (id === "opencode") {
      let state;
      try {
        state = openCodeHandoff(input, c, wanted, doc);
      } catch {
        return finish("retryable-failure");
      }
      if (state !== "applied") return finish(state);
      doc = load(id, { baselineOnly: false });
      have = entries(id, doc, normalizer);
      continue;
    }
    const current = have.find((e) => e.localId === c.localId);
    if (!current) {
      if (id === "copilot") continue;
      return finish("local-conflict");
    }
    if (id !== "copilot" && hash(current.raw) === hash(wanted)) continue;
    if (hash(current.raw) !== c.generation) return finish("local-conflict");
    if (id === "pi") doc[c.localId] = wanted;
    else if (id === "claude-code" && c.provider === "anthropic-key") {
      targetDoc.primaryApiKey = wanted;
      atomic(p, json(targetDoc));
      continue;
    } else if (id === "claude-code") doc.claudeAiOauth = wanted;
    else if (id === "codex") {
      if (c.provider === "openai-key") {
        doc = { OPENAI_API_KEY: wanted };
      } else {
        doc.tokens = wanted;
        doc.OPENAI_API_KEY = null;
        doc.last_refresh = "2100-01-01T00:00:00Z";
      }
    } else if (id === "copilot") {
      delete doc.authTokens[c.localId];
      const sibling = c.localId.endsWith(":github")
        ? c.localId.slice(0, -7)
        : c.localId + ":github";
      if (doc.authTokens[sibling]?.token === current.value.token)
        delete doc.authTokens[sibling];
    } else if (id === "omp") {
      const temp = path.join(
        process.env.HOME,
        ".config/discobox/capture-import-" +
          crypto.randomBytes(8).toString("hex") +
          ".json",
      );
      try {
        fs.writeFileSync(temp, json({ version: 1, credentials: [wanted] }), {
          mode: 0o600,
          flag: "wx",
        });
        command("omp", ["auth", "import", temp], "");
      } finally {
        try {
          fs.unlinkSync(temp);
        } catch {}
      }
    }
    changed = true;
  }
  if (changed && !["omp", "opencode"].includes(id))
    atomic(credentialPath[id], json(doc));
  return finish("applied");
}
function guard(id, definition, normalizer) {
  const database =
    id === "omp" ? ".omp/agent/agent.db" : ".local/share/opencode/opencode.db";
  try {
    fs.statSync(safePath(database));
  } catch (e) {
    if (e.code === "ENOENT") return true;
    throw e;
  }
  const p = credentialPath[id];
  let delivered;
  try {
    delivered = read(p);
  } catch (e) {
    if (e.code === "ENOENT") return false;
    throw e;
  }
  // This user-readable context belongs only to launcher convergence. Capture
  // candidates never read it: their Keep authority comes from private input.
  let context = { sentinels: {}, knownSentinels: {} };
  try {
    const raw = read(".config/discobox/harness-sentinels.json");
    if (Buffer.byteLength(raw) > 256 * 1024) return false;
    context = JSON.parse(raw);
    if (context && context.knownSentinels == null) context.knownSentinels = {};
    const object = (x) => x && typeof x === "object" && !Array.isArray(x);
    if (
      !object(context) ||
      !object(context.sentinels) ||
      !object(context.knownSentinels)
    )
      return false;
    if (
      Object.keys(context.sentinels).length > 128 ||
      Object.keys(context.knownSentinels).length > 128
    )
      return false;
    if (
      Object.values(context.sentinels).some((v) => typeof v !== "string" || !v)
    )
      return false;
    if (
      Object.values(context.knownSentinels).some(
        (v) =>
          !Array.isArray(v) ||
          v.length > 128 ||
          v.some((s) => typeof s !== "string" || !s),
      )
    )
      return false;
    knownBindings(context); // Reject alias collisions between bindings.
  } catch (err) {
    if (err.code !== "ENOENT") return false;
  }
  const doc = JSON.parse(delivered),
    sentinels = {},
    aliases = {};
  const allowed = (spec, name) =>
    name === spec.envName ||
    (spec.multipleAccounts &&
      name.startsWith(spec.envName + "_") &&
      /^(?:[2-9]|[1-9][0-9]+)$/.test(name.slice(spec.envName.length + 1)));
  for (const e of entries(id, doc, normalizer)) {
    const spec = definition.providers.find((p) => p.id === e.provider);
    if (!spec) return false;
    const value = material(id, e);
    let name = Object.keys(context.sentinels).find(
      (name) =>
        allowed(spec, name) &&
        (context.sentinels[name] === value ||
          (context.knownSentinels[name] || []).includes(value)),
    );
    if (!name) {
      name = spec.envName;
      for (let n = 2; sentinels[name]; n++) name = spec.envName + "_" + n;
      sentinels[name] = value;
    } else {
      if (sentinels[name]) return false;
      sentinels[name] = context.sentinels[name];
      aliases[name] = context.knownSentinels[name] || [];
    }
  }
  const result = capture(id, definition, normalizer, {
    baseline: { files: [{ path: p, content: delivered }] },
    sentinels,
    knownSentinels: aliases,
  });
  return (
    !result.protectedPaths.length && result.credentials.every((c) => c.keep)
  );
}
function main() {
  const [id, metadata, mode] = process.argv.slice(2);
  const definition = JSON.parse(fs.readFileSync(metadata, "utf8")).harness
    .saveBack;
  let normalizer = {};
  try {
    normalizer = require(
      path.join(path.dirname(metadata), id + "-normalize.cjs"),
    );
  } catch (e) {
    if (e.code !== "MODULE_NOT_FOUND") throw e;
  }
  if (mode === "guard") {
    process.exitCode = guard(id, definition, normalizer) ? 0 : 1;
    return;
  }
  const raw = fs.readFileSync(0, "utf8");
  if (raw.length > 2 * 1024 * 1024) throw Error("input size");
  const input = JSON.parse(raw);
  if (input.version !== 1) throw Error("version");
  const result =
    mode === "export"
      ? capture(id, definition, normalizer, input)
      : mode === "handoff"
        ? handoff(id, input, normalizer)
        : null;
  if (!result) throw Error("mode");
  process.stdout.write(JSON.stringify(result));
}
if (require.main === module)
  try {
    main();
  } catch (error) {
    // Never print exception messages or stacks: parsers and child processes
    // can embed credential contents. The agent accepts only these fixed codes.
    const reason =
      error instanceof SyntaxError
        ? "invalid-json"
        : ["ENOENT", "EACCES", "EPERM"].includes(error?.code)
          ? "file-unavailable"
          : "operation-failed";
    process.stderr.write(`discobox-capture: ${reason}\n`);
    if (process.argv.at(-1) === "handoff")
      process.stdout.write('{"state":"retryable-failure"}');
    else process.exitCode = 1;
  }
module.exports = { capture, handoff, entries, material, guard };
