// node measure.mjs URL SECONDS browser|console|idle
// Run in the sandbox. Only one viewer should be connected during each run.
import { browser, waitConnected } from "./browser.mjs";
import { readFile, readdir, writeFile } from "node:fs/promises";
import { setTimeout as delay } from "node:timers/promises";
const [url, seconds = "20", workload = "browser"] = process.argv.slice(2);
const duration = Number(seconds);
if (
  !url ||
  !Number.isFinite(duration) ||
  !(duration > 0) ||
  !["browser", "console", "idle"].includes(workload)
)
  throw new Error("usage: measure.mjs URL SECONDS browser|console|idle");
const renderer = new URL(url).searchParams.get("renderer") || "novnc";
if (!["voxl", "novnc"].includes(renderer))
  throw new Error("renderer must be voxl or novnc");
const viewer = await browser();
let suspended;
function resume() {
  if (!suspended) return;
  const pid = suspended;
  suspended = undefined;
  try {
    process.kill(pid, "SIGCONT");
  } catch (error) {
    if (error.code !== "ESRCH") console.error(error);
  }
}
let interrupted = false;
async function interrupt(status) {
  if (interrupted) return;
  interrupted = true;
  resume();
  try {
    await viewer.stop();
  } finally {
    process.exit(status);
  }
}
process.once("SIGINT", () => void interrupt(130));
process.once("SIGTERM", () => void interrupt(143));
async function cpuTicks() {
  // Approximate server-side box CPU by excluding this instrument and every
  // process carrying the headless viewer's private profile. The source apps,
  // desktop, agent and prototype proxy remain included.
  const profile = viewer.child.spawnargs
    .find((arg) => arg.startsWith("--user-data-dir="))
    .slice("--user-data-dir=".length);
  let server = 0,
    box = 0;
  for (const name of await readdir("/proc")) {
    if (!/^\d+$/.test(name)) continue;
    try {
      const stat = await readFile(`/proc/${name}/stat`, "utf8");
      const command = stat.slice(stat.indexOf("(") + 1, stat.lastIndexOf(")"));
      const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
      const ticks = Number(fields[11]) + Number(fields[12]);
      const cmd = await readFile(`/proc/${name}/cmdline`, "utf8");
      if (Number(name) !== process.pid && !cmd.includes(profile)) box += ticks;
      if (
        ["Xorg", "voxl-native", "x11vnc"].includes(command) ||
        (command === "ld-linux-x86-64" && cmd.includes("voxl-native")) ||
        (command === "python3" && cmd.includes("discobox-vnc-websockify"))
      )
        server += ticks;
    } catch {
      /* process exited */
    }
  }
  return { server, box };
}

try {
  if (renderer === "novnc" && process.env.SPIKE_VOXL_PID) {
    const pid = Number(process.env.SPIKE_VOXL_PID);
    if (!Number.isInteger(pid) || pid <= 1)
      throw new Error("invalid SPIKE_VOXL_PID");
    const cmd = await readFile(`/proc/${pid}/cmdline`, "utf8");
    if (!cmd.includes("voxl-native") || !cmd.includes("--port=6901"))
      throw new Error("SPIKE_VOXL_PID is not the spike streamer");
    process.kill(pid, "SIGSTOP");
    suspended = pid;
  }
  await viewer.send("Emulation.setDeviceMetricsOverride", {
    width: 1280,
    height: 800,
    deviceScaleFactor: 1,
    mobile: false,
  });
  await viewer.send("Page.navigate", { url });
  await waitConnected(viewer);
  await delay(3000);
  await viewer.evaluate(`window.__spikeSample = (() => {
    const c=document.createElement('canvas');c.width=480;c.height=48;
    const ctx=c.getContext('2d',{willReadFrequently:true});
    const result={latency:[],intervals:[],unique:0,errors:0,last:-1,lastTime:0};
    const cw=24,ch=24;
    const isConsole=${workload === "console"};
    let running=true;
    function tick(){
      if(!running||${workload === "idle"})return;
      const doc=document.querySelector('#screen iframe')?.contentDocument||document;
      const el=doc.querySelector('video.stripe')||doc.querySelector('#screen canvas');
      if(el)try{
        if(isConsole)ctx.drawImage(el,0,0,el.videoWidth||el.width,el.videoHeight||el.height,0,0,480,48);else ctx.drawImage(el,0,0,cw*20,ch*2,0,0,480,48);
        const d=ctx.getImageData(0,0,480,48).data;let value=0n;
        for(let b=0;b<40;b++){const i=((Math.floor(b/20)*24+12)*480+(b%20)*24+12)*4;if(d[i]+d[i+1]+d[i+2]>382)value|=1n<<BigInt(b);}
        let frame=Number(value>>24n);const stamp=Number(value&0xffffffn);
        if(isConsole){frame=2166136261;for(let i=0;i<d.length;i+=4)frame=Math.imul(frame^d[i]^d[i+1]^d[i+2],16777619)>>>0;}
        if(frame!==result.last){
          const now=performance.now();let lat=(Date.now()&0xffffff)-stamp;if(lat<0)lat+=0x1000000;
          if(isConsole||(lat>=0&&lat<5000)){if(!isConsole)result.latency.push(lat);result.unique++;if(result.lastTime)result.intervals.push(now-result.lastTime);result.lastTime=now;}
          result.last=frame;
        }
      }catch{result.errors++;}
      requestAnimationFrame(tick);
    }
    requestAnimationFrame(tick);
    return {result,reset:()=>{result.latency=[];result.intervals=[];result.unique=0;result.errors=0;result.lastTime=0;},stop:()=>{running=false;return result;}};
  })()`);
  await fetch(new URL("/spike/metrics", url), { method: "POST" });
  const ticks = await cpuTicks();
  await viewer.evaluate("window.__spikeSample.reset()");
  const started = performance.now();
  if (workload === "console") {
    await viewer.send("Input.dispatchMouseEvent", {
      type: "mousePressed",
      x: 1200,
      y: 400,
      button: "left",
      clickCount: 1,
    });
    await viewer.send("Input.dispatchMouseEvent", {
      type: "mouseReleased",
      x: 1200,
      y: 400,
      button: "left",
      clickCount: 1,
    });
    for (let i = 0; i < duration * 10; i++) {
      const key = i % 2 ? "ArrowUp" : "ArrowDown";
      for (const type of ["keyDown", "keyUp"])
        await viewer.send("Input.dispatchKeyEvent", {
          type,
          key,
          code: key,
          windowsVirtualKeyCode: i % 2 ? 38 : 40,
        });
      await delay(Math.max(0, started + (i + 1) * 100 - performance.now()));
    }
  } else await delay(duration * 1000);
  const result = await viewer.evaluate("window.__spikeSample.stop()");
  const elapsed = (performance.now() - started) / 1000;
  const endTicks = await cpuTicks();
  const metrics = await (await fetch(new URL("/spike/metrics", url))).json();
  const q = (values, percentile) => {
    const sorted = [...values].sort((a, b) => a - b);
    return sorted.length
      ? Number(
          sorted[
            Math.min(sorted.length - 1, Math.floor(sorted.length * percentile))
          ].toFixed(2),
        )
      : null;
  };
  const output = {
    renderer: new URL(url).searchParams.get("renderer") || "novnc",
    workload,
    seconds: elapsed,
    frames: result.unique,
    fps: result.unique / elapsed,
    samples: result.latency.length,
    errors: result.errors,
    drawToBrowserMs: {
      p50: q(result.latency, 0.5),
      p95: q(result.latency, 0.95),
      p99: q(result.latency, 0.99),
    },
    frameIntervalMs: {
      p50: q(result.intervals, 0.5),
      p95: q(result.intervals, 0.95),
      max: q(result.intervals, 1),
    },
    downstreamMbps: (metrics.downstreamBytes * 8) / elapsed / 1e6,
    // Linux USER_HZ = 100 on this x86_64 test host. See report's environment.
    streamerAndXorgCores: (endTicks.server - ticks.server) / 100 / elapsed,
    boxCoresExcludingViewerAndInstrument:
      (endTicks.box - ticks.box) / 100 / elapsed,
    voxlSuspended: Boolean(suspended),
    metrics,
    geometry: await viewer.evaluate(
      "document.getElementById('chip-geometry').textContent",
    ),
  };
  if (!output.frames && workload !== "idle")
    throw new Error(
      "no observed frames; check the source workload and marker geometry",
    );
  if (process.env.SHOT)
    await writeFile(
      process.env.SHOT,
      Buffer.from((await viewer.send("Page.captureScreenshot")).data, "base64"),
    );
  console.log(JSON.stringify(output));
} finally {
  resume();
  await viewer.stop();
}
