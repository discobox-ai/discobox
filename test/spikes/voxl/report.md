# Desktop renderer comparison — issue #583

Observed 2026-11-05. **Recommendation: adopt in part**, initially as an opt-in
renderer for motion-heavy desktops. Keep noVNC for colored-edge fidelity and
clients without the necessary browser features. Keep Discobox's page, scale,
resize and feedback ownership. This spike makes **no production/default change**;
the prototype PR is a draft, not for merge.

## What voxl is, and whether Xorg can feed it

At the tested commit,
[voxl](https://github.com/tembo/voxl/tree/9d004076f73cb374bdb87ff96d2e23e44b9f47b9)
is a Rust server with an embedded TypeScript browser client. Its Linux capture
uses XDamage and MIT-SHM, converts to I420 and encodes H.264 with CPU x264. One
WebSocket carries video, acknowledgements/rate control, input, cursor and
clipboard messages. The client decodes with WebCodecs in a worker, then presents
through video or an OffscreenCanvas fallback. Input is injected through XTEST.
Wayland is not supported by this revision.

**The existing Xorg dummy display works without replacing Xorg.** The actual log
was `capturing via MIT-SHM GetImage`. Its optional direct Xvfb framebuffer path
was unavailable; Xorg is not Xvfb started with `-shmem`. The existing server also
has no XTEST scroll axes, so voxl logs `scrolling with wheel clicks`. Neither
an X server patch nor a kernel change was used for these measurements.

The snapshot has 88 commits, beginning 2026-10-12, with the latest tested commit
on 2026-11-04. It is young and actively changing, rather than a mature protocol
contract. Release v0.3.3 was published November 4; its release asset is a source
crate, not a ready-to-install Linux executable. The source license is MIT.
[Upstream's README](https://github.com/tembo/voxl/blob/9d004076f73cb374bdb87ff96d2e23e44b9f47b9/README.md#faq)
explicitly warns that distributing its x264-linked binary entails GPL
requirements. Settle that distribution requirement before a product release.
This PR contains no compiled binary or native libraries.

## Experiment and interpretation

- Same Docker-pool sandbox and X session for both renderers, on a shared AMD
  Threadripper PRO 5975WX host: 64 logical CPUs, no sandbox CPU quota. This is a
  single-host loopback experiment, not a WAN/packet-loss test or physical display
  latency measurement.
- Chromium 156.0.8078.79, Debian 13; noVNC 1.6.0, x11vnc 0.9.17, websockify
  0.12.0, Xorg 21.1.16. CLI/dev server `fb46460b7460+dirty`; the dirty changes are
  this spike. Desktop source/feature trees are unchanged between the sandbox's
  starting `b302561` revision and `fb46460`.
- voxl 0.3.3 at `9d004076f73cb374bdb87ff96d2e23e44b9f47b9`, built with Rust
  1.96.1, esbuild 0.27.2, x264 0.164.x, and this checkout's pinned nixpkgs
  `241313f4e8e508cb9b13278c2b0fa25b9ca27163`. Default ultrafast/CRF 24,
  60 FPS ceiling, four encoder threads, quiet-screen paintover CRF 16. One
  stripe; audio, files and voxl's resize negotiation disabled.
- Same branded page on :6902, CSS viewport 1280×800, DPR 1, resulting desktop
  1280×704 at 96 DPI. Original noVNC flags/settings are retained. The overlay
  adds one local proxy to each route. Only one viewer is connected, and the
  unconnected voxl process is suspended during noVNC timing trials.
- Browser: the same deterministic scrolling text page, with a frame/time marker
  drawn at nominal 60 Hz. Console: the actual Discobox TUI in xterm, with
  alternating Up/Down at 10 Hz against an overview-metadata fixture backed by
  the local dev server. It exposes only the test box, strips harness file bodies
  and keeps notification badges empty (as in the measured fixture).
  Three 20-second trials per renderer/workload, each after a three-second warmup.
- Tables use medians of the three runs; FPS ranges show all three. Latency p95
  is the median of per-run p95s, not a pooled percentile. CPU is in **cores**
  (1.0 = one full core). Box CPU is a `/proc` estimate including source apps and
  proxy, excluding the headless viewer/profile and Node instrument. Idle has
  only one 20-second observation per renderer.
- Full methods and runnable targets are in [README.md](README.md). All final
  records are in [results.jsonl](results.jsonl). Exploratory runs used to correct
  timing/readiness probes are excluded from that dataset.

## Performance

| Item | Existing noVNC | voxl |
| --- | ---: | ---: |
| Browser observed unique frames/s | 13.89 (13.74–14.09) | 59.96 (59.62–59.97) |
| Browser draw → observed pixels, p50 / p95 | 75 / 107 ms | 17 / 18 ms |
| Browser frame interval, p95 | 83.5 ms | 17.4 ms |
| Browser downstream stream bandwidth | 17.73 Mbps | 0.634 Mbps |
| Browser Xorg + streamer CPU | 0.414 cores | 0.314 cores |
| Browser box CPU, viewer/instrument excluded | 0.659 cores | 0.562 cores |
| Console changed sampled images/s | 9.90 (9.85–10.00) | 19.95 (17.15–20.05) |
| Console changed-image interval, p95 | 116.9 ms | 85.8 ms |
| Console downstream stream bandwidth | 1.741 Mbps | 1.055 Mbps |
| Console Xorg + streamer CPU | 0.079 cores | 0.099 cores |
| Console box CPU, viewer/instrument excluded | 0.139 cores | 0.155 cores |
| Idle with console open: Xorg + streamer CPU | 0.0055 cores | 0.0005 cores |
| Idle with console open: downstream | 2.43 Kbps | 0.464 Kbps |

The browser workload is visibly smoother with voxl: roughly 4.3× the observed
unique frame rate and 28× less stream bandwidth. The mostly static TUI saves
about 39% bandwidth but costs about 0.02 additional streamer/Xorg cores.
Console image counts include partial updates and voxl paintover; **they are not
a claim of twice as many unique source UI frames**. Console source-draw latency
is deliberately null in the raw data. The separate causal key probe below is
the input-latency comparison.

WebSocket counts exclude asset loading, TLS/tunnel overhead and TCP
retransmission. Browser rAF sampling can miss a decoded frame between samples.
Other users share the host, and receiver CPU/physical monitor scanout are not
measured. Treat these as measurements of this setup, not capacity guarantees.

## Quality, input and integration

| Item | Existing noVNC | voxl |
| --- | --- | --- |
| Synthetic key → visible response, p50 / p95 | 115.4 / 132.3 ms; 30/30 source events and visible responses | 32.3 / 33.3 ms; 30/30 source events and visible responses |
| Stationary browser text ROI: RGB MAE / PSNR | 2.221 / 35.36 dB | 1.094 / 46.46 dB |
| Stationary colored console-logo ROI: RGB MAE / PSNR | 0.211 / 45.02 dB | 1.719 / 31.80 dB |
| ASCII, Shift+A, Ctrl+A | 30 ASCII events; both modifier combinations reached source | Same |
| Synthetic `é` | 0/1 correct; source reported NUL | Same; not a successful non-US-layout test |
| Local ↔ remote clipboard | 0/2 directions through the current page | 2/2 directions, with browser clipboard permission granted |
| Mouse click / synthetic touch tap | 1 / 1 source events | 1 / 1 source events |
| One 360-unit wheel event | 120 source scroll px | 240 source scroll px |
| One two-finger synthetic drag | 600 source scroll px | 240 source scroll px |
| 1× → 2×, decoded framebuffer / DPI | 1280×704 @96 → 2560×1472 @192 | Same |
| Narrow viewer at 2× | 1984×1280 decoded, 992 CSS px wide | Same |
| Native GTK panel, Xft, fresh Chromium, VS Code argv | Panel 30→60 px; GDK 1→2; Xft 96→192; Chromium DPR 2; argv scale 2 | Same |
| Annotation at 1× | 150×100 crop and 1280×704 full image, then delete 200 | Same |
| Annotation at 2× | 300×200 crop and 2560×1472 full image, native region (400,284,300,200) | Same |

MAE is in 0–255 RGB units; smaller is closer. Quality is a comparison with the
same X framebuffer, after waiting for painted pixels and a stationary region.
The console crop is (20,20,300,320); the text crop is x20–1100/y100–600.
The colored logo demonstrates a real tradeoff: voxl's lossy I420/H.264 path is
less faithful on strong colored edges, despite its quiet-screen refinement.
The text result does not imply all text or all noVNC encodings behave alike.
NoVNC quality settings were not retuned for this comparison.

The current Discobox page does not wire noVNC's clipboard API into a browser
clipboard bridge. The 0/2 result describes **this page**, not an inherent RFB
limitation. Wheel/touch distances are deliberately reported rather than called
equivalent: calibration differs, and stock Xorg does not expose voxl's smooth
scroll axes. Physical keyboard layouts, IME, Alt/Super/Command behavior,
real mobile gestures and Qt/VS Code rendering were not driven. VS Code's argv
and Qt's environment were inspected; VS Code itself was not launched.

## What would change, and what stays

| Area | Adoption work | Retained ownership |
| --- | --- | --- |
| Desktop page | Integrate the matched voxl client/worker, capability detection, reconnect and decoded-frame snapshot; keep a fallback. The iframe/string-injection overlay is experiment code. | Branded chrome, notes UI and feedback capture/store. Snapshot what the viewer saw, not a later server screenshot. |
| HTTP service | Route matched client assets and its WebSocket on the existing forwarded origin. Keep backend internal and preserve port-forward authorization. | One desktop URL/port; existing display, scale and feedback APIs. |
| Lifecycle | Start/restart voxl lazily with X, after readiness, as the sandbox user in the same IPC namespace. Handle X/session/geometry churn. | Xorg dummy, Xfce, session bus, toolkit/Chromium/VS Code density chain. |
| Feature/image/build | Build and pin a Linux binary per supported architecture. Carry x264, libyuv, Opus and PulseAudio runtime dependencies; audio-off still links audio libraries. Resolve distribution requirements. | Existing provider/base-image/provisioning model and kernel. No X server replacement is needed for the measured gain. |
| Input | Tune wheel/touch calibration; investigate non-US input and test real clients. Pixel-precise scrolling would need an X server patch or another input approach. | Existing X session and app input model. |
| Fallback | Keep noVNC/x11vnc/websockify while client coverage and colored-edge quality matter. Remove them only after a separate product decision. | Existing desktop continues to work during rollout. |

The local runtime pack is about 39 MiB including an unstripped binary. A real
release should use deliberate per-architecture packaging rather than this
loader/library-copy helper. The main new maintenance costs are a Rust/native
build/release line, matched browser assets/protocol, distribution compliance,
and browser/lifecycle/input verification. Upstream's release assets do not
currently remove that build work.

## Observations and remaining limits

- Cold X activation produced one MIT-SHM Attach refusal; a warm retry worked.
  Opening the normal desktop first is the prototype workaround. This is a
  lifecycle readiness requirement for any production integration.
- Live resize occasionally logged a transient MIT-SHM Match and rebuilt the
  capture pipeline. Both clients recovered to the measured geometry.
- One rapid density toggle through the unchanged scale API failed to restart
  Xfce: `xfce4-session-logout` hit the existing five-second command timeout,
  leaving the old panel scale. A settled retry, after closing the console,
  passed. This shared scale-control failure is not attributed to a renderer.
- Exploratory noVNC input probes occasionally missed a visible color response
  while the source received every key; the final run observed all 30. These
  probes do not certify input loss rates or worst-case latency.
- A connected noVNC socket is not yet a painted framebuffer. The quality probe
  now waits for pixels; the earlier premature capture is excluded.
- Only Chromium on Linux x86_64/Docker was driven. Firefox, Safari, mobile,
  ARM64, other providers, WAN loss, multi-viewer contention, multiple stripes,
  audio/microphone/files and physical end-to-end latency remain unverified.

Proceed to a product/design decision for an opt-in voxl transport, with the
quality and compatibility fallback retained. This experiment is sufficient to
show that the existing Xorg session can feed it and that the motion/bandwidth
benefit is material. It is not sufficient to remove noVNC or change the default.

Additional live probes rejected a foreign Host and foreign Origin on HTTP and a
foreign Origin on a WebSocket upgrade (403), while the local page returned 200.
Interrupting a noVNC timing run while its voxl process was suspended exited 130,
resumed the streamer and left its health endpoint responding `ok`.
The console fixture was narrowed during review: its metadata routes return 200,
while other projects, secret lists, file content, exec logs and screen proxies
return 403. Its box list contains only the supplied test ID, and harness file
bodies are omitted. The real console overview/navigation still rendered.
A default feature run also succeeded after removing the optional screenshot
scratch directory; viewer screenshot export now requires an explicit `SHOT`.
