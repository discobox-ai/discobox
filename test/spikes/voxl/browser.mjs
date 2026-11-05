import { spawn } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { attach } from "./cdp.mjs";
export async function browser({
  url = "about:blank",
  headless = true,
  args = [],
} = {}) {
  const profile = await mkdtemp(path.join(os.tmpdir(), "voxl-spike-chrome-"));
  const child = spawn(
    process.env.CHROMIUM || "chromium",
    [
      ...(headless ? ["--headless"] : []),
      "--no-sandbox",
      "--disable-dev-shm-usage",
      "--no-first-run",
      "--no-default-browser-check",
      "--autoplay-policy=no-user-gesture-required",
      `--user-data-dir=${profile}`,
      "--remote-debugging-port=0",
      ...args,
      url,
    ],
    { stdio: ["ignore", "ignore", "pipe"] },
  );
  try {
    const port = await new Promise((resolve, reject) => {
      const timeout = setTimeout(
        () => reject(new Error("Chrome did not start")),
        30000,
      );
      child.on("error", (error) => {
        clearTimeout(timeout);
        reject(error);
      });
      child.on("exit", (code) => {
        clearTimeout(timeout);
        reject(new Error(`Chrome exited ${code}`));
      });
      child.stderr.on("data", (data) => {
        const match = String(data).match(
          /DevTools listening on ws:\/\/127\.0\.0\.1:(\d+)/,
        );
        if (match) {
          clearTimeout(timeout);
          resolve(Number(match[1]));
        }
      });
    });
    const cdp = await attach(port);
    let stopPromise;
    return {
      ...cdp,
      child,
      port,
      stop: () => {
        if (!stopPromise)
          stopPromise = (async () => {
            try {
              await cdp.send("Browser.close");
            } catch {
              /* browser can close before replying */
            }
            cdp.close();
            child.kill("SIGTERM");
            await new Promise((resolve) => {
              if (child.exitCode !== null || child.signalCode !== null)
                return resolve();
              const force = setTimeout(() => child.kill("SIGKILL"), 5000);
              child.once("exit", () => {
                clearTimeout(force);
                resolve();
              });
            });
            await rm(profile, {
              recursive: true,
              force: true,
              maxRetries: 10,
              retryDelay: 100,
            });
          })();
        return stopPromise;
      },
    };
  } catch (error) {
    child.kill();
    await rm(profile, {
      recursive: true,
      force: true,
      maxRetries: 10,
      retryDelay: 100,
    });
    throw error;
  }
}

export const waitConnected = (viewer) =>
  viewer.evaluate(`new Promise((resolve,reject)=>{
 const deadline=performance.now()+30000;
 const check=()=>{
  if(document.getElementById('status-text')?.textContent.startsWith('Connected')) return resolve(true);
  if(performance.now()>deadline) return reject(new Error(document.body.innerText));
  setTimeout(check,100);
 }; check();
})`);
