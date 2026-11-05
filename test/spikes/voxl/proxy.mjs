// Disposable, localhost-only spike overlay. Run inside the desktop sandbox.
import http from "node:http";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

const port = Number(process.env.SPIKE_PORT || 6902);
const desktop = new URL(process.env.DESKTOP_URL || "http://127.0.0.1:6900");
const voxl = new URL(process.env.VOXL_URL || "http://127.0.0.1:6901");
const appPath = process.argv[2];
if (!appPath)
  throw new Error("usage: node proxy.mjs /path/to/desktop/web/app.js");
let app = await readFile(appPath, "utf8");
for (const [from, to] of [
  [
    "function connect() {",
    "function connect() {\n  if (new URLSearchParams(location.search).get('renderer') === 'voxl') return connectVoxl();",
  ],
  [
    "function canvasBox() {",
    "function canvasBox() {\n  if (spikeFrame) return voxlCanvasBox();",
  ],
]) {
  if (app.split(from).length !== 2)
    throw new Error(`viewer changed: expected one ${from}`);
  app = app.replace(from, to);
}
// Put state declarations before boot() can connect; functions remain hoisted.
const overlay = await readFile(
  fileURLToPath(new URL("./overlay.js", import.meta.url)),
  "utf8",
);
app = overlay + "\n" + app;
const hosts = new Set([
  "localhost",
  "127.0.0.1",
  "[::1]",
  ...(process.env.SPIKE_ALLOWED_HOSTS || "").split(",").filter(Boolean),
]);
function allowed(req) {
  try {
    const host = new URL(`http://${req.headers.host}`).hostname;
    return (
      hosts.has(host) &&
      (!req.headers.origin ||
        new URL(req.headers.origin).host === req.headers.host)
    );
  } catch {
    return false;
  }
}
const metrics = { upstreamBytes: 0, downstreamBytes: 0, connections: 0 };
function targetFor(req) {
  const isVoxl = req.url.startsWith("/voxl/");
  return {
    target: isVoxl ? voxl : desktop,
    path: isVoxl ? req.url.slice(5) : req.url,
  };
}
function options(req, target, path) {
  const headers = { ...req.headers, host: target.host };
  if (headers.origin) headers.origin = target.origin;
  return {
    hostname: target.hostname,
    port: target.port,
    path,
    method: req.method,
    headers,
  };
}
const server = http.createServer(async (req, res) => {
  if (!allowed(req)) {
    res.writeHead(403);
    res.end("host or origin refused");
    return;
  }
  if (req.url === "/spike/metrics") {
    if (req.method === "POST")
      Object.assign(metrics, {
        upstreamBytes: 0,
        downstreamBytes: 0,
        connections: 0,
      });
    res.writeHead(200, {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
    });
    res.end(JSON.stringify(metrics));
    return;
  }
  if (req.url === "/app.js") {
    res.writeHead(200, {
      "Content-Type": "text/javascript",
      "Cache-Control": "no-store",
    });
    res.end(app);
    return;
  }
  const { target, path } = targetFor(req);
  const upstream = http.request(options(req, target, path), (response) => {
    res.writeHead(response.statusCode, response.headers);
    response.pipe(res);
  });
  upstream.on("error", (error) => {
    if (!res.headersSent) res.writeHead(502);
    res.end(error.message);
  });
  req.pipe(upstream);
});
server.on("upgrade", (req, client, clientHead) => {
  if (!allowed(req)) {
    client.end("HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n");
    return;
  }
  const { target, path } = targetFor(req);
  const upstream = http.request(options(req, target, path));
  upstream.on("upgrade", (response, remote, remoteHead) => {
    metrics.connections++;
    client.write(`HTTP/1.1 ${response.statusCode} Switching Protocols\r\n`);
    for (let i = 0; i < response.rawHeaders.length; i += 2)
      client.write(
        `${response.rawHeaders[i]}: ${response.rawHeaders[i + 1]}\r\n`,
      );
    client.write("\r\n");
    if (remoteHead.length) client.write(remoteHead);
    if (clientHead.length) remote.write(clientHead);
    remote.on("data", (data) => {
      metrics.downstreamBytes += data.length;
    });
    client.on("data", (data) => {
      metrics.upstreamBytes += data.length;
    });
    remote.on("error", () => client.destroy());
    client.on("error", () => remote.destroy());
    remote.on("close", () => client.destroy());
    client.on("close", () => remote.destroy());
    remote.pipe(client).pipe(remote);
  });
  upstream.on("response", (response) => {
    client.end(
      `HTTP/1.1 ${response.statusCode} Upgrade refused\r\nConnection: close\r\n\r\n`,
    );
    response.resume();
  });
  upstream.on("error", () => client.destroy());
  upstream.end();
});
server.listen(port, "127.0.0.1", () =>
  console.log(
    `spike: http://localhost:${port}/?renderer=voxl (noVNC: ?renderer=novnc)`,
  ),
);
