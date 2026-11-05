// Source browser workload, kept running while alternating viewer measurements.
import http from "node:http";
import { readFile, writeFile } from "node:fs/promises";
import { browser } from "./browser.mjs";
const html = await readFile(new URL("./workload.html", import.meta.url));
const server = http.createServer((_req, res) => {
  res.writeHead(200, { "Content-Type": "text/html" });
  res.end(html);
});
await new Promise((resolve) => server.listen(7070, "127.0.0.1", resolve));
const source = await browser({
  headless: false,
  url: "http://127.0.0.1:7070/",
  args: [
    "--kiosk",
    "--disable-background-timer-throttling",
    "--disable-renderer-backgrounding",
  ],
});
await source.evaluate(
  `new Promise(resolve=>{const f=()=>{if(window.mode!==undefined){window.mode='browser';resolve(true);}else setTimeout(f,100);};f();})`,
);
await writeFile("/tmp/voxl-spike-source-port", String(source.port));
console.log(`source browser on CDP ${source.port}`);
let stopping = false;
for (const signal of ["SIGINT", "SIGTERM"])
  process.on(signal, async () => {
    if (stopping) return;
    stopping = true;
    await source.stop();
    server.close();
  });
