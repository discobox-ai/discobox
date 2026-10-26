"use strict";
exports.oauth = function (auth, provider) {
  const tokens = auth.tokens || {};
  const accessToken = tokens.access_token;
  const refreshToken = tokens.refresh_token;
  if (
    typeof accessToken !== "string" ||
    !accessToken ||
    typeof refreshToken !== "string" ||
    !refreshToken
  ) {
    throw new Error("incomplete credential");
  }
  const decodeClaims = (jwt) => {
    if (typeof jwt !== "string") return null;
    const parts = jwt.split(".");
    if (parts.length !== 3 || !parts[1]) return null;
    try {
      return JSON.parse(Buffer.from(parts[1], "base64url").toString("utf8"));
    } catch (err) {
      return null;
    }
  };
  const secret = {
    token: accessToken,
    refreshToken,
    tokenUrl: provider.oauth.tokenUrl,
    clientId: provider.oauth.clientId,
  };
  // The access token is a JWT, so its own expiry is on hand. Recording it
  // is what lets the control plane refresh just ahead of expiry instead of
  // discovering it from a 401.
  const accessClaims = decodeClaims(accessToken);
  if (accessClaims && typeof accessClaims.exp === "number") {
    secret.accessTokenExpiresAt = accessClaims.exp * 1000;
  }
  const idClaims = decodeClaims(tokens.id_token) || {};
  const authClaims = idClaims["https://api.openai.com/auth"] || {};
  if (
    typeof authClaims.chatgpt_plan_type === "string" &&
    authClaims.chatgpt_plan_type
  ) {
    secret.subscriptionType = authClaims.chatgpt_plan_type;
  }
  const account = {};
  const email =
    idClaims.email || (idClaims["https://api.openai.com/profile"] || {}).email;
  if (typeof email === "string" && email) account.email = email;
  for (const claim of [
    "chatgpt_plan_type",
    "chatgpt_user_id",
    "chatgpt_account_id",
  ]) {
    if (typeof authClaims[claim] === "string" && authClaims[claim])
      account[claim] = authClaims[claim];
  }
  if (authClaims.chatgpt_account_is_fedramp === true)
    account.chatgpt_account_is_fedramp = true;
  const accountId =
    typeof tokens.account_id === "string" && tokens.account_id
      ? tokens.account_id
      : account.chatgpt_account_id || null;
  return { secret, tokens: { account, accountId } };
};
exports.oauthAuthFile = (tokens, envName = "CODEX_OAUTH_TOKEN") => {
  const b64url = (value) =>
    Buffer.from(JSON.stringify(value), "utf8").toString("base64url");
  const claims = {};
  if (tokens.account.email) claims.email = tokens.account.email;
  const authClaims = {};
  for (const claim of [
    "chatgpt_plan_type",
    "chatgpt_user_id",
    "chatgpt_account_id",
  ]) {
    if (tokens.account[claim]) authClaims[claim] = tokens.account[claim];
  }
  if (tokens.account.chatgpt_account_is_fedramp)
    authClaims.chatgpt_account_is_fedramp = true;
  claims["https://api.openai.com/auth"] = authClaims;
  const idToken = `${b64url({ alg: "none", typ: "JWT" })}.${b64url(claims)}.discobox`;
  return {
    path: ".codex/auth.json",
    template: true,
    content:
      JSON.stringify(
        {
          OPENAI_API_KEY: null,
          tokens: {
            id_token: idToken,
            access_token: `{{ .secrets.${envName} }}`,
            refresh_token: "discobox-refresh-happens-in-the-control-plane",
            account_id: tokens.accountId,
          },
          last_refresh: "2100-01-01T00:00:00Z",
        },
        null,
        2,
      ) + "\n",
  };
};

// Local trust is state, not a portable preference. Configure and save-back
// remove every local project section before adding the delivery-time stanza.
exports.stripProjects = function (body) {
  const kept = [];
  let dropping = false;
  for (const line of body.split("\n")) {
    const header = line.match(/^\s*\[\[?\s*(.*?)\s*\]\]?\s*(?:#.*)?$/);
    if (header) {
      dropping = /^projects(?:\s*\.|\s*$)/.test(header[1]);
      if (dropping) continue;
    } else if (/^\s*projects\s*=/.test(line)) continue;
    if (!dropping) kept.push(line);
  }
  return kept.join("\n").trimEnd();
};
