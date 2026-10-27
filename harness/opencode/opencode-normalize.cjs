// Shared by interactive configure and background save-back.
"use strict";
exports.normalize = function (
  stored,
  previous = {},
  sentinels = {},
  definition,
) {
  const farFuture = 4102444800000;
  const placeholder = "discobox-refresh-happens-in-the-control-plane";

  // The previous run's entries and secret types, by the env name each
  // entry's template action names.
  const action = /\{\{\s*\.secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/;
  const previousEntries = new Map();
  try {
    for (const entry of JSON.parse(
      (previous.files || []).find(
        (f) => f.path === ".config/discobox/opencode-credentials.json",
      )?.content || "[]",
    )) {
      const match = JSON.stringify(entry).match(action);
      if (match) previousEntries.set(match[1], entry);
    }
  } catch (err) {
    // A previous file that does not parse keeps nothing.
  }
  const previousTypes = new Map(
    (previous.secrets || []).map((s) => [s && s.envName, s && s.type]),
  );

  // oauthRefresh: the providers whose OAuth sign-in the control plane can
  // renew, and how. Each is a property of that provider's public OAuth
  // client — its token endpoint, client id, and request encoding — not of
  // any one sign-in, so they are fixed here rather than read from the
  // store. opencode 2 signs in with the same clients opencode 1 did
  // (measured on 2.0.29). A provider not listed is delivered as a plain
  // token.
  const oauthRefresh = Object.fromEntries(
    definition.providers
      .filter((p) => p.oauth)
      .map((p) => [
        p.id,
        { ...p.oauth, tokenRequestEncoding: p.oauth.encoding },
      ]),
  );

  const byIntegration = new Map();
  for (const entry of stored) {
    if (!entry || typeof entry.integrationID !== "string" || !entry.value)
      continue;
    if (!byIntegration.has(entry.integrationID))
      byIntegration.set(entry.integrationID, []);
    byIntegration.get(entry.integrationID).push(entry);
  }
  const materialOf = (value) =>
    value.type === "key" ? value.key : value.access;

  const envNames = new Set();
  const envNameFor = (provider) => {
    const base =
      "OPENCODE_" +
      provider
        .toUpperCase()
        .replace(/[^A-Z0-9]+/g, "_")
        .replace(/^_+|_+$/g, "") +
      "_CREDENTIAL";
    let name = base;
    for (let n = 2; envNames.has(name); n++) name = `${base}_${n}`;
    envNames.add(name);
    return name;
  };

  const plan = [];
  for (const provider of [...byIntegration.keys()].sort()) {
    const envName = envNameFor(provider);
    const sentinel = sentinels[envName];
    // One credential per provider: the one opencode uses, which is the
    // active one when the user connected more than one account. A new
    // sign-in beats the seeded sentinel if both read as active, since
    // it is the one the user just made.
    const rank = (entry) =>
      (entry.active ? 2 : 0) +
      (sentinel && materialOf(entry.value) === sentinel ? 0 : 1);
    const chosen = byIntegration
      .get(provider)
      .reduce((best, entry) => (rank(entry) > rank(best) ? entry : best));
    const value = chosen.value;
    const material = materialOf(value);
    if (typeof material !== "string" || !material) {
      envNames.delete(envName);
      continue;
    }
    if (sentinel && material === sentinel && previousEntries.has(envName)) {
      plan.push({
        envName,
        provider,
        kept: true,
        type: previousTypes.get(envName) || "token",
        entry: previousEntries.get(envName),
      });
      continue;
    }
    // Dotted field access, never a quoted map key: this content is JSON,
    // so a quote inside a template action is escaped into the file itself
    // and Go's template parser rejects it — which breaks every sandbox
    // launch while configure still reports success. An env name is
    // always a valid template field name, so there is nothing to quote.
    const template = `{{ .secrets.${envName} }}`;
    const item = { envName, provider, kept: false };
    let delivered;
    if (value.type === "key") {
      item.type = "token";
      item.value = { token: value.key };
      delivered = { ...value, key: template };
    } else {
      const spec = oauthRefresh[provider];
      if (spec && value.refresh && value.refresh !== value.access) {
        item.type = "oauth";
        item.value = {
          token: value.access,
          refreshToken: value.refresh,
          tokenUrl: spec.tokenUrl,
          clientId: spec.clientId,
        };
        if (spec.tokenRequestEncoding)
          item.value.tokenRequestEncoding = spec.tokenRequestEncoding;
        if (Number(value.expires) > 0)
          item.value.accessTokenExpiresAt = Number(value.expires);
        // The refresh token stays in the control plane, so a refresh
        // from inside a sandbox could not succeed — and does not need
        // to, since a sentinel does not go stale.
        delivered = {
          ...value,
          access: template,
          refresh: placeholder,
          expires: farFuture,
        };
      } else {
        item.type = "token";
        item.value = { token: value.access };
        // A provider that authenticates with the refresh field itself
        // (GitHub Copilot stored its token in both under opencode 1)
        // gets the sentinel there too; a real refresh token becomes
        // the placeholder.
        delivered = {
          ...value,
          access: template,
          refresh:
            value.refresh === value.access
              ? template
              : value.refresh
                ? placeholder
                : value.refresh,
        };
        const expires = Number(value.expires) || 0;
        if (expires > 0 && expires < farFuture) {
          item.notice = `${provider}'s sign-in expires ${new Date(expires).toISOString().slice(0, 10)}, and Discobox cannot renew it. Configure again after that, or connect it with an API key.`;
        }
      }
    }
    // Made the image's own entry, under the id that tells it from one
    // the user connects by hand, by opencode-credentials own.
    item.entry = { integrationID: provider, value: delivered };
    plan.push(item);
  }
  return { plan, skipped: [] };
};
