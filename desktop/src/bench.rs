//! `--bench`: how fast a pane takes a program's output, and what each frame costs, headless.
//!
//! Each workload is fed to the focused pane of a workspace on the mock the way a stream's output
//! arrives (the inbox, then `poll`), a fixed slice a frame, and every frame is built and drawn
//! to an offscreen target on the GPU Rune would pick, waiting for the GPU to finish it: on a
//! software driver (lavapipe), drawing is CPU work too, and it counts. Each workload reports:
//!
//! - **emulator**: the escape-sequence parser and grid alone (`Screen::feed`), no drawing;
//! - **drawn**: the workload's bytes over the wall time of its frames, everything included;
//! - **frame**: each frame's time, from its output arriving to the GPU done drawing it.
//!
//! `--bench-check` exits 1 when a workload is past its floor (`FLOORS`), for CI, which runs it
//! on lavapipe with no GPU. The floors are a regression's, not a target's: well past what a
//! four-CPU machine drawing in software comes to, so only a change that makes the window several
//! times slower trips them.
//!
//! `go tool task desktop:bench` runs it. On a real box, `DISCOBOX_DESKTOP_PERF=1` has the window
//! report its own frames as it closes (`perf.rs`).

use std::time::Instant;

use rune::render::{Gpu, OFFSCREEN_FORMAT, Offscreen, Renderer};
use rune::{Env, Runtime};

use crate::backend::Event;
use crate::perf::{Histogram, mb_per_s};
use crate::term::Screen;
use crate::{State, on_key, poll, theme, view};

/// The window the bench draws, in points, as the app opens.
const SIZE: (f32, f32) = (1440.0, 900.0);
/// Output a frame takes, for a stream of output: what a busy pty read hands over at about
/// 60 frames a second.
const SLICE: usize = 64 * 1024;
/// How much output a streamed workload is.
const STREAMED: usize = 8 << 20;
/// Frames of a workload that is not streamed: a full-screen program's repaints, keys echoed.
const FRAMES: usize = 240;

/// One workload: its name, what it stands for, and whether it streams or is per-frame.
pub struct Workload {
    pub name: &'static str,
    pub about: &'static str,
    kind: Kind,
}

enum Kind {
    /// One stream of output, `SLICE` a frame.
    Stream(fn(usize) -> Vec<u8>),
    /// What a frame takes, for each of `FRAMES` frames, given the pane's size.
    Frame(fn(usize, usize, usize) -> Vec<u8>),
}

pub const WORKLOADS: &[Workload] = &[
    Workload { name: "text", about: "plain lines scrolling, a log or `cat` of a file", kind: Kind::Stream(text) },
    Workload { name: "color", about: "lines of 256-colour and bold runs, a build's or `ls --color`'s", kind: Kind::Stream(color) },
    Workload { name: "unicode", about: "wide characters and emoji among ASCII", kind: Kind::Stream(unicode) },
    Workload { name: "tui", about: "a full-screen program repainting every cell in truecolour each frame", kind: Kind::Frame(tui) },
    Workload { name: "typing", about: "a key echoed a frame at a shell prompt", kind: Kind::Frame(typing) },
];

/// The least a workload may come to under `--bench-check`: frames at the 95th percentile, and
/// the emulator's and the drawn rate. On lavapipe held to four CPUs (`LP_NUM_THREADS=4`, a CI
/// runner's), a streamed workload's frames were 30 to 55 ms at p95, the tui's 45, a key's 30;
/// the emulator ran at 55 to 160 MB/s and drawing took streamed output at 2 to 2.6 MB/s. The
/// floors sit about five times past those, so a loaded runner passes and a change that makes the
/// window several times slower does not.
struct Floor {
    name: &'static str,
    frame_p95_ms: f64,
    emulator_mb_s: f64,
    drawn_mb_s: f64,
}

const FLOORS: &[Floor] = &[
    Floor { name: "text", frame_p95_ms: 250.0, emulator_mb_s: 10.0, drawn_mb_s: 0.4 },
    Floor { name: "color", frame_p95_ms: 250.0, emulator_mb_s: 10.0, drawn_mb_s: 0.4 },
    Floor { name: "unicode", frame_p95_ms: 250.0, emulator_mb_s: 10.0, drawn_mb_s: 0.4 },
    // A repaint a frame: its rate is its frames', which the frame time already holds.
    Floor { name: "tui", frame_p95_ms: 250.0, emulator_mb_s: 10.0, drawn_mb_s: 0.0 },
    // A byte a frame: only the frame time means anything.
    Floor { name: "typing", frame_p95_ms: 150.0, emulator_mb_s: 0.0, drawn_mb_s: 0.0 },
];

/// What one workload came to.
pub struct Outcome {
    pub name: &'static str,
    pub bytes: u64,
    pub emulator_mb_s: f64,
    pub drawn_mb_s: f64,
    pub frame: Histogram,
    /// The UI thread's part of each frame: the output taken, the view built and laid out.
    pub build: Histogram,
}

impl Outcome {
    fn line(&self) -> String {
        format!(
            "{:<8} {:>6.1} MB  emulator {:>7.1} MB/s  drawn {:>6.1} MB/s  {} frames, {} (built in p50 {:.2} p95 {:.2})",
            self.name, self.bytes as f64 / 1e6, self.emulator_mb_s, self.drawn_mb_s, self.frame.count(), self.frame.line(),
            self.build.quantile(0.5), self.build.quantile(0.95)
        )
    }

    /// Why this outcome is past its floor, if it is.
    fn under_floor(&self) -> Vec<String> {
        let Some(f) = FLOORS.iter().find(|f| f.name == self.name) else { return vec![] };
        let mut why = vec![];
        if self.frame.quantile(0.95) > f.frame_p95_ms {
            why.push(format!("{}: frame p95 {:.2} ms is over {:.0} ms", self.name, self.frame.quantile(0.95), f.frame_p95_ms));
        }
        if self.emulator_mb_s < f.emulator_mb_s {
            why.push(format!("{}: emulator {:.1} MB/s is under {:.1} MB/s", self.name, self.emulator_mb_s, f.emulator_mb_s));
        }
        if self.drawn_mb_s < f.drawn_mb_s {
            why.push(format!("{}: drawn {:.1} MB/s is under {:.1} MB/s", self.name, self.drawn_mb_s, f.drawn_mb_s));
        }
        why
    }
}

/// Runs `--bench [names]` from the process's arguments, and exits.
pub fn main(args: &[String]) -> ! {
    let at = args.iter().position(|a| a == "--bench").expect("--bench");
    let picked: Vec<&str> = args.get(at + 1).filter(|a| !a.starts_with("--")).map(|a| a.split(',').collect()).unwrap_or_default();
    let check = args.iter().any(|a| a == "--bench-check");
    let scale: f32 = match args.iter().position(|a| a == "--bench-scale") {
        Some(i) => match args.get(i + 1).and_then(|v| v.parse().ok()).filter(|s: &f32| *s > 0.0) {
            Some(s) => s,
            None => die("--bench-scale is a number above 0"),
        },
        None => 1.0,
    };
    let unknown: Vec<&str> = picked.iter().copied().filter(|p| !WORKLOADS.iter().any(|w| w.name == *p)).collect();
    if !unknown.is_empty() {
        let names: Vec<String> = WORKLOADS.iter().map(|w| format!("  {:<8} {}", w.name, w.about)).collect();
        die(&format!("no workload {}; the workloads are:\n{}", unknown.join(", "), names.join("\n")));
    }
    let workloads: Vec<&Workload> = WORKLOADS.iter().filter(|w| picked.is_empty() || picked.contains(&w.name)).collect();

    let gpu = match Gpu::try_new(Gpu::instance(), None) {
        Ok(gpu) => gpu,
        Err(error) => die(&crate::gpu::explain(&error, crate::gpu::adapters())),
    };
    let info = gpu.adapter.get_info();
    let mut bench = Bench::new(gpu, scale);
    let (cols, rows) = bench.pane_size();
    println!(
        "discobox desktop bench: {} ({:?}, {:?}), {}x{} at {scale}x, pane {cols}x{rows}",
        info.name, info.backend, info.device_type, SIZE.0, SIZE.1
    );
    let mut failed = vec![];
    for w in workloads {
        let outcome = bench.run(w);
        println!("{}", outcome.line());
        failed.extend(outcome.under_floor());
    }
    if check && !failed.is_empty() {
        for why in &failed {
            eprintln!("discobox desktop bench: {why}");
        }
        std::process::exit(1);
    }
    std::process::exit(0);
}

fn die(message: &str) -> ! {
    eprintln!("{message}");
    std::process::exit(2);
}

/// The window on the mock, drawn offscreen.
struct Bench {
    gpu: Gpu,
    rt: Runtime<State>,
    renderer: Renderer,
    target: Offscreen,
    pixels: (u32, u32),
}

impl Bench {
    fn new(gpu: Gpu, scale: f32) -> Bench {
        let mut rt = Runtime::with_text(State::new(), view::view, SIZE, Env { system_dark: true, scale }, theme::text_system());
        rt.poll = Some(poll);
        rt.on_key = Some(on_key);
        settle(&mut rt);
        rt.state.open_row(0);
        settle(&mut rt);
        assert!(rt.state.focused_pane().is_some(), "the mock's first box opens on its primary");
        let pixels = ((SIZE.0 * scale).round() as u32, (SIZE.1 * scale).round() as u32);
        // As Rune's headless renders: a target that blends in sRGB when the window does.
        let format = if rt.srgb_blend() { OFFSCREEN_FORMAT.remove_srgb_suffix() } else { OFFSCREEN_FORMAT };
        let mut renderer = Renderer::new(&gpu.device, format);
        renderer.set_srgb_blend(rt.srgb_blend());
        let target = Offscreen::with_format(&gpu.device, pixels.0, pixels.1, format);
        let mut b = Bench { gpu, rt, renderer, target, pixels };
        // Warm the glyph atlas and the pipelines, so the first workload is not charged for them.
        for _ in 0..10 {
            b.frame(None);
        }
        b
    }

    fn pane_size(&self) -> (usize, usize) {
        let p = self.rt.state.focused_pane().expect("a pane");
        (p.screen.cols, p.screen.rows)
    }

    /// One frame: `output` arriving for the pane, the frame built, drawn and finished on the GPU.
    /// Returns how long building it took, in milliseconds.
    fn frame(&mut self, output: Option<Vec<u8>>) -> f64 {
        let t = Instant::now();
        if let Some(bytes) = output {
            let stream = self.rt.state.focused_pane().expect("a pane").stream;
            self.rt.state.inbox.push(Event::Data(stream, bytes));
        }
        self.rt.frame(1.0 / 60.0);
        let built = t.elapsed().as_secs_f64() * 1000.0;
        let Gpu { device, queue, .. } = &self.gpu;
        self.renderer.upload_images(device, queue, &mut self.rt.images);
        self.renderer.prepare_surfaces(device, queue, &self.rt.surfaces, self.pixels);
        self.renderer.prepare(device, queue, &mut self.rt.text.atlas, &self.rt.instances, self.pixels);
        let mut encoder = device.create_command_encoder(&Default::default());
        self.renderer.draw(&mut encoder, &self.target.view, self.rt.clear_color());
        let index = self.renderer.submit(queue, encoder);
        device.poll(wgpu::PollType::Wait { submission_index: Some(index), timeout: None }).expect("the GPU finishes the frame");
        built
    }

    fn run(&mut self, w: &Workload) -> Outcome {
        let (cols, rows) = self.pane_size();
        let slices: Vec<Vec<u8>> = match w.kind {
            Kind::Stream(make) => make(STREAMED).chunks(SLICE).map(<[u8]>::to_vec).collect(),
            Kind::Frame(make) => (0..FRAMES).map(|k| make(k, cols, rows)).collect(),
        };
        let bytes: u64 = slices.iter().map(|s| s.len() as u64).sum();
        let emulator_mb_s = emulate(&slices, cols, rows);
        let (mut frame, mut build) = (Histogram::default(), Histogram::default());
        let start = Instant::now();
        for slice in slices {
            let t = Instant::now();
            build.add(self.frame(Some(slice)));
            frame.add(t.elapsed().as_secs_f64() * 1000.0);
        }
        let drawn_mb_s = mb_per_s(bytes, start.elapsed().as_secs_f64());
        // A clean screen for the next workload, outside its timing.
        self.frame(Some(b"\x1b[0m\x1b[2J\x1b[3J\x1b[H".to_vec()));
        Outcome { name: w.name, bytes, emulator_mb_s, drawn_mb_s, frame, build }
    }
}

fn settle(rt: &mut Runtime<State>) {
    for _ in 0..20 {
        rt.frame(1.0 / 60.0);
    }
}

/// The emulator's rate for `slices` on a pane's grid, with nothing drawn: the best of three runs.
fn emulate(slices: &[Vec<u8>], cols: usize, rows: usize) -> f64 {
    let bytes: u64 = slices.iter().map(|s| s.len() as u64).sum();
    (0..3)
        .map(|_| {
            let mut screen = Screen::new(cols, rows);
            let t = Instant::now();
            for s in slices {
                screen.feed(s);
            }
            mb_per_s(bytes, t.elapsed().as_secs_f64())
        })
        .fold(0.0, f64::max)
}

/// A small deterministic generator (xorshift), so every run feeds the same bytes.
struct Rng(u64);

impl Rng {
    fn next(&mut self) -> u64 {
        self.0 ^= self.0 << 13;
        self.0 ^= self.0 >> 7;
        self.0 ^= self.0 << 17;
        self.0
    }
    fn below(&mut self, n: u64) -> u64 { self.next() % n }
}

const WORDS: &[&str] = &[
    "build", "step", "ok", "compiling", "discobox", "sandbox", "warning:", "error[E0308]", "src/main.rs:42:7", "test",
    "passed", "finished", "release", "target", "the", "of", "and", "a", "with", "cargo", "go", "module", "2026-11-10T12:00:00Z",
];

/// Lines of words until `n` bytes: a log.
fn text(n: usize) -> Vec<u8> {
    let mut r = Rng(0x9e37_79b9_7f4a_7c15);
    let mut out = Vec::with_capacity(n + 256);
    while out.len() < n {
        let width = 40 + r.below(100) as usize;
        let start = out.len();
        while out.len() - start < width {
            out.extend_from_slice(WORDS[r.below(WORDS.len() as u64) as usize].as_bytes());
            out.push(b' ');
        }
        out.extend_from_slice(b"\r\n");
    }
    out
}

/// Lines of coloured runs until `n` bytes: a build's output, a listing in colour.
fn color(n: usize) -> Vec<u8> {
    let mut r = Rng(0xd1b5_4a32_d192_ed03);
    let mut out = Vec::with_capacity(n + 256);
    while out.len() < n {
        for _ in 0..(3 + r.below(8)) {
            let word = WORDS[r.below(WORDS.len() as u64) as usize];
            let bold = if r.below(4) == 0 { "1;" } else { "" };
            out.extend_from_slice(format!("\x1b[{bold}38;5;{}m{word}\x1b[0m ", r.below(256)).as_bytes());
        }
        out.extend_from_slice(b"\r\n");
    }
    out
}

const WIDE: &[&str] = &["漢字", "かな", "한국어", "中文", "😀", "🚀", "🦀", "é", "ß", "→", "✓"];

/// Lines mixing wide characters and emoji with ASCII until `n` bytes.
fn unicode(n: usize) -> Vec<u8> {
    let mut r = Rng(0x2545_f491_4f6c_dd1d);
    let mut out = Vec::with_capacity(n + 256);
    while out.len() < n {
        for _ in 0..(4 + r.below(10)) {
            let piece = if r.below(2) == 0 { WIDE[r.below(WIDE.len() as u64) as usize] } else { WORDS[r.below(WORDS.len() as u64) as usize] };
            out.extend_from_slice(piece.as_bytes());
            out.push(b' ');
        }
        out.extend_from_slice(b"\r\n");
    }
    out
}

/// Frame `k` of a full-screen program: every cell rewritten, each row in its own colours, the
/// pattern moving a cell a frame so no row repeats the frame before's.
fn tui(k: usize, cols: usize, rows: usize) -> Vec<u8> {
    let mut out = Vec::with_capacity(cols * rows * 2);
    out.extend_from_slice(b"\x1b[?25l\x1b[H");
    for y in 0..rows {
        let shade = ((y * 7 + k) % 200) as u8;
        out.extend_from_slice(format!("\x1b[{};1H\x1b[48;2;{};{};40m\x1b[38;2;220;{};{}m", y + 1, 20 + shade / 4, 30, shade, 255 - shade).as_bytes());
        for x in 0..cols {
            out.push(b'!' + ((x + y + k) % 90) as u8);
        }
    }
    out.extend_from_slice(b"\x1b[0m");
    out
}

/// Frame `k` at a shell prompt: one key echoed, and a new prompt every so often.
fn typing(k: usize, _cols: usize, _rows: usize) -> Vec<u8> {
    if k % 40 == 39 { b"\r\n$ ".to_vec() } else { vec![b"discobox run"[k % 12]] }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn workloads_are_the_same_bytes_every_run() {
        assert_eq!(text(4096), text(4096));
        assert_eq!(color(4096), color(4096));
        assert_eq!(unicode(4096), unicode(4096));
        assert!(text(4096).len() >= 4096 && std::str::from_utf8(&unicode(4096)).is_ok());
    }

    #[test]
    fn a_tui_frame_rewrites_every_cell_and_the_next_differs() {
        let (cols, rows) = (80, 24);
        let mut screen = Screen::new(cols, rows);
        screen.feed(&tui(0, cols, rows));
        let rows_of = |s: &Screen| (0..rows).map(|y| s.row_text(y)).collect::<Vec<_>>();
        let first = rows_of(&screen);
        assert!(first.iter().all(|row| row.trim_end().chars().count() == cols), "every cell written: {first:?}");
        screen.feed(&tui(1, cols, rows));
        assert!(rows_of(&screen).iter().zip(&first).all(|(a, b)| a != b), "no row repeats the frame before's");
    }

    #[test]
    fn every_workload_has_a_floor() {
        for w in WORKLOADS {
            assert!(FLOORS.iter().any(|f| f.name == w.name), "{} has no floor", w.name);
        }
    }

    #[test]
    fn a_slow_outcome_says_which_floor_it_is_under() {
        let mut frame = Histogram::default();
        frame.add(500.0);
        let o = Outcome { name: "text", bytes: 1, emulator_mb_s: 1.0, drawn_mb_s: 10.0, frame, build: Histogram::default() };
        let why = o.under_floor();
        assert_eq!(why.len(), 2, "{why:?}");
        assert!(why[0].contains("frame p95") && why[1].contains("emulator"), "{why:?}");
    }
}
