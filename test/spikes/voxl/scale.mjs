import { randomUUID } from "node:crypto";
import { browser, waitConnected } from "./browser.mjs";
import { readFile } from "node:fs/promises";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { setTimeout as delay } from "node:timers/promises";
const exec = promisify(execFile);
async function nativeScale(expectedScale) {
  const deadline = Date.now() + 10000;
  while (Date.now() < deadline) {
    try {
      const pid = (await exec("pgrep", ["-x", "xfce4-panel"])).stdout
        .trim()
        .split("\n")[0];
      const env = await readFile(`/proc/${pid}/environ`, "utf8");
      const panelScale = Number(
        env.match(/(?:^|\0)GDK_SCALE=(\d+)(?:\0|$)/)?.[1],
      );
      const { stdout } = await exec("xwininfo", ["-root", "-tree"]);
      const panels = stdout
        .split("\n")
        .filter((line) => line.includes('"xfce4-panel"'))
        .map((line) => line.match(/ (\d+)x(\d+)[+-]/))
        .filter(Boolean)
        .map((match) => ({ width: Number(match[1]), height: Number(match[2]) }))
        .filter((rect) => rect.width > 100);
      if (panelScale !== expectedScale || !panels.length) {
        await delay(100);
        continue;
      }
      const dpi = (
        await exec("xfconf-query", ["-c", "xsettings", "-p", "/Xft/DPI"])
      ).stdout.trim();
      // Read only this numeric line from the generated JSONC; do not dump a
      // user's other launch arguments, or pretend JSON.parse supports JSONC.
      const argv = await readFile(
        `${process.env.HOME}/.vscode/argv.json`,
        "utf8",
      );
      const vscodeScale =
        Number(
          argv.match(
            /^[\t ]*"force-device-scale-factor"[\t ]*:[\t ]*([1-3])[\t ]*,?[\t ]*$/m,
          )?.[1],
        ) || null;
      return { panels, panelScale, xftDPI: dpi, vscodeScale };
    } catch (error) {
      if (Date.now() + 100 >= deadline) throw error;
      await delay(100);
    }
  }
  throw new Error(`Xfce panel did not settle at scale ${expectedScale}`);
}

const renderer = process.argv[2] || "voxl";
if (!["voxl", "novnc"].includes(renderer))
  throw new Error("renderer must be voxl or novnc");
const viewer = await browser();
let fresh;
async function geometry(scale, differentWidth) {
  return viewer.evaluate(`new Promise((resolve,reject)=>{
    const end=Date.now()+20000;
    async function check(){
      const data=await(await fetch('/api/session')).json();const g=data.geometry;
      const frame=document.querySelector('#screen iframe');const doc=frame?.contentDocument||document;
      const el=doc.querySelector('video.stripe')||doc.querySelector('#screen canvas');
      const width=el?.videoWidth||el?.width;
      if(g?.scale===${scale}&&width===g.width&&g.width!==${differentWidth || 0})return resolve({...g,decodedWidth:width,cssWidth:document.getElementById('screen-wrap').clientWidth});
      if(Date.now()>end)return reject(new Error(JSON.stringify({data,width})));setTimeout(check,100);
    }check();
  })`);
}
try {
  await viewer.send("Emulation.setDeviceMetricsOverride", {
    width: 1280,
    height: 800,
    deviceScaleFactor: 1,
    mobile: false,
  });
  await viewer.send("Page.navigate", {
    url: `http://127.0.0.1:6902/?renderer=${renderer}`,
  });
  await waitConnected(viewer);
  const standard = { ...(await geometry(1)), native: await nativeScale(1) };
  await viewer.evaluate(
    "document.getElementById('scale').value='2';document.getElementById('scale').dispatchEvent(new Event('change'))",
  );
  const hidpi = {
    ...(await geometry(2, standard.width)),
    native: await nativeScale(2),
  };
  fresh = await browser({
    headless: false,
    url: "http://127.0.0.1:7070/",
    args: ["--kiosk"],
  });
  const freshBrowserDPR = await fresh.evaluate("window.devicePixelRatio");
  const noteComment = `voxl-hidpi-${randomUUID()}`;
  await viewer.evaluate("document.getElementById('btn-annotate').click()");
  await viewer.send("Input.dispatchMouseEvent", {
    type: "mousePressed",
    x: 200,
    y: 200,
    button: "left",
    clickCount: 1,
  });
  await viewer.send("Input.dispatchMouseEvent", {
    type: "mouseMoved",
    x: 350,
    y: 300,
    button: "left",
    buttons: 1,
  });
  await viewer.send("Input.dispatchMouseEvent", {
    type: "mouseReleased",
    x: 350,
    y: 300,
    button: "left",
    clickCount: 1,
  });
  await viewer.evaluate(
    `document.getElementById('composer-text').value=${JSON.stringify(noteComment)};document.getElementById('composer').requestSubmit()`,
  );
  const annotation = await viewer.evaluate(`new Promise((resolve,reject)=>{
    const end=Date.now()+5000;
    async function check(){
      const data=await(await fetch('/api/feedback')).json();const note=data.items.find(x=>x.comment===${JSON.stringify(noteComment)});
      if(note){const dimensions=[];try{for(const shot of [note.shot,note.screen]){const image=await createImageBitmap(await(await fetch('/'+shot)).blob());dimensions.push([image.width,image.height]);}}finally{await fetch('/api/feedback/'+note.id,{method:'DELETE'});}resolve({region:note.region,dimensions});}
      else if(Date.now()>end)reject(new Error(document.body.innerText));else setTimeout(check,100);
    }check();
  })`);
  const scaleEnv = await readFile(
    `${process.env.HOME}/.discobox/desktop/scale.env`,
    "utf8",
  );
  await viewer.send("Emulation.setDeviceMetricsOverride", {
    width: 1000,
    height: 700,
    deviceScaleFactor: 2,
    mobile: false,
  });
  const resized = await geometry(2, hidpi.width);
  console.log(
    JSON.stringify({
      renderer,
      standard,
      hidpi,
      resized,
      freshBrowserDPR,
      annotation,
      scaleEnv,
    }),
  );
} finally {
  try {
    await viewer.evaluate(
      "document.getElementById('scale').value='1';document.getElementById('scale').dispatchEvent(new Event('change'))",
    );
    await geometry(1);
  } catch (error) {
    console.error(`restore scale: ${error.message}`);
  }
  const cleanup = await Promise.allSettled([
    ...(fresh ? [fresh.stop()] : []),
    viewer.stop(),
  ]);
  for (const entry of cleanup)
    if (entry.status === "rejected") {
      console.error(entry.reason);
      process.exitCode = 1;
    }
}
