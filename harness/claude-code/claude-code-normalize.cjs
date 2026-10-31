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

  content: JSON.stringify(
    {
      claudeAiOauth: {
        // Only this credential leaf is bound to a sentinel.
        accessToken: { $secret: envName },
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
