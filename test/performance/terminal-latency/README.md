# Terminal latency harness

This diagnostic runs a deterministic byte request/response through a real
development stack. It is centered on the interactive path shared by `discobox run`
and `discobox admin terminal ... attach`:

1. The transport probe, `TestTerminalLatencyE2E` in
   `cli/internal/cli/terminal_latency_e2e_test.go`, drives the shared resumable
   exec connection directly and writes `transport-<profile>.json`. It records
   the input call, physical WebSocket write, host apply acknowledgement, and
   sequence-tagged PTY echo. It is opt-in: it skips unless
   `DISCOBOX_TERMINAL_LATENCY_E2E=1`, which the runner sets.
2. The direct probe, `main.go` in this directory, drives
   `discobox admin terminal --discobox-id <id> attach primary` through a real
   tmux PTY and writes `direct-<profile>.json`. It adds the CLI's local terminal
   and raw-mode path.

After `discobox run` creates the sandbox and resolves its primary terminal, it
calls the same `attachSandboxTerminal` implementation as the direct probe.
Consequently the direct result measures the attached, interactive hot path of
both commands. It intentionally excludes `discobox run` source upload,
provisioning, and initial sandbox startup.

Each transport and direct path runs against three fresh sandboxes by default:

- `quiet`: only the sequence-tagged replies.
- `spinner`: 128-byte terminal updates at 30 writes per second.
- `screen`: 4,800-byte full-screen-style updates at 30 writes per second
  (about 141 KiB/s before probe replies and framing).

`discobox tui` is an optional, quiet-only mode of the same driver, reported as
`tui-quiet.json`. It measures the embedded terminal, VT parsing, Bubble Tea
update loop, and rendering path, but it is not part of the default run because
it is not on the `discobox run` or direct attach path.

The image under `image/` is test-only. Its primary process, `probe.py`, enters
raw mode and turns `DBXPING:00000001` into `DBXPONG:00000001`, so every input
has an unambiguous matching output; it also generates the spinner or screen
load.

## Run

Start the normal development stack first:

```bash
go tool task dev
```

In another terminal (the runner needs `docker`, `go`, `jq`, `timeout`, and
`tmux`):

```bash
go tool task perf:terminal
```

The runner builds the current CLI and fixture image, registers and configures a
uniquely named harness, gives each path/profile pair a fresh disposable
sandbox, and deletes only those resources. Separate sandboxes prevent replay
or detach state from one probe contaminating the next. Reports land under
`.tmp/terminal-latency/<run-id>/`, beside `configure.json` and a
`sandbox-<mode>-<profile>.json` for each sandbox.

After the readiness marker is observed, each client drains the initial replay
for 250 ms before sampling. This keeps attach startup and old screen contents
out of the steady-state typing distribution; the duration is recorded in every
report and can be changed with `DISCOBOX_TERMINAL_LATENCY_SETTLE`.

Useful controls (defaults: 100 samples, a 20ms interval, a 5s per-sample
timeout, 1 vCPU, project `default`):

```bash
DISCOBOX_TERMINAL_LATENCY_SAMPLES=500 \
DISCOBOX_TERMINAL_LATENCY_INTERVAL=5ms \
DISCOBOX_TERMINAL_LATENCY_CPU_VCPUS=0.5 \
go tool task perf:terminal

# Isolate one layer while iterating.
DISCOBOX_TERMINAL_LATENCY_MODES=transport go tool task perf:terminal

# Run only quiet and spinner load, or opt into the Bubble Tea comparison.
DISCOBOX_TERMINAL_LATENCY_PROFILES=quiet,spinner \
DISCOBOX_TERMINAL_LATENCY_MODES=transport,direct,tui \
go tool task perf:terminal

# Keep the probe sandboxes and the harness for manual inspection after the run.
DISCOBOX_TERMINAL_LATENCY_KEEP=1 go tool task perf:terminal
```

`DISCOBOX_TERMINAL_LATENCY_SERVER`, `..._PROJECT`, `..._TIMEOUT`, and
`..._OUTPUT_DIR` are also supported. Unset, the run uses the endpoint `discobox`
dials on its own — the local socket `task dev` binds — so a server reached any
other way is named there and nowhere else. `DISCOBOX_TOKEN` is passed through when
set. The default loads can be changed with `..._SPINNER_HZ`,
`..._SPINNER_BYTES`, `..._SCREEN_HZ`, and `..._SCREEN_BYTES`.

## Interpret

- A slow `writeCallUs` or `physicalWriteUs` points at local queuing, flow
  control, or the client WebSocket write.
- A fast physical write with a slow `applyRoundTripUs` points beyond the client:
  control-plane relay, pool/sandbox agents, shim, or PTY input application.
- A fast apply acknowledgement with a slow `echoRoundTripUs` points at the
  output return path.
- A large gap from transport to direct implicates local PTY/CLI handling.
- A regression from quiet to spinner or screen implicates contention with
  downstream output. Input travels on the full-duplex uplink, but output,
  action acknowledgements, and probe replies ultimately share the downstream
  writer, so this comparison detects downstream head-of-line delay.
- A large gap from direct to the optional TUI result implicates VT parsing, the
  Bubble Tea loop, or rendering; that gap does not describe `discobox run`.

The tmux reports (direct and TUI) include host CPU/IO/memory pressure snapshots and the
sandbox's cgroup CPU quota, pressure, and `cpu.stat` delta. Both report types
also record observed output bytes per second, so a run can prove that the load
was flowing while latency was sampled. In particular, growth in `nr_throttled`
or `throttled_usec` alongside latency outliers is evidence of CPU QoS
throttling rather than byte-copy overhead.

The shared `execstream/resume` package exposes an opt-in action observer
(`resume.WithObserver`, which the transport probe uses) rather than creating an OpenTelemetry span for every keystroke. The observer is the
annotation point: the harness gets exact monotonic timestamps without an
exporter, while normal terminal traffic pays no clock or export cost. It can be
adapted to OTel events or histograms for a sustained profiling deployment
without changing the attach implementations.

These are diagnostic distributions, not pass/fail timing assertions. Host
load, debug builds, Docker storage, and scheduler state all affect the result.
Run multiple trials and compare like-for-like artifacts.
