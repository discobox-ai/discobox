# Voxl desktop spike — #583

This is a disposable experiment, **not a production desktop replacement and not
for merge**. It changes no shipped desktop code, images, service units or defaults.
See [report.md](report.md) for the observations and recommendation, and
[results.jsonl](results.jsonl) for the raw measurements.

## Run the prototype

Use a **Linux x86_64 desktop sandbox**, with this branch checked out and its
Node, Go and Nix provisioning complete. Open the usual Desktop once first, so
Xorg and the session are ready, then close that viewer for measurements.

```sh
go tool task spike:voxl:build
go tool task spike:voxl
```

The build checks out voxl at `9d004076f73cb374bdb87ff96d2e23e44b9f47b9`, builds
with Cargo's lockfile and this checkout's pinned nixpkgs, then carries its native
runtime into `.tmp/voxl-runtime`. The runtime packer supports Linux x86_64 only;
its binary and libraries are local artifacts, never committed or published.

The runner prints its streamer PID. It starts voxl on loopback :6901 and the
prototype page on loopback :6902:

- `http://localhost:6902/?renderer=novnc`: the existing noVNC connection.
- `http://localhost:6902/?renderer=voxl`: voxl inside the **same branded page**.
- The usual desktop on :6900 continues to work unchanged.

Forward :6902 to localhost, or use an authenticated HTTPS port forward. For a
forward with its own hostname, set `SPIKE_ALLOWED_HOSTS=the.exact.hostname` when
starting the runner. The overlay rejects other hosts and cross-origin requests;
voxl itself is reachable only on loopback. WebCodecs requires HTTPS or localhost.
Ctrl-C stops both prototype processes. The original desktop remains available.

The runner disables voxl audio, microphone (its default), file transfers and its
own resize negotiation. One stripe is explicit. `/api/display` and `/api/scale`
remain the only geometry owners. The iframe uses upstream's normal video
presentation, and annotation capture copies a decoded video frame only on demand.

Cold-X startup exposed one transient MIT-SHM Attach refusal. Opening the usual
desktop first avoids that prototype startup race; if it occurs, restart the
runner once X is ready. Production service ordering/retry remains work to do.

## Repeat the comparison

Run only one viewer at a time. Keep the same X session, viewport, density,
source app, and server settings for both renderers. These targets run **inside
the test sandbox**. Use a bounded keepalive while driving it through Docker exec,
which does not count as an attached terminal:

```sh
discobox-power keepalive voxl-spike --for 45m
go tool task spike:voxl:source
```

The source target occupies one terminal. In another, run three measurements per
renderer, each with a three-second warmup and twenty-second measurement window:

```sh
go tool task spike:voxl:measure RENDERER=voxl WORKLOAD=browser SECONDS=20
SPIKE_VOXL_PID=PID_PRINTED_BY_RUNNER go tool task spike:voxl:measure RENDERER=novnc WORKLOAD=browser SECONDS=20
go tool task spike:voxl:features RENDERER=voxl
go tool task spike:voxl:features RENDERER=novnc
go tool task spike:voxl:scale RENDERER=voxl
go tool task spike:voxl:scale RENDERER=novnc
```

`SPIKE_VOXL_PID` makes a noVNC measurement suspend **this spike's** packed streamer
and resume it in cleanup. It checks the process command line first. Otherwise an
unconnected voxl process still observes XDamage and contributes to box CPU.

The feature target leaves the source browser idle. To resume the animation,
restart the source target. To measure idle CPU/bandwidth, leave it idle and use
`WORKLOAD=idle`. Marker and input latency use synchronized clocks on one host,
not physical devices. The feature script resets its source page before each run.

Stop the console target before scale checks, and let the session settle between
consecutive runs. The scale target changes density, **restarting the Xfce session**, launches a
fresh headed Chromium to inspect its DPR, narrows the viewing window, then
restores 1×. Use a disposable desktop: open session windows can close.

### The real console workload

The report also measures the actual Discobox TUI in xterm, alternating Up/Down
at 10 Hz. In the **parent dev checkout**, use the running `task dev` server;
never start another server. Give exactly the test sandbox's IP access to a
read-only bridge:

```sh
go tool task spike:voxl:console-bridge SANDBOX_IP=THE_TEST_SANDBOX_IP SANDBOX_ID=THE_TEST_SANDBOX_ID
```

The bridge binds `172.19.0.1:18080` (override `SPIKE_BRIDGE_ADDR` for another local
Docker bridge). Only that sandbox IP may GET/HEAD the exact overview metadata paths. The box
list is filtered to the supplied ID; harness account/file bodies are omitted, and
approval/rejection badges are fixture-empty. Other projects, subresources,
files, exec logs, screens, writes and WebSocket upgrades are refused. This is
not a general-purpose console connection. Stop the bridge when done.

Copy the parent's `build/discobox` into the test sandbox or build it there. In
the sandbox, with the browser source behind the console, run:

```sh
go tool task spike:voxl:console DEV_SERVER=http://172.19.0.1:18080 CLI_BINARY=/path/to/discobox
```

Dismiss the welcome screen with Enter, if present. Then use
`spike:voxl:measure WORKLOAD=console` for each renderer as above. Use
`spike:voxl:quality RENDERER=voxl` (and `novnc`) while that console is
stationary for the colored-logo crop. Its reported
rate is **changed sampled images**, including paintover, not uniquely identified
source frames. Console draw-to-browser latency is deliberately null; use the
separate causal input probe rather than interpreting a pixel hash as a clock.

Stop the source/console, runner and bridge when finished. Remove the keepalive
with `discobox-power keepalive rm voxl-spike`. Destroy a test sandbox by its
recorded ID, never by a name/filter. Keep measurement stdout outside the sandbox;
its `/tmp` is lost on restart.

## Measurement boundaries and shortcuts

- `measure.mjs` samples rendered video/canvas pixels at browser rAF. The browser
  workload embeds a 40-bit frame/time marker, following the format of voxl's
  MIT-licensed benchmark. Samples can miss a frame between rAFs; they do not
  measure decoder throughput or a physical monitor's scanout.
- Bandwidth counts WebSocket stream bytes at the common overlay, in both
  directions, excluding HTTP setup, TLS/tunnel overhead and TCP retransmission.
- CPU uses Linux `/proc` USER_HZ=100 counters: Xorg+streamer separately, and the
  box processes excluding the headless viewer's private profile and the Node
  instrument. It includes source apps, the desktop and prototype proxy. This is
  an estimate of server-side box CPU, not a cgroup measurement of a remote viewer.
- Image quality compares a stationary text region against the same X framebuffer
  read by voxl's screenshot endpoint. It is not a photo/video quality benchmark.
- The overlay patches two known function declarations in the served `app.js`;
  it fails if their anchors change. It deliberately exposes no production
  renderer interface. A real adoption should integrate the client with the
  page, rather than preserve this injection/iframe experiment.
- The helper scripts and fixed ports are for one experiment at a time. They
  require Chromium and use CDP; no npm packages are installed. Set `SHOT=/existing/directory/file.png` to request a viewer screenshot from
  measure/features; its parent directory must exist. Browser profiles are temporary. No remote file/microphone/audio feature is
  enabled by the experiment.
- Touch and keyboard events are synthetic. Physical layouts, IME, mobile
  browsers, WAN loss, ARM64 and non-Docker providers remain unverified.
