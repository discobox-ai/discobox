//! Frame time and terminal throughput, measured as the app runs: what `--bench` reports
//! (`bench.rs`) and, with `DISCOBOX_DESKTOP_PERF=1`, what a window on real boxes reports as the
//! app ends (`report`). Both count with the same histogram, so their numbers read alike.

use std::sync::Mutex;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Instant;

/// The window's recorder, when `DISCOBOX_DESKTOP_PERF` asks for one: recorded into on the UI
/// thread, and reported from whichever way the app ends (the window closed or quit, or the bridge
/// gone, on its reader's thread).
static RECORDER: Mutex<Option<Recorder>> = Mutex::new(None);

/// Starts the window's recorder when `DISCOBOX_DESKTOP_PERF` asks for one; whether it did.
pub fn start_from_env() -> bool {
    let on = std::env::var_os("DISCOBOX_DESKTOP_PERF").is_some_and(|v| !v.is_empty() && v != "0");
    if on {
        *RECORDER.lock().unwrap_or_else(|e| e.into_inner()) = Some(Recorder::new(TAKEN.load(Ordering::Relaxed)));
    }
    on
}

/// A frame of the window drawn at `at`, whose update and encode took `cpu_ms`.
pub fn record(at: Instant, cpu_ms: Option<f64>) {
    if let Some(r) = RECORDER.lock().unwrap_or_else(|e| e.into_inner()).as_mut() {
        r.frame(at, cpu_ms, TAKEN.load(Ordering::Relaxed));
    }
}

/// Prints what the window's recorder measured to stderr, once: every way the app ends calls it.
/// A Ctrl-C in the terminal ends the app before it can, so a measured run is ended by closing
/// the window or quitting.
pub fn report() {
    if let Some(r) = RECORDER.lock().unwrap_or_else(|e| e.into_inner()).take() {
        eprintln!("{}", r.summary());
    }
}

/// Terminal output the window took into its panes, in bytes, since the process started. Counted
/// always (one relaxed add per chunk); only a recorder reads it.
pub static TAKEN: AtomicU64 = AtomicU64::new(0);

/// Counts `bytes` of a stream's output taken into a pane.
pub fn took(bytes: usize) { TAKEN.fetch_add(bytes as u64, Ordering::Relaxed); }

/// Durations in milliseconds, bounded however long the window runs: 0.05 ms buckets to 250 ms,
/// and the exact largest.
pub struct Histogram {
    buckets: Vec<u32>,
    count: u64,
    max: f64,
}

const STEP_MS: f64 = 0.05;
const BUCKETS: usize = 5000;

impl Default for Histogram {
    fn default() -> Self { Histogram { buckets: vec![0; BUCKETS + 1], count: 0, max: 0.0 } }
}

impl Histogram {
    pub fn add(&mut self, ms: f64) {
        if !ms.is_finite() || ms < 0.0 {
            return;
        }
        let i = ((ms / STEP_MS) as usize).min(BUCKETS);
        self.buckets[i] += 1;
        self.count += 1;
        self.max = self.max.max(ms);
    }

    pub fn count(&self) -> u64 { self.count }

    pub fn max(&self) -> f64 { self.max }

    /// The `q` quantile (0..=1), to the bucket's upper edge: never under the true value, and the
    /// largest for a quantile in the overflow bucket.
    pub fn quantile(&self, q: f64) -> f64 {
        if self.count == 0 {
            return 0.0;
        }
        let rank = ((self.count as f64 * q).ceil() as u64).clamp(1, self.count);
        let mut seen = 0;
        for (i, n) in self.buckets.iter().enumerate() {
            seen += *n as u64;
            if seen >= rank {
                return if i == BUCKETS { self.max } else { ((i + 1) as f64 * STEP_MS).min(self.max) };
            }
        }
        self.max
    }

    /// `p50 1.20 p95 3.40 max 9.80 ms`.
    pub fn line(&self) -> String {
        format!("p50 {:.2} p95 {:.2} max {:.2} ms", self.quantile(0.5), self.quantile(0.95), self.max())
    }
}

/// Megabytes (10^6 bytes) a second.
pub fn mb_per_s(bytes: u64, seconds: f64) -> f64 { if seconds <= 0.0 { 0.0 } else { bytes as f64 / 1e6 / seconds } }

/// A window's frames as the app saw them (`DISCOBOX_DESKTOP_PERF=1`): what each drawn frame cost
/// the UI thread to build and encode, how far apart frames came while a program's output was
/// arriving, and the most output taken in any one second.
pub struct Recorder {
    /// Update and encode of each drawn frame: the UI thread's work, not the GPU's.
    pub cpu: Histogram,
    /// The time from the frame before to each frame that took output: how smoothly a stream of
    /// output was drawn.
    pub busy_gap: Histogram,
    last_frame: Option<Instant>,
    last_taken: u64,
    /// What had been taken when it started.
    first_taken: u64,
    /// The second being counted: when it started, and what had been taken by then.
    second: (Option<Instant>, u64),
    pub peak_per_s: u64,
    pub drawn: u64,
    started: Instant,
}

impl Recorder {
    /// A recorder starting with `taken` bytes of output already taken.
    pub fn new(taken: u64) -> Recorder {
        Recorder {
            cpu: Histogram::default(), busy_gap: Histogram::default(), last_frame: None, last_taken: taken, first_taken: taken,
            second: (None, 0), peak_per_s: 0, drawn: 0, started: Instant::now(),
        }
    }

    /// A frame drawn at `at`, whose update and encode took `cpu_ms`, with `taken` bytes of
    /// output taken by then (`TAKEN`).
    pub fn frame(&mut self, at: Instant, cpu_ms: Option<f64>, taken: u64) {
        self.drawn += 1;
        if let Some(ms) = cpu_ms {
            self.cpu.add(ms);
        }
        if taken > self.last_taken
            && let Some(last) = self.last_frame
        {
            self.busy_gap.add(at.duration_since(last).as_secs_f64() * 1000.0);
        }
        self.last_taken = taken;
        self.last_frame = Some(at);
        match self.second {
            (Some(start), from) if at.duration_since(start).as_secs_f64() >= 1.0 => {
                let per_s = ((taken - from) as f64 / at.duration_since(start).as_secs_f64()) as u64;
                self.peak_per_s = self.peak_per_s.max(per_s);
                self.second = (Some(at), taken);
            }
            (None, _) => self.second = (Some(at), taken),
            _ => {}
        }
    }

    /// What the window measured, for stderr as it closes.
    pub fn summary(&self) -> String {
        let taken = self.last_taken - self.first_taken;
        format!(
            "discobox desktop perf: {} frames drawn in {:.0} s; frame cpu {}; \
             while output arrived, frames {} apart ({} frames); terminal output {:.1} MB, peak {:.2} MB/s",
            self.drawn,
            self.started.elapsed().as_secs_f64(),
            self.cpu.line(),
            self.busy_gap.line(),
            self.busy_gap.count(),
            taken as f64 / 1e6,
            self.peak_per_s as f64 / 1e6,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn quantiles_are_never_under_the_value() {
        let mut h = Histogram::default();
        for ms in 1..=100 {
            h.add(ms as f64);
        }
        assert_eq!(h.count(), 100);
        assert!((h.quantile(0.5) - 50.0).abs() <= STEP_MS + 1e-9, "p50 {}", h.quantile(0.5));
        assert!(h.quantile(0.95) >= 95.0 && h.quantile(0.95) <= 95.0 + STEP_MS + 1e-9, "p95 {}", h.quantile(0.95));
        assert_eq!(h.quantile(1.0), 100.0);
        assert_eq!(h.max(), 100.0);
    }

    #[test]
    fn a_long_frame_lands_in_the_overflow_and_reads_as_the_largest() {
        let mut h = Histogram::default();
        h.add(1.0);
        h.add(900.0);
        assert_eq!(h.quantile(1.0), 900.0);
        assert_eq!(h.quantile(0.5), 1.0 + STEP_MS);
    }

    #[test]
    fn nothing_measured_reads_as_zero() {
        let h = Histogram::default();
        assert_eq!((h.quantile(0.95), h.max()), (0.0, 0.0));
        h.line();
        assert_eq!(mb_per_s(5, 0.0), 0.0);
    }

    #[test]
    fn a_recorder_counts_gaps_only_while_output_arrives() {
        let mut r = Recorder::new(0);
        let t0 = Instant::now();
        let ms = |n: u64| t0 + std::time::Duration::from_millis(n);
        r.frame(ms(0), Some(1.0), 0);
        r.frame(ms(16), Some(1.0), 0);
        assert_eq!(r.busy_gap.count(), 0, "no output, no gap counted");
        r.frame(ms(40), Some(2.0), 1000);
        assert_eq!(r.busy_gap.count(), 1);
        assert!(r.busy_gap.max() >= 24.0 - 1e-9 && r.busy_gap.max() <= 24.0 + 1e-9);
        assert_eq!(r.drawn, 3);
        r.frame(ms(1040), Some(1.0), 3_000_000);
        assert!(r.peak_per_s >= 2_800_000, "3 MB in the 1.04 s after the first frame: {}", r.peak_per_s);
        assert!(r.summary().contains("4 frames drawn"), "{}", r.summary());
    }
}
