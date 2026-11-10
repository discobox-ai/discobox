//! The desktop's identity (ADR 26-11-07-363): drawn natively, in the design language's tokens and
//! JetBrains Mono, with nothing of the TUI's character art left.

use crate::{backend, theme};

/// The window's own drawing: everything that lays out chrome.
const CHROME: &[(&str, &str)] = &[
    ("main.rs", include_str!("main.rs")),
    ("view.rs", include_str!("view.rs")),
    ("overlay.rs", include_str!("overlay.rs")),
    ("sheet.rs", include_str!("sheet.rs")),
    ("icons.rs", include_str!("icons.rs")),
    ("mark.rs", include_str!("mark.rs")),
    ("theme.rs", include_str!("theme.rs")),
];

/// Box drawing (U+2500–U+257F) and block elements (U+2580–U+259F): what a terminal draws frames
/// and the mark with.
fn terminal_art(c: char) -> bool { ('\u{2500}'..='\u{259f}').contains(&c) }

#[test]
fn no_tui_character_imitation_remains() {
    for (file, src) in CHROME {
        for (n, line) in src.lines().enumerate() {
            let at = format!("{file}:{}: {line}", n + 1);
            assert!(!line.chars().any(terminal_art), "box drawing or a block element, which the window draws as vectors: {at}");
            // A bracket typed around a label (`[ name ]`, `[+]`) opens or closes a string.
            assert!(!line.contains("\"[") && !line.contains("]\""), "a TUI bracket typed as text, where the window draws a chip or an icon button: {at}");
        }
    }
}

#[test]
fn the_specs_state_marks_are_glyphs_not_terminal_art() {
    let spec = backend::bundled_spec();
    for (name, g) in spec.states.iter().chain(spec.agents.iter()) {
        assert!(!g.glyph.chars().any(terminal_art), "{name}'s mark {:?} is terminal art", g.glyph);
    }
}

/// Every `<call>("name")` in `src`: the token names the chrome asks for.
fn named<'a>(src: &'a str, call: &str) -> Vec<&'a str> {
    src.split(&format!("{call}(\"")).skip(1).filter_map(|rest| rest.split('"').next()).collect()
}

#[test]
fn every_colour_and_length_the_chrome_names_is_a_token() {
    let spec = backend::bundled_spec();
    let theme = CHROME.iter().find(|(f, _)| *f == "theme.rs").unwrap().1;
    let (colours, lengths) = (named(theme, "token"), named(theme, "length"));
    assert!(colours.len() > 10 && lengths.len() >= 6, "the theme names its tokens: {colours:?} {lengths:?}");
    for name in colours {
        let c = spec.colors.get(name).unwrap_or_else(|| panic!("{name} is not a colour token in the spec"));
        for v in [&c.dark, &c.light] {
            assert!(v.starts_with('#') && u32::from_str_radix(&v[1..], 16).is_ok(), "{name}: {v:?} is not a colour");
        }
    }
    for name in lengths {
        assert!(spec.dimensions.contains_key(name), "{name} is not a length token in the spec");
    }
    // So is every colour the spec's marks are drawn in.
    for g in spec.states.values().chain(spec.agents.values()) {
        assert!(g.color.is_empty() || spec.colors.contains_key(&g.color), "{} is not a token", g.color);
    }
}

/// The chrome adds no colour the tokens do not name (ADR 26-11-07-363 §4): only the theme turns
/// a value into a colour, and outside it a colour is a token's, transparent, or a floating
/// panel's shadow.
#[test]
fn the_chrome_draws_no_colour_of_its_own() {
    for (file, src) in CHROME.iter().filter(|(f, _)| *f != "theme.rs") {
        for (n, line) in src.lines().enumerate() {
            let at = format!("{file}:{}: {line}", n + 1);
            assert!(!line.contains("hex(") && !line.contains("Color {") && !line.contains("oklch"), "a colour written out, not a token: {at}");
            let consts = line.matches("Color::").count();
            let allowed = line.matches("Color::TRANSPARENT").count() + if line.contains(".shadow(") { line.matches("Color::BLACK").count() } else { 0 };
            assert_eq!(consts, allowed, "a colour the tokens do not name: {at}");
        }
    }
}

#[test]
fn the_window_follows_the_appearance() {
    theme::set_tokens(&backend::bundled_spec());
    theme::set_dark(true);
    let (dark_bg, dark_mark) = (theme::bg(), theme::mark());
    theme::set_dark(false);
    let (light_bg, light_mark) = (theme::bg(), theme::mark());
    theme::set_dark(true);
    assert_eq!(dark_bg, theme::hex(0x0a0410));
    assert_eq!(light_bg, theme::hex(0xf7f4fa));
    // The accent deepens on light ground, for contrast.
    assert_eq!((dark_mark, light_mark), (theme::hex(0xf45cff), theme::hex(0xa81fbd)));
}

#[test]
fn chrome_and_terminals_are_set_in_the_bundled_jetbrains_mono() {
    let _text = theme::text_system();
    // JetBrains Mono's advance is 0.6 em; Inter, which the faces replace, is proportional.
    let (m, i) = (rune::text::advance("MMMMMMMMMM", theme::TERM) / 10.0, rune::text::advance("iiiiiiiiii", theme::TERM) / 10.0);
    assert!((m - 0.6 * theme::TERM_SIZE).abs() < 0.05, "a cell is {m} pt");
    assert!((m - i).abs() < 0.01, "monospaced: M is {m} pt, i is {i} pt");
    let chrome = rune::text::advance("discobox", theme::UI);
    assert!((chrome - 8.0 * 0.6 * theme::UI.size).abs() < 0.2, "the chrome's face is the terminals': {chrome} pt");
    assert!(include_str!("../fonts/OFL.txt").contains("SIL Open Font License, Version 1.1"));
}
