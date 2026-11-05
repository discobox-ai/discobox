import { spawn } from "node:child_process";
const children = [];
const stop = () => {
  for (const child of children) {
    child.kill("SIGCONT");
    child.kill("SIGTERM");
  }
};
for (const signal of ["SIGINT", "SIGTERM"]) process.on(signal, stop);
process.on("exit", stop);
const streamer = spawn(
  process.env.VOXL_BIN || "voxl",
  [
    "--addr=127.0.0.1",
    "--port=6901",
    "--enable-basic-auth=false",
    "--audio-enabled=false",
    "--file-transfers=none",
    "--enable-resize=false",
    "--stripes=1",
  ],
  {
    stdio: "inherit",
    env: { ...process.env, DISPLAY: process.env.DISPLAY || ":0" },
  },
);
children.push(streamer);
console.log(`voxl spike streamer PID: ${streamer.pid}`);
const proxy = spawn(
  process.execPath,
  [new URL("./proxy.mjs", import.meta.url).pathname, ...process.argv.slice(2)],
  { stdio: "inherit" },
);
children.push(proxy);
for (const child of children) {
  child.on("error", (error) => {
    console.error(error);
    process.exitCode = 1;
    stop();
  });
  child.on("exit", (code) => {
    if (code) process.exitCode = code;
    stop();
  });
}
