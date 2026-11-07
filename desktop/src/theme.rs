//! The console's colours: the TUI's own (`cli/internal/tui/theme.go`), given as the xterm-256
//! values it names, so the native window reads as the same product.

use rune::{Color, TextStyle};

/// `0xrrggbb` in sRGB, as the TUI's palette is written, to Rune's linear colour.
pub fn hex(rgb: u32) -> Color {
    let lin = |v: u32| {
        let c = v as f32 / 255.0;
        if c <= 0.04045 { c / 12.92 } else { ((c + 0.055) / 1.055).powf(2.4) }
    };
    Color { r: lin((rgb >> 16) & 0xff), g: lin((rgb >> 8) & 0xff), b: lin(rgb & 0xff), a: 1.0 }
}

thread_local! {
    static TOKENS: std::cell::RefCell<std::collections::HashMap<String, Color>> = Default::default();
}

/// Takes the spec's colour tokens (`tui.Spec`), which the CLI reads from the TUI's own theme.
pub fn set_tokens(colors: &std::collections::HashMap<String, String>) {
    TOKENS.with_borrow_mut(|t| {
        for (k, v) in colors {
            if let Ok(rgb) = u32::from_str_radix(v.trim_start_matches('#'), 16) {
                t.insert(k.clone(), hex(rgb));
            }
        }
    });
}

/// A spec colour token, or `fallback` where the spec names none.
pub fn token(name: &str, fallback: u32) -> Color {
    TOKENS.with_borrow(|t| t.get(name).copied()).unwrap_or_else(|| hex(fallback))
}

/// Window ground: a terminal's black. Not a token: the TUI draws on the terminal's own.
pub fn bg() -> Color { hex(0x0b0b0d) }
/// Plain text: the TUI's is the terminal's foreground.
pub fn text() -> Color { token("text", 0xe4e4e4) }
pub fn mark() -> Color { token("mark", 0xf45cff) }
pub fn inactive() -> Color { token("inactive", 0x303030) }
pub fn gold() -> Color { token("gold", 0xffd700) }
pub fn on_gold() -> Color { token("onGold", 0x080808) }
pub fn dim() -> Color { token("dim", 0x585858) }
pub fn grey() -> Color { token("grey", 0x8a8a8a) }
pub fn row_cursor() -> Color { token("rowCursor", 0x3a3a3a) }
pub fn band() -> Color { token("band", 0x303030) }
pub fn warn() -> Color { token("warn", 0xffaf00) }
pub fn done() -> Color { token("add", 0x87ff87) }
pub fn salmon() -> Color { token("del", 0xffaf87) }
pub fn err() -> Color { token("err", 0xff3b3b) }

/// Chrome is set in the UI face; terminals in the mono one.
pub const UI: TextStyle = TextStyle::new(13.0, 400.0);
pub const UI_BOLD: TextStyle = TextStyle::new(13.0, 600.0);
pub const UI_SMALL: TextStyle = TextStyle::new(12.0, 400.0);

/// Terminals use the platform's monospaced face: SF Mono on macOS, fontconfig's `monospace`
/// on Linux, Cascadia Mono or Consolas on Windows.
pub const TERM_SIZE: f32 = 13.0;
pub const CELL_H: f32 = 17.0;
pub const TERM: TextStyle = TextStyle::new(TERM_SIZE, 400.0).family(rune::tok::Family::SystemMono).lh(CELL_H);

thread_local! {
    static CELL_W: std::cell::Cell<f32> = const { std::cell::Cell::new(0.0) };
}

/// A cell's width: the face's advance, measured once, since faces differ (0.6 em for SF Mono,
/// 0.602 for DejaVu Sans Mono and Menlo).
pub fn cell_w() -> f32 {
    CELL_W.with(|c| {
        if c.get() == 0.0 {
            c.set(rune::text::advance(&"M".repeat(100), TERM) / 100.0);
        }
        c.get()
    })
}

/// The 256-colour palette a terminal program addresses, xterm's.
pub fn xterm(i: u8) -> Color {
    const BASE: [u32; 16] = [
        0x1c1c1c, 0xe0555a, 0x5fd75f, 0xe5c07b, 0x61afef, 0xc678dd, 0x56b6c2, 0xd0d0d0,
        0x6b6b6b, 0xff6b70, 0x87ff87, 0xffd75f, 0x87afff, 0xf45cff, 0x87d7d7, 0xffffff,
    ];
    match i {
        0..=15 => hex(BASE[i as usize]),
        16..=231 => {
            let i = i - 16;
            let step = |v: u8| if v == 0 { 0 } else { 55 + v as u32 * 40 };
            hex(step(i / 36) << 16 | step((i / 6) % 6) << 8 | step(i % 6))
        }
        _ => {
            let v = 8 + (i as u32 - 232) * 10;
            hex(v << 16 | v << 8 | v)
        }
    }
}

pub fn term_fg() -> Color { hex(0xdadada) }
