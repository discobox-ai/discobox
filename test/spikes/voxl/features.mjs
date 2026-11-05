// Functional and synthetic input tests against the same source browser as source.mjs.
import { randomUUID } from "node:crypto";
import { browser, waitConnected } from "./browser.mjs";
import { attach } from "./cdp.mjs";
import { readFile, writeFile } from "node:fs/promises";
import { setTimeout as delay } from "node:timers/promises";
const renderer = process.argv[2] || "voxl";
if (!["voxl", "novnc"].includes(renderer))
  throw new Error("renderer must be voxl or novnc");
const source = await attach(
  Number(await readFile("/tmp/voxl-spike-source-port", "utf8")),
);
const viewer = await browser();
const result = { renderer };
const noteComment = `voxl-spike-${randomUUID()}`;
const surface = `(()=>{const f=document.querySelector('#screen iframe');const d=f?.contentDocument||document;return d.querySelector('video.stripe')||d.querySelector('#screen canvas');})()`;
const key = async (name, code, vk, text = "", modifiers = 0) => {
  await viewer.send("Input.dispatchKeyEvent", {
    type: "keyDown",
    key: name,
    code,
    windowsVirtualKeyCode: vk,
    text,
    modifiers,
  });
  await viewer.send("Input.dispatchKeyEvent", {
    type: "keyUp",
    key: name,
    code,
    windowsVirtualKeyCode: vk,
    modifiers,
  });
};
try {
  await source.send("Page.bringToFront");
  await source.send("Page.reload");
  await delay(500);
  await source.evaluate(
    "window.mode='idle';window.scrollTo(0,0);window.inputSerial=0;window.inputEvents=[];window.addEventListener('keydown',e=>window.inputEvents.push({key:e.key,code:e.code,shift:e.shiftKey,ctrl:e.ctrlKey,alt:e.altKey}));document.querySelector('#input').textContent='INPUT';",
  );
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
  await delay(1200);
  // Click the desktop, not the viewer's chrome, to focus the client's keyboard path.
  await viewer.send("Input.dispatchMouseEvent", {
    type: "mousePressed",
    x: 600,
    y: 400,
    button: "left",
    clickCount: 1,
  });
  await viewer.send("Input.dispatchMouseEvent", {
    type: "mouseReleased",
    x: 600,
    y: 400,
    button: "left",
    clickCount: 1,
  });
  await delay(400);
  const sourceSerial = await source.evaluate("window.inputSerial");
  result.mouseClickEvents = sourceSerial;
  const latencies = [];
  for (let trial = 0; trial < 30; trial++) {
    await viewer.evaluate(`window.__inputResult=new Promise(resolve=>{
      const c=document.createElement('canvas');c.width=c.height=1;const ctx=c.getContext('2d',{willReadFrequently:true});
      const source=${surface};const w=source.videoWidth||source.width;
      const pick=()=>{ctx.drawImage(source,w-60,61,1,1,0,0,1,1);return Array.from(ctx.getImageData(0,0,1,1).data).join(',');};
      const before=pick(),start=performance.now();
      function tick(){if(pick()!==before)return resolve(performance.now()-start);if(performance.now()-start>2000)return resolve(null);requestAnimationFrame(tick);}requestAnimationFrame(tick);
    });'armed'`);
    await key("a", "KeyA", 65, "a");
    latencies.push(await viewer.evaluate("window.__inputResult"));
  }
  const valid = latencies.filter((x) => x !== null).sort((a, b) => a - b);
  result.inputMs = {
    sourceEvents: (await source.evaluate("window.inputSerial")) - sourceSerial,
    trials: 30,
    received: valid.length,
    p50: valid[Math.floor(valid.length * 0.5)] ?? null,
    p95: valid[Math.floor(valid.length * 0.95)] ?? null,
  };
  await key("é", "KeyE", 69, "é");
  await viewer.send("Input.dispatchKeyEvent", {
    type: "keyDown",
    key: "Shift",
    code: "ShiftLeft",
    windowsVirtualKeyCode: 16,
    modifiers: 8,
  });
  await key("A", "KeyA", 65, "A", 8);
  await viewer.send("Input.dispatchKeyEvent", {
    type: "keyUp",
    key: "Shift",
    code: "ShiftLeft",
    windowsVirtualKeyCode: 16,
  });
  await viewer.send("Input.dispatchKeyEvent", {
    type: "keyDown",
    key: "Control",
    code: "ControlLeft",
    windowsVirtualKeyCode: 17,
    modifiers: 2,
  });
  await key("a", "KeyA", 65, "", 2);
  await viewer.send("Input.dispatchKeyEvent", {
    type: "keyUp",
    key: "Control",
    code: "ControlLeft",
    windowsVirtualKeyCode: 17,
  });
  await delay(200);
  result.keyboard = await source.evaluate("window.inputEvents.slice(-12)");
  const serialBefore = await source.evaluate("window.inputSerial");
  await viewer.send("Emulation.setTouchEmulationEnabled", {
    enabled: true,
    maxTouchPoints: 5,
  });
  await viewer.send("Input.dispatchTouchEvent", {
    type: "touchStart",
    touchPoints: [{ x: 500, y: 350 }],
  });
  await delay(80);
  await viewer.send("Input.dispatchTouchEvent", {
    type: "touchEnd",
    touchPoints: [],
  });
  await delay(250);
  result.touchTapEvents =
    (await source.evaluate("window.inputSerial")) - serialBefore;
  await source.evaluate("window.scrollTo(0,0)");
  await viewer.send("Input.dispatchTouchEvent", {
    type: "touchStart",
    touchPoints: [
      { x: 500, y: 450 },
      { x: 550, y: 450 },
    ],
  });
  await viewer.send("Input.dispatchTouchEvent", {
    type: "touchMove",
    touchPoints: [
      { x: 500, y: 150 },
      { x: 550, y: 150 },
    ],
  });
  await viewer.send("Input.dispatchTouchEvent", {
    type: "touchEnd",
    touchPoints: [],
  });
  await delay(300);
  result.twoFingerScrollPx = await source.evaluate("window.scrollY");
  await source.evaluate("window.scrollTo(0,0)");
  await viewer.send("Emulation.setTouchEmulationEnabled", { enabled: false });
  const scrollBefore = await source.evaluate("window.scrollY");
  await viewer.send("Input.dispatchMouseEvent", {
    type: "mouseWheel",
    x: 600,
    y: 400,
    deltaY: 360,
    deltaX: 0,
  });
  await delay(300);
  result.wheelScrollPx =
    (await source.evaluate("window.scrollY")) - scrollBefore;
  await source.evaluate(
    "document.documentElement.style.overflow='hidden';document.body.style.overflow='hidden';window.addEventListener('wheel',e=>e.preventDefault(),{passive:false});window.scrollTo(0,0)",
  );
  await delay(600);
  result.quality = await viewer.evaluate(`(async()=>{
    const reference=await createImageBitmap(await(await fetch('/voxl/screenshot')).blob());
    const el=${surface};const w=el.videoWidth||el.width,h=el.videoHeight||el.height;
    const c=document.createElement('canvas');c.width=w;c.height=h;const ctx=c.getContext('2d',{willReadFrequently:true});
    ctx.drawImage(reference,0,0);const a=ctx.getImageData(0,0,w,h).data;
    ctx.clearRect(0,0,w,h);ctx.drawImage(el,0,0);const b=ctx.getImageData(0,0,w,h).data;
    let abs=0,sq=0,n=0;for(let y=100;y<Math.min(600,h);y++)for(let x=20;x<Math.min(1100,w);x++)for(let k=0;k<3;k++){const diff=a[(y*w+x)*4+k]-b[(y*w+x)*4+k];abs+=Math.abs(diff);sq+=diff*diff;n++;}
    return {width:w,height:h,mae:abs/n,psnr:sq?10*Math.log10(255*255/(sq/n)):null};
  })()`);
  for (const [origin, cdp] of [
    ["http://127.0.0.1:6902", viewer],
    ["http://127.0.0.1:7070", source],
  ]) {
    await cdp.send("Browser.grantPermissions", {
      origin,
      permissions: ["clipboardReadWrite", "clipboardSanitizedWrite"],
    });
  }
  const clipboard = {};
  try {
    await viewer.evaluate("navigator.clipboard.writeText('local-583')");
    await key("v", "KeyV", 86, "", 2);
    await delay(300);
    clipboard.localToRemote = await source.evaluate(
      "navigator.clipboard.readText()",
    );
    await source.evaluate("navigator.clipboard.writeText('remote-583')");
    await delay(400);
    await viewer.send("Input.dispatchMouseEvent", {
      type: "mouseMoved",
      x: 601,
      y: 401,
    });
    clipboard.remoteToLocal = await viewer.evaluate(
      "navigator.clipboard.readText()",
    );
  } catch (error) {
    clipboard.error = error.message;
  }
  clipboard.localToRemotePass = clipboard.localToRemote === "local-583";
  clipboard.remoteToLocalPass = clipboard.remoteToLocal === "remote-583";
  result.clipboard = clipboard;
  await viewer.evaluate(`document.getElementById('btn-annotate').click()`);
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
  result.annotation = await viewer.evaluate(`new Promise((resolve,reject)=>{
    const end=Date.now()+5000;
    async function check(){
      const data=await(await fetch('/api/feedback')).json();const note=data.items.find(x=>x.comment===${JSON.stringify(noteComment)});
      if(note){const dimensions=[];for(const shot of [note.shot,note.screen]){const image=await createImageBitmap(await(await fetch('/'+shot)).blob());dimensions.push([image.width,image.height]);}const cleanup=(await fetch('/api/feedback/'+note.id,{method:'DELETE'})).status;resolve({region:note.region,dimensions,cleanup});}
      else if(Date.now()>end)reject(new Error(document.body.innerText));else setTimeout(check,100);
    }check();
  })`);
  if (process.env.SHOT)
    await writeFile(
      process.env.SHOT,
      Buffer.from((await viewer.send("Page.captureScreenshot")).data, "base64"),
    );

  console.log(JSON.stringify(result));
} finally {
  source.close();
  await viewer.stop();
}
