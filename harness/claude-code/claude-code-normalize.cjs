"use strict";
exports.oauth = function (oauth, provider) {
  if (
    typeof oauth.accessToken !== "string" ||
    !oauth.accessToken ||
    typeof oauth.refreshToken !== "string" ||
    !oauth.refreshToken
  ) {
    throw new Error("incomplete credential");
  }
  const out = {
    token: oauth.accessToken,
    refreshToken: oauth.refreshToken,
    tokenUrl: provider.oauth.tokenUrl,
    clientId: provider.oauth.clientId,
  };
  if (typeof oauth.expiresAt === "number") {
    out.accessTokenExpiresAt = oauth.expiresAt;
  }
  // Copied verbatim, never defaulted: an absent scope list has to stay
  // absent so nothing downstream can mistake a guess for what the
  // authorization server actually granted.
  if (Array.isArray(oauth.scopes)) {
    const scopes = oauth.scopes.filter((s) => typeof s === "string" && s);
    if (scopes.length) {
      out.scopes = scopes;
    }
  }
  if (typeof oauth.subscriptionType === "string" && oauth.subscriptionType) {
    out.subscriptionType = oauth.subscriptionType;
  }
  return out;
};
exports.credentialsFile = (oauth, envName = "CLAUDE_CODE_OAUTH_TOKEN") => ({
  path: ".claude/.credentials.json",
  template: true,
  content: JSON.stringify(
    {
      claudeAiOauth: {
        // Dotted field access, never a quoted map key. This whole
        // object is JSON-encoded to become the file's content, so a
        // quote inside a template action comes out backslash-escaped
        // and Go's parser rejects it ("unexpected \\ in operand") —
        // which breaks every sandbox launch while configure still
        // reports success. A secret's env name is always a valid
        // template field name (HarnessConfigEnvVarNamePattern), so
        // there is nothing to quote.
        accessToken: `{{ .secrets.${envName} }}`,
        refreshToken: "discobox-refresh-happens-in-the-control-plane",
        expiresAt: 4102444800000,
        scopes: oauth.scopes,
        subscriptionType: oauth.subscriptionType,
      },
    },
    null,
    2,
  ),
});
