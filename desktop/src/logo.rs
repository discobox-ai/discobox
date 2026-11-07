//! The discobox mark, from the TUI's own cell data (`cli/internal/tui/logo.json`). A terminal
//! draws it with block elements; here each block becomes the rectangle it stands for, so it is
//! crisp at any size.

use rune::{El, el};
use serde::Deserialize;

use crate::theme;

#[derive(Deserialize)]
struct Doc {
    width: usize,
    rows: Vec<Vec<Run>>,
}

#[derive(Deserialize)]
struct Run {
    t: String,
    f: Option<String>,
    b: Option<String>,
}

/// The part of a cell a block element covers, as `[x0, y0, x1, y1]` fractions.
fn shape(c: char) -> Option<[f32; 4]> {
    let lower = |n: f32| [0.0, 1.0 - n / 8.0, 1.0, 1.0];
    let left = |n: f32| [0.0, 0.0, n / 8.0, 1.0];
    Some(match c {
        '▁' => lower(1.0),
        '▂' => lower(2.0),
        '▃' => lower(3.0),
        '▄' => lower(4.0),
        '▅' => lower(5.0),
        '▆' => lower(6.0),
        '▇' => lower(7.0),
        '█' => lower(8.0),
        '▎' => left(2.0),
        '▍' => left(3.0),
        '▌' => left(4.0),
        '▋' => left(5.0),
        '▖' => [0.0, 0.5, 0.5, 1.0],
        '▗' => [0.5, 0.5, 1.0, 1.0],
        '▘' => [0.0, 0.0, 0.5, 0.5],
        '▝' => [0.5, 0.0, 1.0, 0.5],
        _ => return None,
    })
}

fn parse_hex(s: &str) -> rune::Color {
    theme::hex(u32::from_str_radix(s.trim_start_matches('#'), 16).unwrap_or(0xf45cff))
}

/// The mark at `cell` points wide (cells are twice as tall as wide), on `ground`.
pub fn view(cell: f32, ground: rune::Color) -> El {
    let doc: Doc = serde_json::from_str(include_str!("../../cli/internal/tui/logo.json")).expect("logo.json");
    let (cw, ch) = (cell, cell * 2.0);
    let mut out = el().size(doc.width as f32 * cw, doc.rows.len() as f32 * ch);
    for (y, row) in doc.rows.iter().enumerate() {
        let mut x = 0usize;
        for run in row {
            for c in run.t.chars() {
                let (px, py) = (x as f32 * cw, y as f32 * ch);
                let rect = |r: [f32; 4], col| {
                    el().absolute(px + r[0] * cw, py + r[1] * ch).size((r[2] - r[0]) * cw + 0.25, (r[3] - r[1]) * ch + 0.25).fill(col)
                };
                if let Some(b) = &run.b {
                    // Reverse video: the cell is the colour, the block is cut out of it.
                    out = out.child(rect([0.0, 0.0, 1.0, 1.0], parse_hex(b)));
                    if let Some(s) = shape(c) {
                        out = out.child(rect(s, ground));
                    }
                } else if let (Some(f), Some(s)) = (&run.f, shape(c)) {
                    out = out.child(rect(s, parse_hex(f)));
                }
                x += 1;
            }
        }
    }
    out
}
