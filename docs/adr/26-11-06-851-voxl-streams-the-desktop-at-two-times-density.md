# 26-11-06-851 — Voxl streams the desktop at two times density

- **Status**: Proposed
- **Date**: 2026-11-06
- **Issue**: [#698](https://github.com/discobox-ai/discobox/issues/698)
- **Relates to**: [26-09-05-409](26-09-05-409-an-image-declares-services-in-the-format-a-repository-does.md)
  (declared desktop port), [26-09-25-146](26-09-25-146-sandbox-terminals-share-the-desktops-socket-activated-session-bus.md)
  (shared session bus), and [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §4 (optional provisioned features). Their decisions remain unchanged.

## Context

The owner chose voxl as the primary desktop transport on
[PR #674](https://github.com/discobox-ai/discobox/pull/674#issuecomment-6669371150),
with 2× default scaling and clipboard support. The
[spike report](https://github.com/discobox-ai/discobox/issues/583#issuecomment-6651227515)
and unmerged prototype at `f70022870f8c61fe75ad9325074c5255ac04d7b7` are evidence,
not production code. Its earlier opt-in recommendation predates that choice.

At base `31d6deb4d1226f6efd58919d2a33f00fd422dbf7`,
`sandbox-agent/desktop/server.go` serves noVNC and proxies `/websockify`;
`features/desktop/provision.yaml` provisions noVNC, x11vnc and websockify.
`desktop/web/app.js` owns resizing, density and annotation interaction;
`desktop/web/capture.js` reads the displayed canvas. There is no desktop
clipboard bridge; the existing browser clipboard call copies feedback text.

The spike's pinned voxl revision is
`9d004076f73cb374bdb87ff96d2e23e44b9f47b9` (0.3.3): XDamage/MIT-SHM capture
of the existing Xorg dummy screen, CPU x264 H.264, browser WebCodecs decoding,
XTEST input, and one WebSocket carrying video and control. Its Linux amd64
Chromium/Docker experiment measured about 60 versus 14 observed browser frames
per second, with lower bandwidth, and successful permitted text clipboard in
both directions. Colored terminal edges were less faithful. These are results
of that setup, not browser coverage or capacity guarantees. Cold capture and
resize exposed readiness/rebuild failures that production must handle.

## Decision

### 1. Keep Discobox's desktop; replace its stream

Voxl is the only renderer inside the existing branded page. Keep Xorg dummy,
Xfce, the shared session bus, the sandbox user's identity, and the existing
scale, display and feedback APIs. Discobox owns geometry; disable voxl's
independent resize negotiation. Audio, microphone and file transfer are disabled
and outside this decision.

The page, matched voxl client/worker assets, stream and APIs share the existing
forwarded origin on port 6900, including beneath a forwarding URL prefix. The
voxl backend is internal and loopback-only; it introduces no separately
forwarded port, public listener, credential or direct browser-to-pool connection.
Enforce the desktop's access boundary on both HTTP and WebSocket upgrades,
including Host/Origin checks and the forwarding layer's authorization. Do not
assume a loopback backend makes a browser-origin request trustworthy.

### 2. Remove VNC completely

The owner decided on 2026-11-09 to remove noVNC and the VNC server, without an
automatic or manual fallback. The client-facing contract is the desktop webpage
and its forwarded origin, not a directly exposed VNC endpoint. Keep that web
contract while removing the noVNC assets, x11vnc server, websockify bridge,
`/novnc/` and `/websockify` routes, VNC resize helper and their units, sockets,
configuration and package dependencies. Do not retain port 6080's legacy resize
contract as a compatibility shim.

An in-place provisioning upgrade must stop and disable the retired sockets and
services and remove their managed files so already-provisioned boxes cannot
continue activating VNC. Preserve the person's home, scale and feedback state;
package removal must respect other consumers of shared dependencies.

Capability detection must test the actual configured decoder/worker path,
including secure-context requirements; browser brand or the presence of
`VideoDecoder` alone is insufficient. An unsupported client gets a clear
explanation and guidance to use a supported browser/context. Stream failures
remain visible, with bounded retries and an explicit reconnect action. Neither
case starts VNC or offers a renderer selector.

### 3. Two times is the default density, independent of browser DPR

New desktops start at 2×. An explicit manual choice takes precedence, then the
sandbox's remembered scale, then 2×. Keep existing remembered values, including
1× written by an older image; do not reinterpret or overwrite durable state.
Browser-detected DPR does not override the default or a remembered/manual value.
The current viewer's automatic DPR report can lower 2× to 1× and must change.
The `auto` API shape can remain, but an automatic suggestion must preserve that
precedence, including after a viewer restart.

Size continues to follow the available CSS viewport. The server multiplies by
the integer density and applies its existing bounds, 64-pixel mode rounding
and bounded mode eviction; the viewer uses returned geometry. Thus a nominal
1280×720 CSS region at 2× requests approximately 2560×1440 framebuffer pixels,
subject to those bounds and rounding. This is neither a fixed desktop resolution
nor a second browser zoom. Do not promise physical pixel parity on a DPR 1 screen.

Retain the single density chain: framebuffer, Xft DPI at 96×scale, toolkit
environment, cursor, decorations, Chromium and VS Code. Manual changes retain
the current session restart implications and surface apply/restart failures.
Reconnects never change density or restart the X session.
The existing shared density restart defect is tracked by #675; adopting a
renderer is not evidence that it is solved.

### 4. Bidirectional plain-text clipboard, under the person's control

Provide local-to-desktop paste and desktop-to-local copy. Both operate through
voxl's clipboard protocol and X clipboard selection; use the
CLIPBOARD selection, without silently replacing PRIMARY. Do not implement paste
as synthetic keystrokes. Rich content, images, files and clipboard history are
outside scope.

A deliberate paste action may read the local clipboard and send that text to
the sandbox. A deliberate copy action may write remote clipboard text to the
local clipboard. Do not poll/read the host clipboard on focus, connect or
reconnect, and do not automatically overwrite it upon remote selection changes.
Disable conflicting upstream automatic synchronization. Request browser access
only in the relevant user gesture; a denial, missing secure context or missing
clipboard API must not break video or input. Offer a text panel to paste local
text manually or select/copy remote text with the browser's ordinary commands.
Show failures and allow retry without repeated permission prompts. Bound message
sizes and avoid logging or persisting clipboard contents. Clear transient
clipboard buffers when the viewer disconnects.

### 5. Package a matched pair and keep the desktop cold

Build voxl from an immutable source revision with locked dependencies for Linux
amd64 and arm64. The spike revision is the initial integration baseline; any
replacement must be pinned and validated with its matching browser assets.
Deliver the binary, worker/client and native runtime dependencies through the
platform bundle's desktop feature, with provisioning readiness covering the
whole set. Do not download an unpinned executable in the browser or at startup.
Keep the existing feature-off and provisioning/snapshot model.

The spike reports x264, libyuv, Opus and PulseAudio dependencies even with audio
off. Determine the actual native dependency closure for each architecture and
target distribution; declare/install or bundle it deliberately. Source MIT
licensing does not settle distribution of an x264-linked executable. Before
shipping binaries, verify applicable licenses and provide required notices,
corresponding source/build material and any required distribution terms. This
ADR does not assert legal clearance or choose a different encoder by default.

The viewer's socket activation and readiness notification must not wait for X
or voxl. TCP probes, `GET /` and `GET /api/session` leave X and capture cold.
A real page requesting pixels, or an application connecting to `DISPLAY=:0`,
may activate X. Start capture only on a stream request after X/geometry readiness,
as the sandbox user in the same IPC namespace. Serialize startup/recovery;
handle cold MIT-SHM refusal, X/session restarts and geometry churn with bounded
retry and capture reconstruction. Restarting a streamer must not kill apps.
Keep the 0600 session bus and the declared `ai.discobox.desktop` service/protocol;
port discovery must never probe an internal capture listener into life.

### 6. Preserve what the viewer can do

Keep reconnect, keyboard/modifiers, mouse, wheel/touch, fullscreen, density and
live resize. Release held input on disconnect, focus loss.
Calibrate wheel/touch against the existing experience; the spike's synthetic
input did not validate physical keyboards, non-US layouts or IME.

Annotations capture the decoded frame actually presented to the person, at its
native dimensions, with the corresponding geometry. Freeze/copy one frame for
both crop and full-screen context so a resize or another arriving frame cannot
mix them. Preserve the existing feedback record and coordinate contract; do not
substitute a newer server screenshot. If capture fails, retain the note and
explain the missing image, as the current viewer does.

## Alternatives rejected

- **Retain noVNC as an automatic or manual fallback:** rejected by the owner.
  It would keep a second streaming stack and browser integration to maintain;
  preserving a directly exposed VNC protocol is not a client requirement.
- **Keep voxl opt-in:** conflicts with the owner's primary-renderer decision.
- **Replace Xorg/Xfce or introduce WebRTC:** the measured gain used the current
  X session and WebSocket forwarding; neither replacement is needed here.
- **Let both Discobox and voxl resize X:** competing geometry owners produce races and
  invalidate annotation coordinates and the toolkit density chain.
- **Use DPR as the initial density:** ordinary-screen browsers would silently
  lower the requested 2× default and can trigger a destructive session restart.
- **Always synchronize local clipboard:** focus/reconnect is not permission to
  disclose local text or replace the person's local clipboard.
- **Ship the spike overlay:** iframe/string injection and ad hoc native-library
  copying were experimental scaffolding, not maintainable product integration.

## Consequences and acceptance evidence

This adds a Rust/native build and protocol-coupled browser assets. H.264's
motion/bandwidth gains come with CPU cost and colored-edge loss; 2× also raises
pixel count compared with 1×. No broad browser, provider or ARM64 support claim
follows from the spike.

Delivery must demonstrate cold activation, first decoded frame, recovery after
streamer/X/session restart, resize and manual/remembered/default density across
page and service restarts. Verify copy/paste and denied/unavailable permissions,
real keyboard/modifier/layout/IME input, pointer/wheel/touch, and annotation
crops at 1× and 2×. Exercise the actual forwarded URL and foreign Host/Origin
rejection. Cover both Linux architectures and the supported Debian/Ubuntu
provisioning paths, and test Chromium, Firefox and Safari against the
capability policy; report mobile and WAN limitations explicitly. Unsupported
clients must follow §2 rather than hang. Verify that upgraded boxes have no
active VNC listeners or activation units and that retired web routes are gone. Required compatibility cannot be
claimed solely from API feature detection.

Implementation owns exact process/unit wiring, retry limits, clipboard payload
limits, encoder tuning and UI details. Those remain open engineering choices,
subject to the behavior above; they are not permissions to drop recovery,
clipboard or annotation support. Update the affected current-state DESIGN.md
files with the implementation, not in this proposal.
