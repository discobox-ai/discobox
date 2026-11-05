// Parent-side fixture for one test sandbox's console overview. Loopback calls
// inherit dev-user authority, so IP filtering alone is not sufficient.
import http from "node:http";
import net from "node:net";
const [sandboxIP, sandboxID] = process.argv.slice(2);
if (!net.isIP(sandboxIP || "") || !/^sbx_[a-z0-9]+$/.test(sandboxID || ""))
  throw new Error("usage: console-bridge.mjs SANDBOX_IP SANDBOX_ID");
const prefix = "/projects/default";
const metadata = new Set([
  prefix,
  `${prefix}/pools`,
  `${prefix}/sandboxes`,
  `${prefix}/harness-configs`,
]);
// These badges were empty in the measurement fixture. Do not expose another
// sandbox's approval/rejection details merely to render an empty badge.
const emptyBadges = new Map([
  [`${prefix}/approval-requests`, { approvalRequests: [] }],
  [`${prefix}/secret-rejections`, { secretRejections: [] }],
]);
function json(res, status, value) {
  res.writeHead(status, {
    "Content-Type": "application/json",
    "Cache-Control": "no-store",
  });
  res.end(JSON.stringify(value));
}
const server = http.createServer((req, res) => {
  let url;
  try {
    url = new URL(req.url, "http://127.0.0.1");
  } catch {
    res.writeHead(400);
    res.end();
    return;
  }
  if (
    req.socket.remoteAddress !== sandboxIP ||
    !["GET", "HEAD"].includes(req.method) ||
    !req.url.startsWith("/") ||
    req.url.startsWith("//") ||
    (!metadata.has(url.pathname) && !emptyBadges.has(url.pathname))
  ) {
    res.writeHead(403);
    res.end("outside the console metadata fixture");
    return;
  }
  if (emptyBadges.has(url.pathname)) {
    json(res, 200, emptyBadges.get(url.pathname));
    return;
  }
  const upstream = http.request(
    {
      hostname: "127.0.0.1",
      port: 8080,
      path: url.pathname + url.search,
      method: "GET",
      // Do not forward a sandbox-supplied credential, assertion or cookie.
      headers: { host: "127.0.0.1:8080", accept: "application/json" },
    },
    async (response) => {
      try {
        if (response.statusCode !== 200) {
          response.resume();
          json(res, 502, { error: "dev metadata unavailable" });
          return;
        }
        const chunks = [];
        let bytes = 0;
        for await (const chunk of response) {
          bytes += chunk.length;
          if (bytes > 4 * 1024 * 1024)
            throw new Error("fixture metadata too large");
          chunks.push(chunk);
        }
        let body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
        if (url.pathname.endsWith("/sandboxes")) {
          body = {
            sandboxes: (body.sandboxes || []).filter(
              (box) => box.id === sandboxID,
            ),
          };
        }
        if (url.pathname.endsWith("/harness-configs")) {
          // Harness DTOs also contain configuredFiles/fileOverrides/file content.
          // The overview needs identities, not anyone's captured account files.
          const fields = [
            "id",
            "projectId",
            "slug",
            "name",
            "builtIn",
            "createdAt",
            "updatedAt",
            "boundSecrets",
            "revision",
          ];
          body = {
            harnessConfigs: (body.harnessConfigs || []).map((config) =>
              Object.fromEntries(
                fields
                  .filter((key) => Object.hasOwn(config, key))
                  .map((key) => [key, config[key]]),
              ),
            ),
          };
        }
        json(res, 200, body);
      } catch {
        response.destroy();
        if (!res.headersSent)
          json(res, 502, { error: "invalid fixture metadata" });
        else res.destroy();
      }
    },
  );
  upstream.on("error", () => {
    if (!res.headersSent) json(res, 502, { error: "dev metadata unavailable" });
    else res.destroy();
  });
  upstream.end();
});
server.on("upgrade", (_req, socket) =>
  socket.end("HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n"),
);
server.listen(18080, process.env.SPIKE_BRIDGE_ADDR || "172.19.0.1", () =>
  console.log(`console metadata fixture: ${sandboxIP}, ${sandboxID}`),
);
