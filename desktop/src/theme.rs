//! The console's look on desktop (ADR 26-11-07-363): the design language's tokens
//! (`assets/brand/tokens.json`, `LANGUAGE.md`), as the console core's spec hands them over, in
//! both palettes, and JetBrains Mono, bundled. The TUI draws the same tokens by their xterm
//! indices, so the two read as one product.
//!
//! The window follows the system's appearance: [`view`](crate::view::view) sets it from Rune's
//! each frame, and every colour here reads it. Rune's `--theme light|dark` picks one, which is
//! how the headless snapshots draw both.

use std::cell::{Cell, RefCell};
use std::collections::HashMap;

use rune::tok::Family;
use rune::{Color, Spring, TextStyle};

/// `0xrrggbb` in sRGB, as the tokens are written, to Rune's linear colour.
pub fn hex(rgb: u32) -> Color {
    let lin = |v: u32| {
        let c = v as f32 / 255.0;
        if c <= 0.04045 { c / 12.92 } else { ((c + 0.055) / 1.055).powf(2.4) }
    };
    Color { r: lin((rgb >> 16) & 0xff), g: lin((rgb >> 8) & 0xff), b: lin(rgb & 0xff), a: 1.0 }
}

fn parse(s: &str) -> Option<Color> { u32::from_str_radix(s.trim_start_matches('#'), 16).ok().map(hex) }

thread_local! {
    /// Each token's dark and light colour, by its name in the spec.
    static TOKENS: RefCell<HashMap<String, [Color; 2]>> = Default::default();
    /// Each space, radius and stroke token, in points.
    static DIMENSIONS: RefCell<HashMap<String, f32>> = Default::default();
    static DARK: Cell<bool> = const { Cell::new(true) };
}

/// Takes the spec's tokens: its colours in both palettes, and its space, radii and strokes.
pub fn set_tokens(spec: &crate::wire::Spec) {
    TOKENS.with_borrow_mut(|t| {
        for (k, v) in &spec.colors {
            if let (Some(dark), Some(light)) = (parse(&v.dark), parse(&v.light)) {
                t.insert(k.clone(), [dark, light]);
            }
        }
    });
    DIMENSIONS.with_borrow_mut(|d| d.extend(spec.dimensions.iter().map(|(k, v)| (k.clone(), *v as f32))));
}

/// Which palette the window draws: the system's appearance, as Rune reports it.
pub fn set_dark(dark: bool) { DARK.with(|d| d.set(dark)) }

pub fn dark() -> bool { DARK.with(Cell::get) }

/// A colour token by its name in the spec, in the window's palette. Every name the app asks
/// for is in the spec (`identity_tests`); one that is not draws nothing rather than a colour
/// the design language does not name.
pub fn token(name: &str) -> Color {
    let i = if dark() { 0 } else { 1 };
    TOKENS.with_borrow(|t| t.get(name).map(|c| c[i])).unwrap_or(Color::TRANSPARENT)
}

/// A space, radius or stroke token by its name in the spec, in points; 0 for a name it does
/// not carry, which `identity_tests` rules out.
pub fn length(name: &str) -> f32 { DIMENSIONS.with_borrow(|d| d.get(name).copied()).unwrap_or(0.0) }

/// Page and window background.
pub fn bg() -> Color { token("ink") }
/// Frames, panels, menus and the palette.
pub fn raised() -> Color { token("ink-raised") }
/// Inputs, and the ground terminal content is drawn on.
pub fn sunken() -> Color { token("ink-sunken") }
/// Body text and headings.
pub fn text() -> Color { token("text") }
/// The accent: focus, selection, the brand.
pub fn mark() -> Color { token("magenta") }
/// The mark's own purple, for the vector mark only.
pub fn brand() -> Color { token("brand") }
/// Hairlines and unfocused frames.
pub fn line() -> Color { token("line") }
/// Dividers that must show, hover outlines, keycap edges.
pub fn line_bright() -> Color { token("line-bright") }
pub fn gold() -> Color { token("gold") }
pub fn on_gold() -> Color { token("on-gold") }
/// Hints, disabled, metadata.
pub fn dim() -> Color { token("text-faint") }
/// Secondary text.
pub fn grey() -> Color { token("text-muted") }
/// The selected row, and selected text.
pub fn selection() -> Color { token("selection") }
pub fn warn() -> Color { token("warn") }
pub fn done() -> Color { token("green") }
pub fn salmon() -> Color { token("salmon") }
pub fn err() -> Color { token("err") }
/// Something that passed or is open: a check, an approval, an open issue.
pub fn ok() -> Color { token("ok") }
/// Information that is not a state: a merge, a link, code in a sentence, a diff's hunk.
pub fn info() -> Color { token("info") }

/// What a word or a mark on an issue's tab is drawn in, by the tone the core gave it
/// (`console.IssueTone`).
pub fn tone(t: crate::wire::IssueTone) -> Color {
    use crate::wire::IssueTone;
    match t {
        IssueTone::Open | IssueTone::Good => ok(),
        IssueTone::Merged => info(),
        IssueTone::Warn => warn(),
        IssueTone::Bad => err(),
        IssueTone::Busy => gold(),
        IssueTone::Shut | IssueTone::Quiet => dim(),
    }
}

/// A GitHub label's own colour, six hex digits as its page draws it: the issue's data, not the
/// chrome's, drawn only as the label's dot; none for one that is not a colour.
pub fn label(rgb: &str) -> Option<Color> { (rgb.len() == 6).then(|| parse(rgb)).flatten() }

/// How an issue's markdown is drawn: the window's own text and tokens, in its face, code on the
/// sunken ground terminals are drawn on, links and inline code in the info colour.
pub fn markdown() -> rune_kit::markdown::MarkdownStyle {
    rune_kit::markdown::MarkdownStyle::neutral(dark())
        .text(text())
        .muted(grey())
        .link(info())
        .link_underline(line_bright())
        .inline_code_bg(sunken())
        .code_block(sunken(), line())
        .quote_rule(line_bright())
        .rule(line())
        .selection(selection())
        .diff(done(), err())
        .diff_hunk(info())
        .body_font(UI.lh(20.0))
        .heading_font(1, TextStyle::new(15.0, 600.0).family(Family::Mono).lh(22.0))
        .heading_font(2, TextStyle::new(14.0, 600.0).family(Family::Mono).lh(20.0))
        .heading_font(3, UI_BOLD.lh(20.0))
        .code_block_font(TERM)
}

// ---------------------------------------------------------------------------------------------
// Type: JetBrains Mono throughout (§2), hierarchy from weight, size and colour.

/// The faces the app bundles (SIL Open Font License, `desktop/fonts/OFL.txt`), at the weights
/// the chrome sets, and the italic terminals ask for.
pub const FONTS: &[&[u8]] = &[
    include_bytes!("../fonts/JetBrainsMono-Regular.ttf"),
    include_bytes!("../fonts/JetBrainsMono-Medium.ttf"),
    include_bytes!("../fonts/JetBrainsMono-SemiBold.ttf"),
    include_bytes!("../fonts/JetBrainsMono-Bold.ttf"),
    include_bytes!("../fonts/JetBrainsMono-Italic.ttf"),
];
pub const FAMILY: &str = "JetBrains Mono";

/// A text system in the app's faces, as the window's: tests lay out and measure with what ships.
/// Rune keeps the faces it measures with per process, so every test's runtime is built on it.
#[cfg(test)]
pub fn text_system() -> rune::text::TextSystem { rune::text::TextSystem::new(FONTS, (FAMILY, FAMILY)) }

/// Interface text.
pub const UI: TextStyle = TextStyle::new(13.0, 400.0).family(Family::Mono);
/// What is selected or shown, and titles.
pub const UI_BOLD: TextStyle = TextStyle::new(13.0, 600.0).family(Family::Mono);
/// Captions, hints, keys and metadata: `--step--1`'s weight. The type scale's sizes are rem on a
/// page (16 px a step-0); the window sets its interface at its own density, as the TUI's is the
/// terminal's.
pub const UI_SMALL: TextStyle = TextStyle::new(11.5, 500.0).family(Family::Mono);

/// Terminals take the same face, at its measured advance.
pub const TERM_SIZE: f32 = 13.0;
pub const CELL_H: f32 = 17.0;
pub const TERM: TextStyle = TextStyle::new(TERM_SIZE, 400.0).family(Family::Mono).lh(CELL_H);

thread_local! {
    static CELL_W: Cell<f32> = const { Cell::new(0.0) };
}

/// A cell's width: JetBrains Mono's advance, measured once (0.6 em).
pub fn cell_w() -> f32 {
    CELL_W.with(|c| {
        if c.get() == 0.0 {
            c.set(rune::text::advance(&"M".repeat(100), TERM) / 100.0);
        }
        c.get()
    })
}

// ---------------------------------------------------------------------------------------------
// Shape and motion (`LANGUAGE.md`, Space, shape, motion): the shape is the spec's tokens.

/// Keys and chips.
pub fn r_key() -> f32 { length("radius-key") }
/// Rows and bands.
pub fn r_row() -> f32 { length("radius-row") }
/// Menus and the palette.
pub fn r_card() -> f32 { length("radius-card") }
/// Frames and panes.
pub fn r_frame() -> f32 { length("radius-frame") }
/// A focused frame, in the accent.
pub fn stroke_focus() -> f32 { length("stroke-focus") }
/// An unfocused frame, in the line colour.
pub fn stroke_quiet() -> f32 { length("stroke-quiet") }
/// Focus, chips, rows, the sidebar and maximize. The tokens carry no motion; the design
/// language names Rune's springs.
pub const SNAPPY: Spring = rune::tok::motion::SNAPPY;

// ---------------------------------------------------------------------------------------------
// Terminal content keeps the program's own colours in both themes, on the palette's sunken
// ground (§3).

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

/// A terminal's ground.
pub fn term_bg() -> Color { sunken() }
/// A terminal's default foreground: the palette's text, so it reads on either ground.
pub fn term_fg() -> Color { text() }
