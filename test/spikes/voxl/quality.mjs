// Compare a stationary framebuffer region with the decoded viewer pixels.
import { browser, waitConnected } from "./browser.mjs";
import { setTimeout as delay } from "node:timers/promises";
const [renderer = "voxl", ...coordinates] = process.argv.slice(2);
if (!["voxl", "novnc"].includes(renderer))
  throw new Error("renderer must be voxl or novnc");
const region = coordinates.length
  ? coordinates.map(Number)
  : [20, 20, 300, 320];
if (
  region.length !== 4 ||
  region.some((x) => !Number.isInteger(x) || x < 0) ||
  !region[2] ||
  !region[3]
)
  throw new Error("usage: quality.mjs RENDERER [X Y WIDTH HEIGHT]");
const viewer = await browser();
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
  // RFB's connect event precedes its first framebuffer paint. In this
  // nonblack logo fixture, wait for actual pixels before the settling window.
  await viewer.evaluate(`new Promise((resolve,reject)=>{
    const deadline=Date.now()+10000,c=document.createElement('canvas');c.width=c.height=7;
    const ctx=c.getContext('2d',{willReadFrequently:true});
    function check(){
      const doc=document.querySelector('#screen iframe')?.contentDocument||document;
      const el=doc.querySelector('video.stripe')||doc.querySelector('#screen canvas');
      if(el)try{const [x,y,w,h]=${JSON.stringify(region)};ctx.drawImage(el,x,y,w,h,0,0,7,7);if(ctx.getImageData(0,0,7,7).data.some((v,i)=>i%4!==3&&v>0))return resolve(true);}catch{}
      if(Date.now()>deadline)return reject(new Error('no painted pixels in quality ROI'));setTimeout(check,100);
    }check();
  })`);
  await delay(3000);
  const quality = await viewer.evaluate(`(async()=>{
    const reference=await createImageBitmap(await(await fetch('/voxl/screenshot')).blob());
    const doc=document.querySelector('#screen iframe')?.contentDocument||document;
    const el=doc.querySelector('video.stripe')||doc.querySelector('#screen canvas');
    const w=el.videoWidth||el.width,h=el.videoHeight||el.height;
    const [x,y,rw,rh]=${JSON.stringify(region)};
    if(x+rw>w||y+rh>h||reference.width!==w||reference.height!==h)throw new Error('ROI/geometry mismatch');
    const c=document.createElement('canvas');c.width=w;c.height=h;const ctx=c.getContext('2d',{willReadFrequently:true});
    ctx.drawImage(reference,0,0);const a=ctx.getImageData(x,y,rw,rh).data;
    ctx.clearRect(0,0,w,h);ctx.drawImage(el,0,0);const b=ctx.getImageData(x,y,rw,rh).data;
    let abs=0,sq=0,n=0;for(let i=0;i<a.length;i++)if(i%4!==3){const d=a[i]-b[i];abs+=Math.abs(d);sq+=d*d;n++;}
    return {width:w,height:h,mae:abs/n,psnr:sq?10*Math.log10(255*255/(sq/n)):null};
  })()`);
  console.log(JSON.stringify({ renderer, region, quality }));
} finally {
  await viewer.stop();
}
