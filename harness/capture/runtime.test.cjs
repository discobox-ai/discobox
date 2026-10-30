"use strict";
const { test } = require("node:test"),
  assert = require("node:assert/strict"),
  fs = require("fs"),
  os = require("os"),
  path = require("path");
const { capture, handoff, guard, resolveReferences, initialize } = require("./runtime.cjs");
const { project } = require("./settings.cjs");
function fixture(id) {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), "capture-test-"));
  process.env.HOME = home;
  const folder = id === "codex" ? "codex-cli" : id;
  const definition = require("../" + folder + "/image.json").harness.saveBack;
  const normalizer = require("../" + folder + "/" + id + "-normalize.cjs");
  return {
    home,
    definition,
    normalizer,
    input: { version: 1, baseline: { files: [] }, sentinels: {} },
    write(p, v) {
      fs.mkdirSync(path.dirname(path.join(home, p)), { recursive: true });
      fs.writeFileSync(
        path.join(home, p),
        typeof v === "string" ? v : JSON.stringify(v),
      );
    },
    run(input) {
      return capture(id, definition, normalizer, input || this.input);
    },
    close() {
      fs.rmSync(home, { recursive: true, force: true });
    },
  };
}
const render = (files, sentinels) => files.map(f => {
 try { return {...f, content: JSON.stringify(resolveReferences(JSON.parse(f.content),sentinels))}; }
 catch(e) { if(f.path.endsWith(".toml"))return f; throw e; }
});
test("pi exports renewable login and safe settings, then recognizes delivered sentinel", () => {
  const f = fixture("pi");
  try {
    f.write(".pi/agent/auth.json", {
      anthropic: {
        type: "oauth",
        access: "real-access",
        refresh: "real-refresh",
        expires: 1234,
        unrecognizedSecret: "never-copy",
      },
    });
    f.write(".pi/agent/settings.json", {
      defaultModel: "sonnet",
      apiKey: "never-copy",
      extensions: ["!secret"],
    });
    const out = f.run();
    assert.equal(out.credentials[0].value.refreshToken, "real-refresh");
    assert.equal(out.credentials[0].type, "oauth");
    assert(!JSON.stringify(out.files).includes("never-copy"));
    assert.equal(out.diagnostics.length, 0);
    const sentinels = { PI_ANTHROPIC_CREDENTIAL: "synthetic-sentinel" };
    const delivered = render(out.files, sentinels);
    for (const file of delivered) f.write(file.path, file.content);
    const kept = f.run({
      ...f.input,
      sentinels,
      baseline: { files: delivered },
    });
    assert.equal(kept.credentials.length, 1);
    assert.equal(kept.credentials[0].keep, true);
    assert.equal(kept.credentials[0].value, undefined);
  } finally {
    f.close();
  }
});
test("pi Copilot access-token churn is not a new GitHub login", () => {
  const f = fixture("pi");
  try {
    f.write(".pi/agent/auth.json", {
      "github-copilot": {
        type: "oauth",
        access: "short-lived-token",
        refresh: "github-token",
        expires: 123,
      },
    });
    const out = f.run();
    assert.equal(out.credentials[0].value.token, "github-token");
    const sentinels = { PI_GITHUB_COPILOT_CREDENTIAL: "github-sentinel" },
      files = render(out.files, sentinels);
    const auth = JSON.parse(files[0].content);
    auth["github-copilot"].access = "new-derived-token";
    auth["github-copilot"].expires = 98765;
    f.write(".pi/agent/auth.json", auth);
    const kept = f.run({ ...f.input, sentinels, baseline: { files } });
    assert.equal(kept.credentials[0].keep, true);
    assert.equal(kept.credentials[0].value, undefined);
    assert(!JSON.stringify(kept.files).includes("new-derived-token"));
  } finally {
    f.close();
  }
});
test("wrong-provider known sentinel and unsupported login never become new secret", () => {
  const f = fixture("pi");
  try {
    f.write(".pi/agent/auth.json", {
      anthropic: { type: "api_key", key: "other-sentinel" },
      "openai-codex": {
        type: "oauth",
        access: "jwt-secret",
        refresh: "refresh-secret",
      },
      private: { type: "api_key", key: "private-secret" },
    });
    const out = f.run({
      ...f.input,
      sentinels: { PI_OPENAI_CREDENTIAL: "other-sentinel" },
    });
    assert.equal(out.credentials.length, 0);
    assert(out.protectedPaths.includes(".pi/agent/auth.json"));
    assert(!JSON.stringify(out).includes("private-secret"));
    assert(!JSON.stringify(out).includes("jwt-secret"));
  } finally {
    f.close();
  }
});
test("missing, malformed, and unsupported files never express deletion", () => {
  const f = fixture("pi");
  try {
    assert.deepEqual(f.run().credentials, []);
    f.write(".pi/agent/auth.json", '{"anthropic":');
    const out = f.run();
    assert.equal(out.files.length, 0);
    assert(out.protectedPaths.includes(".pi/agent/auth.json"));
    assert.equal(out.diagnostics[0].code, "partial");
  } finally {
    f.close();
  }
});
test("baseline-only reads delivered settings and keeps credentials", () => {
  const f = fixture("pi");
  try {
    f.write(".pi/agent/settings.json", { defaultModel: "local-edit" });
    f.write(".pi/agent/auth.json", {
      anthropic: { type: "api_key", key: "local-login" },
    });
    const out = f.run({
      ...f.input,
      baselineOnly: true,
      sentinels: { PI_ANTHROPIC_CREDENTIAL: "old-sentinel" },
      baseline: {
        files: [
          {
            path: ".pi/agent/settings.json",
            content: '{"defaultModel":"delivered"}',
          },
          {
            path: ".pi/agent/auth.json",
            content: '{"anthropic":{"type":"api_key","key":"old-sentinel"}}',
          },
        ],
      },
    });
    assert(out.files.some((f) => f.content.includes("delivered")));
    assert(!JSON.stringify(out).includes("local-login"));
    assert.equal(out.credentials[0].keep, true);
  } finally {
    f.close();
  }
});
test("handoff preserves a newer login, then selectively sentinelizes an unchanged entry", () => {
  const f = fixture("pi");
  try {
    const auth = {
      anthropic: { type: "api_key", key: "new-login" },
      openai: { type: "api_key", key: "separate-login" },
    };
    f.write(".pi/agent/auth.json", auth);
    const out = f.run(),
      credential = out.credentials.find((c) => c.provider === "anthropic"),
      sentinels = {
        PI_ANTHROPIC_CREDENTIAL: "delivered-a",
        PI_OPENAI_CREDENTIAL: "delivered-b",
      };
    const ack = {
      version: 1,
      credentials: [credential],
      files: render(out.files, sentinels),
      sentinels,
    };
    auth.anthropic.key = "newer-login";
    f.write(".pi/agent/auth.json", auth);
    assert.equal(handoff("pi", ack, f.normalizer).state, "local-conflict");
    assert.equal(
      JSON.parse(fs.readFileSync(path.join(f.home, ".pi/agent/auth.json")))
        .anthropic.key,
      "newer-login",
    );
    auth.anthropic.key = "new-login";
    f.write(".pi/agent/auth.json", auth);
    assert.equal(handoff("pi", ack, f.normalizer).state, "applied");
    const stored = JSON.parse(
      fs.readFileSync(path.join(f.home, ".pi/agent/auth.json")),
    );
    assert.equal(stored.anthropic.key, "delivered-a");
    assert.equal(stored.openai.key, "separate-login");
    assert.equal(handoff("pi", ack, f.normalizer).state, "applied");
  } finally {
    f.close();
  }
});
test("Claude preserves OAuth scopes and Codex keeps only unsigned account claims", () => {
  for (const id of ["claude-code", "codex"]) {
    const f = fixture(id);
    try {
      if (id === "claude-code")
        f.write(".claude/.credentials.json", {
          claudeAiOauth: {
            accessToken: "access",
            refreshToken: "refresh",
            expiresAt: 123,
            scopes: ["user:profile"],
            subscriptionType: "pro",
          },
        });
      else {
        const jwt = (x) =>
          "e30." +
          Buffer.from(JSON.stringify(x)).toString("base64url") +
          ".signed";
        f.write(".codex/auth.json", {
          tokens: {
            access_token: jwt({ exp: 123 }),
            refresh_token: "refresh",
            id_token: jwt({
              email: "test@example.test",
              "https://api.openai.com/auth": { chatgpt_account_id: "acct" },
            }),
            account_id: "acct",
          },
        });
      }
      const out = f.run();
      assert.equal(out.credentials[0].type, "oauth");
      assert.equal(out.credentials[0].value.refreshToken, "refresh");
      assert(!JSON.stringify(out.files).includes(".signed"));
      if (id === "claude-code")
        assert(out.files[0].content.includes("user:profile"));
      else assert(out.files[0].content.includes("acct"));
    } finally {
      f.close();
    }
  }
});
test("Claude locally entered API key is captured without credentials file", () => {
  const f = fixture("claude-code");
  try {
    f.write(".claude.json", {
      primaryApiKey: "new-key",
      projects: { "/private": {} },
    });
    const out = f.run();
    assert.equal(out.credentials[0].value.token, "new-key");
    assert(!out.files[0].content.includes("private"));
  } finally {
    f.close();
  }
});
test("Copilot clears only the acknowledged login and its duplicate", () => {
  const f = fixture("copilot");
  try {
    const key = "https://github.com:alice";
    f.write(".copilot/config.json", {
      lastLoggedInUser: { host: "https://github.com", login: "alice" },
      authTokens: {
        [key]: { token: "alice-token" },
        [key + ":github"]: { token: "alice-token" },
        "https://github.com:bob": { token: "bob-token" },
      },
    });
    const out = f.run();
    assert.equal(out.credentials[0].value.token, "alice-token");
    assert.equal(
      handoff(
        "copilot",
        { credentials: out.credentials, files: [], sentinels: {} },
        f.normalizer,
      ).state,
      "applied",
    );
    const stored = JSON.parse(
      fs.readFileSync(path.join(f.home, ".copilot/config.json")),
    );
    assert.equal(stored.authTokens[key], undefined);
    assert.equal(stored.authTokens[key + ":github"], undefined);
    assert.equal(
      stored.authTokens["https://github.com:bob"].token,
      "bob-token",
    );
  } finally {
    f.close();
  }
});
test("OMP normalization keeps account bindings across export order", () => {
  const f = fixture("omp");
  try {
    const base = "OMP_ANTHROPIC_CREDENTIAL";
    const previous = {
      files: [
        {
          path: ".config/discobox/omp-credentials.json",
          content: JSON.stringify({
            version: 1,
            credentials: [
              {
                provider: "anthropic",
                credential: {
                  type: "oauth",
                  accountId: "a",
                  access: { $secret: base },
                  refresh: "placeholder",
                },
              },
              {
                provider: "anthropic",
                credential: {
                  type: "oauth",
                  accountId: "b",
                  access: { $secret: base + "_2" },
                  refresh: "placeholder",
                },
              },
            ],
          }),
        },
      ],
      secrets: [
        { envName: base, type: "oauth" },
        { envName: base + "_2", type: "oauth" },
      ],
    };
    const result = f.normalizer.normalize(
      {
        version: 1,
        credentials: [
          {
            provider: "anthropic",
            credential: {
              type: "oauth",
              accountId: "b",
              access: "new-b",
              refresh: "refresh-b",
            },
          },
          {
            provider: "anthropic",
            credential: {
              type: "oauth",
              accountId: "a",
              access: "sentinel-a",
              refresh: "placeholder",
            },
          },
        ],
      },
      previous,
      { [base]: "sentinel-a", [base + "_2"]: "sentinel-b" },
      f.definition,
    );
    assert.equal(result.plan.find((p) => p.kept).envName, base);
    assert.equal(result.plan.find((p) => !p.kept).envName, base + "_2");
  } finally {
    f.close();
  }
});
test("portable projection excludes trust and credentials; malformed grammar fails closed", () => {
  const spec = {
    format: "toml",
    exclude: ["projects", "apiKey"],
  };
  const got = project(
    'model = "safe"\n[projects.private]\ntrust_level = "trusted"\n[tui]\nnotifications = true',
    spec,
  );
  assert(got.content.includes("safe"));
  assert(!got.content.includes("private"));
  assert.equal(got.unsupported.length, 1);
  assert.throws(() => project('model = "unterminated', spec));
  const jc = project(
    '{"model":"https://example.test/a//b",/* comment */"apiKey":"secret",}',
    { format: "jsonc", exclude: ["apiKey"] },
  );
  assert(jc.content.includes("https://example.test/a//b"));
  assert(!jc.content.includes("secret"));
});
function nativeStub(f, id, doc) {
  const bin = path.join(f.home, "bin");
  fs.mkdirSync(bin);
  const old = process.env.PATH;
  process.env.PATH = bin + path.delimiter + old;
  f.write("store.json", doc);
  f.write(
    id === "omp" ? ".omp/agent/agent.db" : ".local/share/opencode/opencode.db",
    "database-marker",
  );
  fs.writeFileSync(
    path.join(bin, id),
    `#!/usr/bin/env node
const fs=require('fs'),p=require('path'),home=process.env.HOME,file=p.join(home,'store.json');let doc=JSON.parse(fs.readFileSync(file));const args=process.argv.slice(2).filter(x=>x!=='--standalone');
if(args[1]==='export'){process.stdout.write(JSON.stringify(doc));process.exit();}
if(args[1]==='import'&&fs.existsSync(p.join(home,'fail-import-once'))){fs.unlinkSync(p.join(home,'fail-import-once'));process.exit(5);}
if(args[1]==='import'){const incoming=JSON.parse(args[2]?fs.readFileSync(args[2]):fs.readFileSync(0));if('${id}'==='omp'){for(const e of incoming.credentials){const i=doc.credentials.findIndex(c=>c.provider===e.provider&&(c.credential.accountId||'')===(e.credential.accountId||''));if(i<0)doc.credentials.push(e);else doc.credentials[i]=e;}}else{for(const e of incoming)if(!doc.some(c=>c.id===e.id))doc.push({...e,active:!doc.some(c=>c.integrationID===e.integrationID&&c.active)});}}
else if(args[1]==='logout'){const gone=doc.find(c=>c.id===args[3]);doc=doc.filter(c=>c.id!==args[3]);if(gone?.active){const next=doc.find(c=>c.integrationID===gone.integrationID);if(next)next.active=true;}}else if(args[1]==='switch'){for(const c of doc)if(c.integrationID===args[2])c.active=c.id===args[3];}else process.exit(3);
fs.writeFileSync(file,JSON.stringify(doc));
`,
    { mode: 0o755 },
  );
  return () => {
    process.env.PATH = old;
  };
}
test("native DB capture and handoff use exports/imports; launch guard preserves local login", {
  skip: process.platform !== "linux" &&
    "Native handoff requires Linux /proc and POSIX executable stubs",
}, () => {
  for (const id of ["omp", "opencode"]) {
    const f = fixture(id),
      provider = id === "omp" ? "anthropic" : "openai";
    const value = { type: id === "omp" ? "api_key" : "key", key: "local-key" };
    const doc =
      id === "omp"
        ? { version: 1, credentials: [{ provider, credential: value }] }
        : [
            {
              id: "local-account",
              integrationID: provider,
              value,
              active: true,
              label: "local",
            },
          ];
    const restore = nativeStub(f, id, doc);
    try {
      const out = f.run();
      assert.equal(out.credentials.length, 1);
      assert.equal(out.credentials[0].value.token, "local-key");
      const sentinels = { [out.credentials[0].envName]: "delivered-sentinel" },
        files = render(out.files, sentinels);
      for (const file of files) f.write(file.path, file.content);
      assert.equal(guard(id, f.definition, f.normalizer), false);
      const ack = {
        version: 1,
        credentials: out.credentials,
        files,
        sentinels,
      };
      assert.equal(handoff(id, ack, f.normalizer).state, "applied");
      const stored = JSON.parse(
        fs.readFileSync(path.join(f.home, "store.json")),
      );
      assert(!JSON.stringify(stored).includes("local-key"));
      assert(JSON.stringify(stored).includes("delivered-sentinel"));
      assert.equal(handoff(id, ack, f.normalizer).state, "applied");
      assert.equal(guard(id, f.definition, f.normalizer), true);
    } finally {
      restore();
      f.close();
    }
  }
});

test("Codex captures edits from actual rendered bundle config and retains portable trust", () => {
  const f = fixture("codex");
  try {
    const rendered = '[projects."/source/project"]\ntrust_level = "trusted"\n';
    f.write(".codex/config.toml", 'model = "new-model"\n' + rendered);
    const out = f.run(),
      config = out.files.find((f) => f.path === ".codex/config.toml");
    assert(config);
    assert.equal(config.template, undefined);
    assert(config.content.includes('model = "new-model"'));
    assert(!config.content.includes("/source/project"));
    assert(!config.content.includes("[projects."));
    assert(
      !out.diagnostics.some(
        (d) => d.path === ".codex/config.toml" && d.code === "partial",
      ),
    );
    const normal = f.run({
      ...f.input,
      baselineOnly: true,
      baseline: { files: [{ path: ".codex/config.toml", content: rendered }] },
    });
    assert.equal(
      normal.files.some((f) => f.path === ".codex/config.toml"),
      false,
    );
    assert(
      !normal.diagnostics.some(
        (d) => d.path === ".codex/config.toml" && d.code === "partial",
      ),
    );
  } finally {
    f.close();
  }
});
test("OpenCode retries logout/import midpoint and restores captured active account", {
  skip: process.platform !== "linux" &&
    "Native handoff requires Linux /proc and POSIX executable stubs",
}, () => {
  const f = fixture("opencode");
  const restore = nativeStub(f, "opencode", [
    {
      id: "saved-account",
      integrationID: "openai",
      active: true,
      label: "saved",
      value: { type: "key", key: "new-login" },
    },
    {
      id: "other-account",
      integrationID: "openai",
      active: false,
      label: "other",
      value: { type: "key", key: "unrelated-login" },
    },
  ]);
  try {
    const out = f.run(),
      sentinels = { OPENCODE_OPENAI_CREDENTIAL: "accepted-sentinel" },
      files = render(out.files, sentinels);
    const ack = {
      version: 1,
      candidateId: "durable-ack",
      credentials: out.credentials,
      files,
      sentinels,
    };
    f.write("fail-import-once", "fail");
    assert.equal(
      handoff("opencode", ack, f.normalizer).state,
      "retryable-failure",
    );
    let stored = JSON.parse(fs.readFileSync(path.join(f.home, "store.json")));
    assert.equal(stored.length, 1);
    assert.equal(stored[0].id, "other-account");
    assert.equal(stored[0].active, true);
    assert.equal(handoff("opencode", ack, f.normalizer).state, "applied");
    stored = JSON.parse(fs.readFileSync(path.join(f.home, "store.json")));
    assert.equal(
      stored.find((e) => e.id === "saved-account").value.key,
      "accepted-sentinel",
    );
    assert.equal(stored.find((e) => e.id === "saved-account").active, true);
    assert.equal(
      stored.find((e) => e.id === "other-account").value.key,
      "unrelated-login",
    );
    assert.equal(stored.find((e) => e.id === "other-account").active, false);
    assert.equal(handoff("opencode", ack, f.normalizer).state, "applied");
  } finally {
    restore();
    f.close();
  }
});
test("OpenCode midpoint retry preserves a newer local account instead of switching over it", {
  skip: process.platform !== "linux" &&
    "Native handoff requires Linux /proc and POSIX executable stubs",
}, () => {
  const f = fixture("opencode");
  const restore = nativeStub(f, "opencode", [
    {
      id: "saved",
      integrationID: "openai",
      active: true,
      value: { type: "key", key: "saved-login" },
    },
  ]);
  try {
    const out = f.run(),
      sentinels = { OPENCODE_OPENAI_CREDENTIAL: "accepted-sentinel" };
    const ack = {
      version: 1,
      candidateId: "ack-conflict",
      credentials: out.credentials,
      files: render(out.files, sentinels),
      sentinels,
    };
    f.write("fail-import-once", "fail");
    assert.equal(
      handoff("opencode", ack, f.normalizer).state,
      "retryable-failure",
    );
    f.write("store.json", [
      {
        id: "newer",
        integrationID: "openai",
        active: true,
        value: { type: "key", key: "newer-login" },
      },
    ]);
    assert.equal(
      handoff("opencode", ack, f.normalizer).state,
      "local-conflict",
    );
    assert.equal(
      JSON.parse(fs.readFileSync(path.join(f.home, "store.json")))[0].value.key,
      "newer-login",
    );
  } finally {
    restore();
    f.close();
  }
});
test("reconsider rebuilds shared auth from current entries plus only selected immutable credentials", () => {
  const f = fixture("pi");
  try {
    f.write(".pi/agent/auth.json", {
      anthropic: { type: "api_key", key: "remote-a" },
      openai: { type: "api_key", key: "remote-b" },
    });
    const remote = f.run();
    f.write(".pi/agent/auth.json", {
      anthropic: {
        type: "oauth",
        access: "unselected-a",
        refresh: "unselected-refresh",
        expires: 1,
      },
      openai: { type: "api_key", key: "selected-b" },
    });
    const local = f.run();
    const candidate = {
      credentials: local.credentials,
      files: local.files.map((file) => ({ path: file.path, file })),
    };
    // A third login on disk must never participate in reconsideration.
    f.write(".pi/agent/auth.json", {
      openai: { type: "api_key", key: "newer-local-b" },
    });
    const out = f.run({
      ...f.input,
      reconsider: {
        candidate,
        current: { files: remote.files },
        replaceCredentials: ["PI_OPENAI_CREDENTIAL"],
      },
    });
    const auth = JSON.parse(
      out.files.find((f) => f.path === ".pi/agent/auth.json").content,
    );
    assert.equal(auth.anthropic.type, "api_key");
    assert.deepEqual(auth.anthropic.key, { $secret: "PI_ANTHROPIC_CREDENTIAL" });
    assert.equal(
      out.credentials.find((c) => c.envName === "PI_ANTHROPIC_CREDENTIAL").keep,
      true,
    );
    const selected = out.credentials.find(
      (c) => c.envName === "PI_OPENAI_CREDENTIAL",
    );
    assert.deepEqual(
      selected,
      local.credentials.find((c) => c.envName === "PI_OPENAI_CREDENTIAL"),
    );
    assert.equal(selected.value.token, "selected-b");
    assert(!JSON.stringify(out).includes("newer-local-b"));
    assert(!JSON.stringify(out).includes("unselected-refresh"));
  } finally {
    f.close();
  }
});

test("OMP reconsider preserves current account binding while replacing only selected second account", () => {
  const f = fixture("omp");
  try {
    const env = "OMP_ANTHROPIC_CREDENTIAL",
      p = ".config/discobox/omp-credentials.json";
    const entry = (account, name) => ({
      provider: "anthropic",
      credential: {
        type: "api_key",
        key: { $secret: name },
        accountId: account,
      },
    });
    const current = {
      files: [
        {
          path: p,
          content: JSON.stringify({
            version: 1,
            credentials: [
              entry("current-a", env),
              entry("current-b", env + "_2"),
            ],
          }),
        },
      ],
    };
    const file = {
      path: p,
      content: JSON.stringify({
        version: 1,
        credentials: [
          entry("unselected-a", env),
          entry("selected-b", env + "_2"),
        ],
      }),
    };
    const chosen = {
      provider: "anthropic",
      envName: env + "_2",
      account: "selected-b",
      localId: "local-b",
      generation: "captured-b",
      type: "token",
      value: { token: "secret-b" },
    };
    const out = f.run({
      ...f.input,
      reconsider: {
        current,
        candidate: {
          files: [{ path: p, file }],
          credentials: [
            { ...chosen, envName: env, value: { token: "unselected-secret" } },
            chosen,
          ],
        },
        replaceCredentials: [env + "_2"],
      },
    });
    const doc = JSON.parse(out.files[0].content);
    assert.equal(doc.credentials[0].credential.accountId, "current-a");
    assert.equal(doc.credentials[1].credential.accountId, "selected-b");
    assert.deepEqual(
      out.credentials.find((c) => c.envName === env + "_2"),
      chosen,
    );
    assert.equal(out.credentials.find((c) => c.envName === env).keep, true);
    assert(!JSON.stringify(out).includes("unselected-secret"));
  } finally {
    f.close();
  }
});

test("reminted file sentinel remains a keep binding until ordinary repair delivers the new sentinel", () => {
  const f = fixture("pi");
  try {
    const name = "PI_ANTHROPIC_CREDENTIAL";
    f.write(".pi/agent/auth.json", {
      anthropic: {
        type: "oauth",
        access: "original-login",
        refresh: "original-refresh",
        expires: 1234,
      },
    });
    const captured = f.run(),
      oldFiles = render(captured.files, { [name]: "old-delivered-sentinel" });
    for (const file of oldFiles) f.write(file.path, file.content);
    const input = {
      ...f.input,
      baseline: { files: oldFiles },
      sentinels: { [name]: "new-runtime-sentinel" },
      knownSentinels: { [name]: ["old-delivered-sentinel"] },
    };
    const stale = f.run(input);
    assert.equal(stale.credentials.length, 1);
    assert.equal(stale.credentials[0].keep, true);
    assert.equal(stale.credentials[0].value, undefined);
    assert.equal(stale.protectedPaths.length, 0);
    assert(!JSON.stringify(stale.files).includes("old-delivered-sentinel"));
    assert.equal(
      JSON.stringify(stale.files),
      JSON.stringify(f.run({ ...input, baselineOnly: true }).files),
    );
    // The agent can now perform its regular sentinel-file repair, without a
    // false new-login candidate blocking it.
    const repaired = render(stale.files, input.sentinels);
    for (const file of repaired) f.write(file.path, file.content);
    const fresh = f.run({ ...input, baseline: { files: repaired } });
    assert.equal(fresh.credentials[0].keep, true);
    assert.equal(fresh.credentials[0].value, undefined);
    assert.equal(
      JSON.parse(fs.readFileSync(path.join(f.home, ".pi/agent/auth.json")))
        .anthropic.access,
      "new-runtime-sentinel",
    );
  } finally {
    f.close();
  }
});
test("native DB export recognizes an old sentinel alias and still captures a subsequent real login", {
  skip: process.platform === "win32" &&
    "Native export fixture requires POSIX executable stubs",
}, () => {
  const f = fixture("opencode"),
    name = "OPENCODE_OPENAI_CREDENTIAL";
  const restore = nativeStub(f, "opencode", [
    {
      id: "cred_discobox_openai",
      integrationID: "openai",
      active: true,
      value: { type: "key", key: "old-db-sentinel" },
    },
  ]);
  try {
    const template = {
      path: ".config/discobox/opencode-credentials.json",
      content: JSON.stringify([
        {
          id: "cred_discobox_openai",
          integrationID: "openai",
          active: true,
          value: { type: "key", key: "old-db-sentinel" },
        },
      ]),
    };
    const input = {
      ...f.input,
      baseline: { files: [template] },
      sentinels: { [name]: "new-runtime-sentinel" },
      knownSentinels: { [name]: ["old-db-sentinel"] },
    };
    const kept = f.run(input);
    assert.equal(kept.credentials[0].keep, true);
    assert.equal(kept.credentials[0].value, undefined);
    assert.equal(kept.protectedPaths.length, 0);
    f.write("store.json", [
      {
        id: "new-login",
        integrationID: "openai",
        active: true,
        value: { type: "key", key: "new-real-token" },
      },
    ]);
    const changed = f.run(input);
    assert.equal(changed.credentials[0].keep, false);
    assert.equal(changed.credentials[0].value.token, "new-real-token");
  } finally {
    restore();
    f.close();
  }
});
test("prior alias from another binding cannot become a secret or keep the wrong provider", () => {
  const f = fixture("pi");
  try {
    f.write(".pi/agent/auth.json", {
      anthropic: { type: "api_key", key: "prior-openai-sentinel" },
    });
    const out = f.run({
      ...f.input,
      sentinels: { PI_OPENAI_CREDENTIAL: "current-openai-sentinel" },
      knownSentinels: { PI_OPENAI_CREDENTIAL: ["prior-openai-sentinel"] },
    });
    assert.equal(out.credentials.length, 0);
    assert(out.protectedPaths.includes(".pi/agent/auth.json"));
  } finally {
    f.close();
  }
});

test("launcher-only context lets a reminted delivery replace old DB sentinels without trusting it for capture", {
  skip: process.platform !== "linux" &&
    "Native handoff requires Linux /proc and POSIX executable stubs",
}, () => {
  const f = fixture("opencode"),
    name = "OPENCODE_OPENAI_CREDENTIAL";
  const row = (key) => ({
    id: "cred_discobox_openai",
    integrationID: "openai",
    active: true,
    value: { type: "key", key },
  });
  const restore = nativeStub(f, "opencode", [row("old-db-sentinel")]);
  try {
    f.write(".config/discobox/opencode-credentials.json", [
      row("current-sentinel"),
    ]);
    assert.equal(guard("opencode", f.definition, f.normalizer), false);
    f.write(".config/discobox/harness-sentinels.json", {
      sentinels: { [name]: "current-sentinel" },
      knownSentinels: { [name]: ["old-db-sentinel"] },
    });
    assert.equal(guard("opencode", f.definition, f.normalizer), true);
    f.write("store.json", [row("new-real-login")]);
    assert.equal(guard("opencode", f.definition, f.normalizer), false);
    // Even a forged launcher context cannot label a new login Keep in a
    // server-bound export. Only the private ExportInput may supply aliases.
    f.write(".config/discobox/harness-sentinels.json", {
      sentinels: { [name]: "current-sentinel" },
      knownSentinels: { [name]: ["new-real-login"] },
    });
    const exported = f.run({
      ...f.input,
      sentinels: { [name]: "current-sentinel" },
    });
    assert.equal(exported.credentials[0].keep, false);
    assert.equal(exported.credentials[0].value.token, "new-real-login");
    f.write(".config/discobox/harness-sentinels.json", '{"sentinels":');
    assert.equal(guard("opencode", f.definition, f.normalizer), false);
  } finally {
    restore();
    f.close();
  }
});

test("unknown-only fresh settings churn is absent while clearing delivered portable fields remains an edit", () => {
  const f = fixture("pi"),
    p = ".pi/agent/settings.json";
  try {
    f.write(p, { lastChangelogVersion: "1.4.0" });
    assert.equal(f.run().diagnostics.length, 0);
    assert.equal(
      f.run().files.some((file) => file.path === p),
      false,
    );
    const baseline = {
      files: [
        {
          path: p,
          content: JSON.stringify({
            defaultModel: "delivered-model",
            lastChangelogVersion: "1.3.0",
          }),
        },
      ],
    };
    const cleared = f.run({ ...f.input, baseline });
    assert.deepEqual(
      JSON.parse(cleared.files.find((file) => file.path === p).content),
      {},
    );
    // Saving the explicit clear converges: empty baseline and vendor-only local
    // state both normalize to absence, rather than producing a second edit.
    const emptyBaseline = { files: cleared.files };
    assert.equal(
      f
        .run({ ...f.input, baseline: emptyBaseline })
        .files.some((file) => file.path === p),
      false,
    );
    assert.equal(
      f
        .run({ ...f.input, baseline: emptyBaseline, baselineOnly: true })
        .files.some((file) => file.path === p),
      false,
    );
    f.write(p, {});
    assert.deepEqual(
      JSON.parse(
        f.run({ ...f.input, baseline }).files.find((file) => file.path === p)
          .content,
      ),
      {},
    );
  } finally {
    f.close();
  }
});

test("settings values are preserved without duplicating a vendor preference schema", () => {
  const f = fixture("pi"),
    p = ".pi/agent/settings.json";
  try {
    f.write(p, { defaultModel: 42 });
    const out = f.run({
      ...f.input,
      baseline: {
        files: [{ path: p, content: '{"defaultModel":"delivered"}' }],
      },
    });
    assert.equal(JSON.parse(out.files.find((file) => file.path === p).content).defaultModel, 42);
    assert(!out.diagnostics.some((d) => d.path === p));
  } finally {
    f.close();
  }
});

test("reconsider replaces an existing binding whose delivered template did not change", () => {
  const f = fixture("pi");
  try {
    f.write(".pi/agent/auth.json", {
      anthropic: { type: "api_key", key: "synthetic-replacement-login" },
    });
    const captured = f.run();
    const candidate = {
      id: "existing-binding-replacement",
      baseline: { files: captured.files },
      files: [],
      credentials: captured.credentials,
    };
    const projected = f.run({
      ...f.input,
      reconsider: {
        candidate,
        current: { files: captured.files },
        replaceCredentials: ["PI_ANTHROPIC_CREDENTIAL"],
      },
    });
    assert.deepEqual(projected.credentials, captured.credentials);
    const auth = projected.files.find((file) => file.path === ".pi/agent/auth.json");
    assert(auth);
    assert.match(auth.content, /PI_ANTHROPIC_CREDENTIAL/);
    assert(!auth.content.includes("synthetic-replacement-login"));
  } finally {
    f.close();
  }
});

test("bundled descriptive about metadata is ignored in current and delivered harness settings", () => {
  const f = fixture("pi");
  try {
    const p = ".config/discobox/pi-harness.json";
    const content = JSON.stringify({ about: "Bundled editor instructions", judgeModel: "" });
    f.write(p, content);
    for (const baselineOnly of [false, true]) {
      const result = f.run({
        ...f.input,
        baselineOnly,
        baseline: { files: [{ path: p, content }] },
      });
      assert(!result.diagnostics.some((d) => d.path === p));
      assert(!JSON.stringify(result.files).includes("Bundled editor instructions"));
    }
    f.write(p, { about: "Bundled text", judgeModel: "model", unknownOption: "preserved" });
    const unknown = f.run();
    assert(!unknown.diagnostics.some((d) => d.path === p));
    assert(JSON.stringify(unknown.files).includes("preserved"));
    f.write(p, { about: 123, judgeModel: "model" });
    assert(!f.run().diagnostics.some((d) => d.path === p));
    f.write(p, { about: "Bundled text", judgeModel: "model" });
    f.write(".pi/agent/settings.json", { about: "not harness metadata" });
    assert(!f.run().diagnostics.some((d) => d.path === ".pi/agent/settings.json"));
  } finally {
    f.close();
  }
});

test("Codex empty settings compare unrendered delivery templates without evaluating local templates", () => {
  const f = fixture("codex");
  try {
    const p = ".codex/config.toml";
    const bundled = {path:p,content:""};
    const trustOnly = '[projects."/source/project"]\ntrust_level = "trusted"\n';
    const run = (baseline) => f.run({ ...f.input, baseline: { files: [baseline] } });
    const config = (output) => output.files.find((file) => file.path === p);
    const noPartial = (output) => assert(!output.diagnostics.some(
      (d) => d.path === p && d.code === "partial",
    ));
    f.write(p, trustOnly);
    const untouched = run(bundled);
    noPartial(untouched);
    assert.equal(config(untouched), undefined);

    f.write(p, 'model = "saved-model"\n' + trustOnly);
    const saved = run(bundled);
    noPartial(saved);
    assert(config(saved).content.includes('model = "saved-model"'));

    f.write(p, trustOnly);
    // Both the original conditional and the save-back suffix may accompany a
    // delivered model. Clearing that last portable preference must be captured.
    for (const baseline of [config(saved), {
      ...bundled, content: 'model = "saved-model"\n' + bundled.content,
    }]) {
      const cleared = run(baseline);
      noPartial(cleared);
      assert.equal(config(cleared).content.trim(), "");
      const converged = run(config(cleared));
      noPartial(converged);
      assert.equal(config(converged), undefined);
    }

    f.write(p, bundled.content);
    noPartial(run(bundled));
    f.write(p, trustOnly);
    const unknown = run({ ...bundled, content: '{{ if .unknown }}\n' + bundled.content });
    assert(unknown.diagnostics.some((d) => d.path === p && d.code === "partial"));
  } finally {
    f.close();
  }
});


test("all harnesses preserve future preferences and redact known credential locations", () => {
  for (const [id, p, body, secret] of [
    ["claude-code", ".claude/settings.json", {theme:"light", newPreference:{nested:[true,{accent:"blue"}]},env:{ANTHROPIC_API_KEY:"local-secret"}}, "local-secret"],
    ["copilot", ".copilot/settings.json", {theme:"light",newPreference:{nested:[1,2]},authTokens:{host:"local-secret"}}, "local-secret"],
    ["pi", ".pi/agent/settings.json", {theme:"light",newPreference:{nested:[1,2]},apiKey:"local-secret"}, "local-secret"],
    ["opencode", ".config/opencode/opencode.json", {theme:"light",newPreference:{nested:[1,2]},provider:{custom:{options:{apiKey:"local-secret",baseURL:"https://provider.example"}}}}, "local-secret"],
    ["omp", ".omp/agent/config.yml", "theme: light\nnewPreference:\n  nested: [one, two]\napiKey: local-secret\n", "local-secret"],
    ["codex", ".codex/config.toml", 'theme = "light"\nnewPreference = { nested = ["one", "two"] }\n[model_providers.custom]\napi_key = "local-secret"\nbase_url = "https://provider.example"\n', "local-secret"],
  ]) {
    const f = fixture(id);
    try {
      f.write(p, body);
      const exported = f.run().files.find(file => file.path === p);
      assert(exported, id);
      assert(exported.content.includes("light"), id);
      assert(exported.content.includes("newPreference"), id);
      assert(!exported.content.includes(secret), id);
    } finally { f.close(); }
  }
});

test("rich TOML and YAML settings survive normalization", () => {
  const toml = project('theme = "light"\n[[profiles.work.tools]]\nname = "one"\nargs = ["--flag", "two"]\n[profiles.work.env]\nTOKEN = "secret"\n', {format:"toml",exclude:["profiles.*.env"]});
  assert(toml.content.includes("tools"));
  assert(toml.content.includes("--flag"));
  assert(!toml.content.includes("secret"));
  const yaml = project('theme: light\nfeature:\n  description: |\n    multiple\n    lines\n  sequence:\n    - enabled: true\n      names: [one, two]\n', {format:"yaml"});
  assert.equal(JSON.parse(yaml.content).feature.sequence[0].enabled, true);
});


test("Claude bundled settings and theme changes normalize without warning-only candidates", () => {
  const f = fixture("claude-code");
  try {
    const p = ".claude/settings.json";
    const baseline = require("../claude-code/image.json").harness.files.find(file => file.path === p);
    f.write(p, baseline.content);
    const delivered = f.run({...f.input,baseline:{files:[baseline]},baselineOnly:true});
    const unchanged = f.run({...f.input,baseline:{files:[baseline]}});
    assert.equal(unchanged.diagnostics.length, 0);
    assert.deepEqual(unchanged.files, delivered.files);
    const settings = JSON.parse(baseline.content);
    settings.theme = "light";
    f.write(p, settings);
    const changed = f.run({...f.input,baseline:{files:[baseline]}});
    assert.equal(changed.diagnostics.length, 0);
    assert.equal(JSON.parse(changed.files.find(file => file.path === p).content).theme,"light");
    const after = f.run({...f.input,baseline:{files:changed.files},baselineOnly:true});
    assert.deepEqual(after.files, changed.files);
    assert.equal(after.diagnostics.length, 0);
  } finally { f.close(); }
});


test("Codex parses local project tables without corrupting multiline strings", () => {
 const f=fixture("codex"), p=".codex/config.toml";
 try {
  f.write(p, 'developer_instructions = """\nExplain projects:\n[projects.example]\ntrust_level = "trusted"\n"""\n[projects.local]\ntrust_level = "trusted"\n[shell_environment_policy.set]\nAWS_SECRET_ACCESS_KEY = "never-copy"\n');
  const result=f.run();
  const config=result.files.find(file=>file.path===p);
  assert(config);
  assert(config.content.includes("Explain projects"));
  assert(config.content.includes("projects.example"));
  assert(!config.content.includes("projects.local"));
  assert(!config.content.includes("never-copy"));
  assert.equal(result.diagnostics.length,0);
  f.write(p, 'developer_instructions = "{{ .secrets.OTHER }}"\n');
  const unsafe=f.run();
  assert(unsafe.files.find(file=>file.path===p).content.includes("{{ .secrets.OTHER }}"));
  assert.equal(unsafe.diagnostics.length,0);
 } finally { f.close(); }
});


test("Claude signed-out OAuth is restorable but an unrecognized login stays protected", () => {
  const f = fixture("claude-code");
  try {
    const signedOut = { accessToken: "", refreshToken: "", expiresAt: 0,
      subscriptionTypeSource: "oauth", rateLimitTier: "default_claude_ai" };
    f.write(".claude/.credentials.json", { claudeAiOauth: signedOut });
    const out = f.run();
    assert.deepEqual(out.protectedPaths, []);
    assert.deepEqual(out.credentials, []);
    assert.deepEqual(out.diagnostics, []);
    for (const oauth of [
      { ...signedOut, accessToken: "!unrecognized-login" },
      { ...signedOut, refreshToken: "local-refresh-token" },
    ]) {
      f.write(".claude/.credentials.json", { claudeAiOauth: oauth });
      assert(f.run().protectedPaths.includes(".claude/.credentials.json"));
    }
  } finally { f.close(); }
});

test("Claude reconsider preserves literal onboarding state with and without a bound API key", () => {
  const f = fixture("claude-code");
  try {
    f.write(".claude/.credentials.json", { claudeAiOauth: {
      accessToken: "replacement-token", refreshToken: "replacement-refresh", expiresAt: 123,
    } });
    const captured = f.run();
    for (const bound of [false, true]) {
      const seed = { path: ".claude.json", content: JSON.stringify({hasCompletedOnboarding: true,
        ...(bound ? { primaryApiKey: {$secret: "ANTHROPIC_API_KEY"} } : {})}) };
      const files = [...captured.files, seed];
      const baseline = { files, secretRevisions: {
        CLAUDE_CODE_OAUTH_TOKEN: "oauth-revision",
        ...(bound ? { ANTHROPIC_API_KEY: "key-revision" } : {}),
      } };
      const projected = f.run({ ...f.input, reconsider: {
        candidate: { id: "claude-replacement", baseline, files: [], credentials: captured.credentials },
        current: baseline, replaceCredentials: ["CLAUDE_CODE_OAUTH_TOKEN"],
      } });
      assert.deepEqual(projected.credentials.find(c => c.envName === "CLAUDE_CODE_OAUTH_TOKEN"), captured.credentials[0]);
      assert(projected.files.some(f => f.path === ".claude/.credentials.json"));
      assert.equal(projected.credentials.some(c => c.envName === "ANTHROPIC_API_KEY" && c.keep), bound);
      if (!bound) assert.deepEqual(projected.files.find(f => f.path === ".claude.json"), seed);
    }
  } finally { f.close(); }
});

test("exporter CLI failures report a fixed reason without input contents", () => {
  const cp = require("child_process");
  const result = cp.spawnSync(process.execPath, [path.join(__dirname, "runtime.cjs"),
    "claude-code", path.join(__dirname, "../claude-code/image.json"), "export"],
    { input: '{"private-token":"do-not-log", broken}', encoding: "utf8" });
  assert.equal(result.status, 1);
  assert.equal(result.stderr, "discobox-capture: invalid-json\n");
  assert.equal(result.stdout, "");
});

test("Claude initialization preserves a local login and preferences across restarts", () => {
 const f=fixture("claude-code");
 try {
  f.write(".claude.json",{primaryApiKey:"newer-local-key",customPreference:true,projects:{"/another":{hasTrustDialogAccepted:true}}});
  initialize("claude-code");initialize("claude-code");
  const state=JSON.parse(fs.readFileSync(path.join(f.home,".claude.json"),"utf8"));
  assert.equal(state.primaryApiKey,"newer-local-key");assert.equal(state.customPreference,true);
  assert.equal(state.projects[process.cwd()].hasTrustDialogAccepted,true);
  assert.equal(state.projects["/another"].hasTrustDialogAccepted,true);
  assert.equal(state.hasCompletedOnboarding,true);assert.equal(state.hasSeenAutoDefaultNudge,true);
  f.write(".claude.json","{broken");
  assert.throws(()=>initialize("claude-code"));
  assert.equal(fs.readFileSync(path.join(f.home,".claude.json"),"utf8"),"{broken");
 }finally{f.close();}
});
