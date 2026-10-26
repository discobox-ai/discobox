"use strict";
exports.entry = function (state) {
  const user = state.lastLoggedInUser || {};
  if (typeof user.host !== "string" || typeof user.login !== "string")
    return undefined;
  if (user.host.replace(/\/+$/, "") !== "https://github.com")
    throw Error("unsupported host");
  const tokens = state.authTokens || {},
    key = `${user.host}:${user.login}`;
  const localId = tokens[key] ? key : key + ":github",
    value = tokens[localId];
  if (!value || typeof value.token !== "string" || !value.token)
    return undefined;
  return { provider: "github", value, raw: value, localId };
};
