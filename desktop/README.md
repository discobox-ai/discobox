# desktop: `discobox desktop`

The discobox console as a native app, in Rust on
[Rune](https://github.com/anomalyco/rune). Rune is pinned to a `dev` revision
in `Cargo.toml`.

`discobox desktop` runs this app as a child process with `--bridge`, and serves
it the console core over stdin and stdout
([`cli/internal/console/bridge`](../cli/internal/console/bridge/DESIGN.md)).
The app holds no credentials and talks to no server.

Its design is in [`DESIGN.md`](DESIGN.md), and how a person uses it in the
Console guide's "The desktop app"
([`docs/public/console.md`](../docs/public/console.md#the-desktop-app)). What it lacks against
the TUI, and what the TUI lacks against it, is in the parity ledger
([`docs/console/parity.md`](../docs/console/parity.md)). The work is tracked
in epic #786.

## Build, run, test

```sh
go tool task build:desktop          # build/discobox-desktop, beside build/discobox
build/discobox desktop              # against the configured server
go tool task desktop:spec           # refresh src/spec.json, which the mock runs with (generate does too)
go tool task desktop:test           # unit, parity and contract tests
go tool task desktop:snap           # every scene headless in both themes, to build/desktop/<scene>-<theme>.png
go tool task desktop:bench          # terminal throughput and frame time, headless (WORKLOADS=text,tui ARGS=--bench-scale 2 CPUS=4)
cd desktop && cargo run --release  # the app on the mock, without a CLI
```

- **Debugging:** `DISCOBOX_DESKTOP_DEBUG=1` logs every bridge message each way
  to stderr. `RUNE_GPU_LOG=1` says which adapter the window draws on.
- **Measuring:** `DISCOBOX_DESKTOP_PERF=1 build/discobox desktop` prints the
  window's frame times and terminal throughput when the window is closed or
  quit (not on a Ctrl-C), against real boxes; `desktop:bench` measures the
  same headless, and `CPUS=4` holds it to four CPUs, as a small GPU-less
  machine.
- **Headless:** `--snap ws.png --scene workspace` renders a frame with no
  window; `--theme light` (or `dark`) stands in for the system's appearance.
- **Toolchain:** `nix develop .#desktop` is the repository's shell plus rustup
  and, on Linux, the libraries the app builds and runs with. Without a GPU,
  render on Mesa's software driver it carries:
  `VK_ICD_FILENAMES="$DISCOBOX_LAVAPIPE_ICD"`. Outside Nix, Linux builds need
  `libxkbcommon-dev` and `libwayland-dev`, and `mesa-vulkan-drivers` with
  `VK_ICD_FILENAMES=/usr/share/vulkan/icd.d/lvp_icd.json` for no GPU.
- **CI:** the `desktop` job, on Linux only, runs `go tool task ci:desktop` in
  that shell: `desktop:test`, then `desktop:snap`, every scene rendered
  headless in both themes on the software driver and kept as the `desktop-scenes` artifact,
  then `desktop:bench --bench-check` on that driver.
  macOS and Windows do not build the app, or run the bridge's contract
  suite, in CI yet. The Go side, its parity scenarios included, runs in
  every job.
- **Parity:** what one console lacks is listed, with its reason, in
  [`docs/console/parity.md`](../docs/console/parity.md).
