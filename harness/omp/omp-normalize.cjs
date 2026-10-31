// Shared by interactive configure and background save-back.
"use strict";
exports.normalize = function (
  exported,
  previous = {},
  previousSentinels = {},
  definition,
) {
  const farFuture = 4102444800000;
  const placeholder = "discobox-refresh-happens-in-the-control-plane";

  // The previous run's entries and secret types, by the env name each
  // entry's secret reference names.
  const action = /"\$secret"\s*:\s*"([A-Za-z_][A-Za-z0-9_]*)"/;
  const previousEntries = new Map();
  const kept = (previous.files || []).find(
    (f) => f && f.path === ".config/discobox/omp-credentials.json",
  );
  try {
    const doc = JSON.parse(kept ? kept.content : "{}") || {};
    for (const entry of Array.isArray(doc.credentials) ? doc.credentials : []) {
      const match = JSON.stringify(entry).match(action);
      if (match) previousEntries.set(match[1], entry);
    }
  } catch (err) {
    // A previous file that does not parse keeps nothing.
  }
  const previousTypes = new Map(
    (previous.secrets || []).map((s) => [s && s.envName, s && s.type]),
  );

  // oauthRefresh: the providers whose OAuth login the control plane can
  // renew, and how. Each is a property of that provider's public OAuth
  // client — its token endpoint, client id, and request encoding — not of
  // any one sign-in, so they are fixed here rather than read from the
  // export. omp signs in to these with the same clients the claude-code and
  // codex images (and opencode's xAI) are refreshed for. A provider not
  // listed is delivered as a plain token.
  const oauthRefresh = Object.fromEntries(
    definition.providers
      .filter((p) => p.oauth)
      .map((p) => [
        p.id,
        { ...p.oauth, tokenRequestEncoding: p.oauth.encoding },
      ]),
  );

  // Which secret each credential is. omp keeps several accounts of one
  // provider side by side, and exports them in whatever order its store
  // holds them, which a sign-in replacing one can change — so a position
  // in the export names nothing. Three passes, each taking only names the
  // ones before it left:
  //   1. A credential still holding the PREV_ sentinel of a previous
  //      secret is that secret, kept.
  //   2. A new credential for an account a previous secret held — the
  //      same provider, accountId and email — replaces that secret in
  //      place.
  //   3. Anything else takes the provider's lowest free name: the first
  //      account OMP_<PROVIDER>_CREDENTIAL, the next _2.
  const baseName = (provider) =>
    "OMP_" +
    provider
      .toUpperCase()
      .replace(/[^A-Z0-9]+/g, "_")
      .replace(/^_+|_+$/g, "") +
    "_CREDENTIAL";
  const identity = (provider, credential) =>
    credential && (credential.accountId || credential.email)
      ? JSON.stringify([
          provider,
          credential.accountId || "",
          credential.email || "",
        ])
      : "";
  const sentinels = new Map();
  for (const name of previousEntries.keys()) {
    const sentinel = previousSentinels[name];
    if (sentinel) sentinels.set(sentinel, name);
  }
  const credentials = [];
  for (const item of Array.isArray(exported.credentials)
    ? exported.credentials
    : []) {
    const provider = item && item.provider;
    const value = item && item.credential;
    if (
      typeof provider !== "string" ||
      !provider ||
      !value ||
      typeof value !== "object"
    )
      continue;
    const material =
      value.type === "api_key"
        ? value.key
        : value.type === "oauth"
          ? value.access
          : undefined;
    if (typeof material !== "string" || !material) continue;
    credentials.push({ provider, value, material });
  }
  const taken = new Set();
  for (const c of credentials) {
    const name = sentinels.get(c.material);
    if (name && !taken.has(name)) {
      c.envName = name;
      c.kept = true;
      taken.add(name);
    }
  }
  for (const c of credentials) {
    // A sentinel no secret claimed is a copy of one already kept, and
    // a sentinel is never captured as a credential's value.
    if (c.envName || sentinels.has(c.material)) continue;
    const id = identity(c.provider, c.value);
    if (!id) continue;
    for (const [name, entry] of previousEntries) {
      if (
        !taken.has(name) &&
        identity(entry.provider, entry.credential) === id
      ) {
        c.envName = name;
        taken.add(name);
        break;
      }
    }
  }
  for (const c of credentials) {
    if (c.envName || sentinels.has(c.material)) continue;
    const base = baseName(c.provider);
    let name = base;
    for (let n = 2; taken.has(name); n++) name = `${base}_${n}`;
    c.envName = name;
    taken.add(name);
  }

  // notice says when a login nothing renews stops working, or that it
  // already has: a kept one as much as a new one, since a reconfigure
  // about something else would otherwise carry a dead login on in
  // silence.
  const notice = (provider, credential) => {
    if (!credential || credential.type !== "oauth") return undefined;
    const expires = Number(credential.expires) || 0;
    if (expires <= 0 || expires >= farFuture) return undefined;
    const day = new Date(expires).toISOString().slice(0, 10);
    if (expires <= Date.now()) {
      return `${provider}'s sign-in expired ${day}, and Discobox cannot renew it. Sign in to it again, or with an API key.`;
    }
    return `${provider}'s sign-in expires ${day}, and Discobox cannot renew it. Configure again after that, or sign in with an API key.`;
  };

  const plan = [];
  for (const { provider, value, envName, kept } of credentials) {
    if (!envName) continue;
    const label = value.email
      ? `omp: ${provider} (${value.email})`
      : `omp: ${provider}`;
    if (kept) {
      const entry = previousEntries.get(envName);
      const type = previousTypes.get(envName) || "token";
      plan.push({
        envName,
        provider,
        label,
        kept: true,
        type,
        entry,
        notice:
          type === "token" ? notice(provider, entry.credential) : undefined,
      });
      continue;
    }
    // Explicit JSON data binds this credential to its sandbox sentinel.
    const reference = { $secret: envName };
    const planned = { envName, provider, label, kept: false };
    if (value.type === "api_key") {
      planned.type = "token";
      planned.value = { token: value.key };
      planned.entry = { provider, credential: { ...value, key: reference } };
    } else {
      const spec = oauthRefresh[provider];
      if (spec && value.refresh && value.refresh !== value.access) {
        planned.type = "oauth";
        planned.value = {
          token: value.access,
          refreshToken: value.refresh,
          tokenUrl: spec.tokenUrl,
          clientId: spec.clientId,
        };
        if (spec.tokenRequestEncoding)
          planned.value.tokenRequestEncoding = spec.tokenRequestEncoding;
        if (Number(value.expires) > 0)
          planned.value.accessTokenExpiresAt = Number(value.expires);
        // The refresh token stays in the control plane, so a refresh
        // from inside a sandbox could not succeed — and does not need
        // to, since a sentinel does not go stale.
        planned.entry = {
          provider,
          credential: {
            ...value,
            access: reference,
            refresh: placeholder,
            expires: farFuture,
          },
        };
      } else {
        planned.type = "token";
        planned.value = { token: value.access };
        // A provider that authenticates with the refresh field itself
        // (GitHub Copilot stores its token in both) gets the sentinel
        // there too; a real refresh token becomes the placeholder.
        planned.entry = {
          provider,
          credential: {
            ...value,
            access: reference,
            refresh: value.refresh === value.access ? reference : placeholder,
          },
        };
        planned.notice = notice(provider, value);
      }
    }
    plan.push(planned);
  }
  return { plan, skipped: [] };
};
