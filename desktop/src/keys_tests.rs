use super::*;
use rune::Mods;

#[test]
fn numbered_shortcuts_use_the_physical_number_row() {
    use PhysicalKeyCode::*;
    let codes = [Digit1, Digit2, Digit3, Digit4, Digit5, Digit6, Digit7, Digit8, Digit9];
    // Logical characters from shifted US, German and French number rows. The same
    // physical positions must select the same tabs, including with Alt held for services.
    for layout in ["!@#$%^&*(", "!\"§$%&/()", "123456789"] {
        for (i, c) in layout.chars().enumerate() {
            for alt in [false, true] {
                let mods = Mods { ctrl: !MAC, shift: !MAC, cmd: true, alt, ..Default::default() };
                let ev = KeyEvent { key: Key::Command(if MAC { char::from(b'1' + i as u8) } else { c }), mods };
                let binding = format!("primary+{}{n}", if alt { "alt+" } else { "" }, n = i + 1);
                assert!(matches(&binding, &ev, Some(codes[i])), "{layout}: {binding}");
                assert!(!matches(&binding, &ev, Some(codes[(i + 1) % codes.len()])));
                assert!(!matches(&binding, &ev, Some(KeyA)));
                let wrong_alt = KeyEvent { mods: Mods { alt: !alt, ..mods }, key: ev.key.clone() };
                assert!(!matches(&binding, &wrong_alt, Some(codes[i])));
                let no_primary = KeyEvent { mods: Mods { ctrl: false, cmd: false, ..mods }, key: ev.key.clone() };
                assert!(!matches(&binding, &no_primary, Some(codes[i])));
            }
        }
    }
}

#[test]
fn letters_still_follow_the_logical_key() {
    let mods = Mods { ctrl: !MAC, shift: !MAC, cmd: true, ..Default::default() };
    let ev = KeyEvent { key: Key::Command('t'), mods };
    assert!(matches("primary+t", &ev, Some(PhysicalKeyCode::KeyY)));
    assert!(!matches("primary+y", &ev, Some(PhysicalKeyCode::KeyY)));
}
