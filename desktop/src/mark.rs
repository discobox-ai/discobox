//! The discobox mark as vector art (ADR 26-11-07-363 §1): the brand's own outline
//! (`assets/brand/logo-purple.svg`, the drawing the TUI's block-element capture is rendered
//! from), drawn by Rune's icon rasteriser in the brand purple, crisp at any size.
//!
//! Not Rune's `ImageCache::svg`: that decodes on worker threads and draws nothing until a later
//! frame, which needs a waker in the state and a wait in every one-frame scene (`--snap`), and
//! rasterises once per size. An icon is rasterised in the frame that draws it, through the glyph
//! atlas, at any size. The cost is moving the SVG's paths onto the square, origin-anchored view
//! box Rune's icons take, which is all the reading below does. It is tinted with the `brand`
//! token, which is the SVG's own purple in both palettes.

use std::sync::OnceLock;

use rune::{El, Icon, icon};

use crate::theme;

const SVG: &str = include_str!("../../assets/brand/logo-purple.svg");

/// The mark, `size` points square, in the brand colour.
pub fn view(size: f32) -> El { icon(mark(), size).color(theme::brand()).key("mark") }

/// The SVG's paths as a Rune icon. A Rune icon's view box starts at the origin and is square, so
/// each path moves from the SVG's view box onto a square one, centred on the shorter side.
pub fn mark() -> &'static Icon {
    static ICON: OnceLock<Icon> = OnceLock::new();
    ICON.get_or_init(|| {
        let vb: Vec<f32> = attr(SVG, "viewBox").expect("the mark has a viewBox").split_whitespace().map(|v| v.parse().expect("a viewBox number")).collect();
        let [x, y, w, h] = vb[..] else { panic!("the mark's viewBox is four numbers") };
        let side = w.max(h);
        let (dx, dy) = ((side - w) / 2.0 - x, (side - h) / 2.0 - y);
        let mut body = String::new();
        for el in SVG.split('<').filter(|e| e.starts_with("path ")) {
            let d = attr(el, " d").expect("a path has d");
            let rule = if attr(el, "class").is_some_and(|c| class_rule(c).contains("evenodd")) { " fill-rule=\"evenodd\"" } else { "" };
            body.push_str(&format!("<path d=\"{}\"{rule} fill=\"currentColor\"/>", translate(d, dx, dy)));
        }
        Icon { name: "discobox-mark", view: side, body: Box::leak(body.into_boxed_str()) }
    })
}

/// The declarations of `.class` in the SVG's style sheet, where its fill rule is set.
fn class_rule(class: &str) -> &'static str {
    let Some(at) = SVG.find(&format!(".{class} {{")) else { return "" };
    let rule = &SVG[at..];
    &rule[..rule.find('}').unwrap_or(rule.len())]
}

/// The value of `name="…"` in `el`.
fn attr<'a>(el: &'a str, name: &str) -> Option<&'a str> {
    let at = el.find(&format!("{name}=\""))? + name.len() + 2;
    Some(&el[at..at + el[at..].find('"')?])
}

/// SVG path data moved by `(dx, dy)`: absolute commands' coordinates move, relative ones are
/// relative to a point that moved with them, and are kept as written.
fn translate(d: &str, dx: f32, dy: f32) -> String {
    let mut out = String::new();
    let mut rest = d;
    while let Some(c) = rest.chars().next() {
        let at = rest[1..].find(|ch: char| ch.is_ascii_alphabetic() && ch != 'e' && ch != 'E').map_or(rest.len(), |i| i + 1);
        let (seg, args) = (&rest[..at], &rest[1..at]);
        rest = &rest[at..];
        // The x and y of each argument, by its place in one command's group of arguments.
        let shift = |i: usize| -> f32 {
            match c {
                'M' | 'L' | 'T' | 'C' | 'S' | 'Q' => if i.is_multiple_of(2) { dx } else { dy },
                'H' => dx,
                'V' => dy,
                'A' => match i % 7 { 5 => dx, 6 => dy, _ => 0.0 },
                _ => 0.0,
            }
        };
        if !c.is_ascii_uppercase() || c == 'Z' {
            out.push_str(seg);
            continue;
        }
        out.push(c);
        let nums = numbers(args);
        out.push_str(&nums.iter().enumerate().map(|(i, n)| format!("{}", n + shift(i))).collect::<Vec<_>>().join(" "));
    }
    out
}

/// SVG's compact number list: `1.5.5-2` is 1.5, .5 and -2.
fn numbers(s: &str) -> Vec<f32> {
    let mut out = vec![];
    let mut cur = String::new();
    let flush = |cur: &mut String, out: &mut Vec<f32>| {
        if !cur.is_empty() {
            out.push(cur.parse().expect("a path number"));
            cur.clear();
        }
    };
    for ch in s.chars() {
        match ch {
            '-' | '+' if !cur.ends_with(['e', 'E']) => {
                flush(&mut cur, &mut out);
                cur.push(ch);
            }
            '.' if cur.contains('.') => {
                flush(&mut cur, &mut out);
                cur.push(ch);
            }
            ',' | ' ' | '\n' | '\t' => flush(&mut cur, &mut out),
            _ => cur.push(ch),
        }
    }
    flush(&mut cur, &mut out);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn numbers_read_svg_compact_lists() {
        assert_eq!(numbers("1.5.5-2,3e-1 4"), vec![1.5, 0.5, -2.0, 0.3, 4.0]);
    }

    #[test]
    fn absolute_commands_move_and_relative_ones_do_not() {
        assert_eq!(translate("M1,2c.5.5,1,1,2,2h-1ZL3 4", 10.0, 20.0), "M11 22c.5.5,1,1,2,2h-1ZL13 24");
        assert_eq!(translate("H1V2", 10.0, 20.0), "H11V22");
    }

    #[test]
    fn the_mark_is_every_path_of_the_brand_svg_on_a_square() {
        let m = mark();
        assert_eq!(m.body.matches("<path").count(), SVG.matches("<path ").count());
        assert!(m.body.contains("fill-rule=\"evenodd\""), "the outline keeps its even-odd rule: {}", m.body);
        // The mark's points land inside the square view box.
        let first = m.body.split("d=\"M").nth(1).unwrap();
        let xy = numbers(&first[..first.find(|c: char| c.is_ascii_alphabetic()).unwrap()]);
        assert!(xy.iter().all(|&v| (0.0..=m.view).contains(&v)), "{xy:?} outside 0..{}", m.view);
        // And something is drawn.
        assert!(rune::text::icon_coverage(m, 64).iter().any(|&c| c > 128));
    }
}
