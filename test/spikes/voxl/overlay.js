// Spike-only additions to the existing viewer, injected by proxy.mjs.
// No production bundle or desktop service is changed.
let spikeFrame;
let spikeMonitor;

function connectVoxl() {
  clearInterval(spikeMonitor);
  spikeFrame?.remove();
  state.connected = false;
  dom.curtain.hidden = false;
  dom.curtain.classList.remove("fading");
  setStatus("connecting", "Connecting to voxl…");
  const frame = document.createElement("iframe");
  frame.title = "Voxl desktop spike";
  frame.src = "/voxl/";
  frame.allow = "clipboard-read; clipboard-write; autoplay; fullscreen";
  frame.style.cssText = "border:0;width:100%;height:100%;display:block";
  dom.screen.replaceChildren(frame);
  spikeFrame = frame;
  spikeMonitor = setInterval(() => {
    const doc = frame.contentDocument;
    const source =
      doc?.querySelector("video.stripe") || doc?.querySelector("canvas#screen");
    const ready = doc?.querySelector("#status")?.style.display === "none";
    if (ready && source && (source.videoWidth || source.width)) {
      state.connected = true;
      setStatus("connected", "Connected · voxl spike");
      dom.curtain.hidden = true;
    } else if (state.connected) {
      state.connected = false;
      setStatus("connecting", "Voxl reconnecting…");
    }
  }, 100);
}

// Capture a decoded frame only when annotating. The normal video presentation
// path stays in use; there is no permanent copy of every frame into a canvas.
function voxlCanvasBox() {
  const frame = spikeFrame;
  const source =
    frame?.contentDocument?.querySelector("video.stripe") ||
    frame?.contentDocument?.querySelector("canvas#screen");
  if (!source) return null;
  const width = source.videoWidth || source.width;
  const height = source.videoHeight || source.height;
  if (!width || !height) return null;
  const rect = source.getBoundingClientRect();
  const outer = frame.getBoundingClientRect();
  const origin = dom.overlay.getBoundingClientRect();
  if (!rect.width || !rect.height) return null;
  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  canvas.getContext("2d").drawImage(source, 0, 0);
  return {
    canvas,
    left: outer.left + rect.left - origin.left,
    top: outer.top + rect.top - origin.top,
    width: rect.width,
    height: rect.height,
    scaleX: width / rect.width,
    scaleY: height / rect.height,
  };
}
