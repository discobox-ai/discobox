//! Keys, the desktop's way: the spec's commands carry `desktop` shortcuts, where `primary` is
//! Cmd on macOS and Ctrl+Shift elsewhere (Ctrl belongs to the terminal there, as in GNOME
//! Terminal and Windows Terminal). There is no leader. A key no shortcut takes goes to the
//! list while it has focus, and otherwise to the focused pane, as the bytes an xterm sends.
//!
//! Every shortcut and list key is the spec's (ADR 26-11-07-598 §2–3): a registry command's, or
//! a gesture's — moving the list's cursor, a tab by its number — which the app acts on itself.
//! An open menu or palette keeps its own keys (`overlay.rs`). Where focus
//! goes is the app's (ADR 26-11-07-876 §2); which key moves it is the registry's.

use rune::{Key, KeyEvent, Shortcut, Tx};

use crate::wire::Spec;
use crate::{Focus, State, overlay, run_command};

pub const MAC: bool = cfg!(target_os = "macos");

/// A key's name as the spec spells it: `t`, `enter`, `tab`, `up`, `f2`, `backslash`.
fn key_name(ev: &KeyEvent) -> Option<String> {
    Some(match &ev.key {
        Key::Command(' ') => "space".into(),
        Key::Command(c) => c.to_lowercase().to_string(),
        Key::Text(t) if t == " " => "space".into(),
        Key::Text(t) if t.chars().count() == 1 => t.to_lowercase(),
        Key::Enter => "enter".into(),
        Key::Tab => "tab".into(),
        Key::Escape => "escape".into(),
        Key::Up => "up".into(),
        Key::Down => "down".into(),
        Key::Left => "left".into(),
        Key::Right => "right".into(),
        Key::Home => "home".into(),
        Key::End => "end".into(),
        Key::ContextMenu => "contextmenu".into(),
        Key::Function(n) => format!("f{n}"),
        _ => return None,
    })
    .map(|k| match k.as_str() {
        "\\" => "backslash".into(),
        // Shift turns a digit into its symbol on a US layout; the shortcut names the digit.
        ")" => "0".into(),
        _ => k,
    })
}

/// Whether a spec shortcut (`primary+shift+t`, `ctrl+tab`, `f2`) is this key press.
pub fn matches(binding: &str, ev: &KeyEvent) -> bool {
    let parts: Vec<&str> = binding.split('+').collect();
    let (key, mods) = parts.split_last().unwrap();
    let (mut ctrl, mut shift, mut alt, mut cmd) = (false, false, false, false);
    for m in mods {
        match *m {
            "primary" if MAC => cmd = true,
            "primary" => (ctrl, shift) = (true, true),
            "ctrl" => ctrl = true,
            "shift" => shift = true,
            "alt" => alt = true,
            _ => return false,
        }
    }
    let m = ev.mods;
    // On macOS `cmd` is Command and `ctrl` Control; elsewhere `cmd` mirrors Control.
    let (has_ctrl, has_cmd) = if MAC { (m.ctrl, m.cmd) } else { (m.ctrl, false) };
    // Shift is part of a letter's case; a shortcut without shift is matched only without it.
    has_ctrl == ctrl && has_cmd == cmd && m.alt == alt && m.shift == shift && key_name(ev).as_deref() == Some(key)
}

/// A command's desktop shortcuts, from the spec.
pub fn bindings(spec: &Spec, id: &str) -> Vec<String> {
    spec.command(id).map(|c| c.bindings.desktop.keys.clone()).unwrap_or_default()
}

/// What a command is called: the registry's label.
pub fn label_of(spec: &Spec, id: &str) -> Option<String> { spec.command(id).map(|c| c.label.clone()) }

/// The command a key press runs, if any.
fn command_for(s: &State, ev: &KeyEvent) -> Option<String> {
    s.spec.commands.iter().find(|c| c.bindings.desktop.keys.iter().any(|b| matches(b, ev))).map(|c| c.id.clone())
}

/// The gesture a key press is, if any, and the binding it matched: the spec's gestures, never a
/// key written here.
fn gesture_for<'a>(s: &'a State, ev: &KeyEvent) -> Option<(&'a str, &'a str)> {
    s.spec.gestures.iter().find_map(|g| {
        g.bindings.desktop.keys.iter().find(|b| matches(b, ev)).map(|b| (g.id.as_str(), b.as_str()))
    })
}

/// A shortcut as the platform writes it: `⌘⇧T`, or `Ctrl+Shift+T`.
pub fn label(binding: &str) -> String {
    let parts: Vec<&str> = binding.split('+').collect();
    let (key, mods) = parts.split_last().unwrap();
    let key = match *key {
        "enter" => if MAC { "↩".into() } else { "Enter".into() },
        "tab" => "Tab".into(),
        "up" => "↑".into(),
        "space" => if MAC { "␣".into() } else { "Space".into() },
        "escape" => "Esc".into(),
        "down" => "↓".into(),
        "left" => "←".into(),
        "right" => "→".into(),
        "backslash" => "\\".into(),
        k => k.to_uppercase(),
    };
    if MAC {
        let mut s = String::new();
        for m in mods {
            s.push_str(match *m { "primary" => "⌘", "ctrl" => "⌃", "shift" => "⇧", "alt" => "⌥", _ => "" });
        }
        s + &key
    } else {
        let mut v: Vec<&str> = vec![];
        for m in mods {
            match *m {
                "primary" => v.extend(["Ctrl", "Shift"]),
                "ctrl" => v.push("Ctrl"),
                "shift" => v.push("Shift"),
                "alt" => v.push("Alt"),
                _ => {}
            }
        }
        v.dedup();
        v.push(&key);
        v.join("+")
    }
}

/// The first desktop shortcut of a command, labelled.
pub fn shortcut_of(s: &State, id: &str) -> Option<String> { bindings(&s.spec, id).first().map(|b| label(b)) }

/// A spec shortcut as a native menu's key equivalent (macOS: `cmd+t`).
pub fn menu_shortcut(binding: &str) -> Option<Shortcut> {
    if !binding.starts_with("primary+") {
        return None;
    }
    binding.replacen("primary", "cmd", 1).replace("enter", "return").parse().ok()
}

pub fn on_key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool {
    if matches!(ev.key, Key::Modifier { .. }) {
        return false;
    }
    if s.menu.is_some() {
        return overlay::menu_key(s, ev, tx);
    }
    if s.filter_menu.is_some() {
        return overlay::filter_key(s, ev, tx);
    }
    // The palette's field takes its keys first; these are the ones that reach the window while
    // something else in the palette has the focus.
    if s.palette.is_some() {
        overlay::palette_key(s, ev, tx);
        return true;
    }
    if s.help {
        if ev.key == Key::Escape || command_for(s, ev).as_deref() == Some("help") {
            s.help = false;
        }
        return true;
    }
    // A tab by its number (the pane gesture): ⌘1 is the first. The binding's digit is its number.
    if let Some(("pane", binding)) = gesture_for(s, ev)
        && let Some(n) = binding.chars().last().and_then(|c| c.to_digit(10))
    {
        s.goto_pane(n.saturating_sub(1) as usize);
        return true;
    }
    if let Some(id) = command_for(s, ev) {
        // A shortcut with no modifier (Enter attaches, F2 renames) is the list's; in a pane the
        // key is the program's.
        let in_pane = s.focus == Focus::Panes && s.live_ws().is_some_and(|w| !w.panes.is_empty());
        let bare = !(ev.mods.ctrl || ev.mods.alt || ev.mods.shift || ev.mods.cmd);
        if !(bare && in_pane) {
            run_command(s, &id, tx);
            return true;
        }
    }
    if s.live.is_none() || s.focus == Focus::List {
        return list_key(s, ev, tx);
    }
    if let Some(bytes) = encode(s, ev) {
        s.send(&bytes);
    }
    true
}

/// The list's keys: the spec's gestures. Enter is attach's own shortcut, which runs before this.
fn list_key(s: &mut State, ev: &KeyEvent, _tx: &mut Tx) -> bool {
    let Some((gesture, _)) = gesture_for(s, ev) else { return false };
    let last = s.rows().len().saturating_sub(1);
    match gesture {
        "up" => s.move_cursor(s.cursor.saturating_sub(1)),
        "down" => s.move_cursor(s.cursor + 1),
        "first" => s.move_cursor(0),
        "last" => s.move_cursor(last),
        "select" => s.toggle_select(),
        "range" => s.toggle_range(),
        "clear selection" => s.clear_selection(),
        // Back lets a range being drawn go first, as the TUI's Esc does; then to the panes.
        "back" if s.range.is_some() => s.range = None,
        "back" if s.live.is_some() => s.focus = Focus::Panes,
        // Off macOS the menu bar is the window's; F10 opens its first menu, as GNOME's apps do.
        "menu bar" if !MAC => {
            if let Some(&first) = s.spec.menus.first() {
                overlay::open_bar(s, first);
            }
        }
        "context menu" => {
            if let Some(r) = s.rows().get(s.cursor) {
                let id = r.r#box.clone();
                s.open_menu(id, (260.0, 120.0 + s.cursor as f32 * 28.0));
            }
        }
        _ => return false,
    }
    true
}

/// A key as the bytes an xterm sends for it.
fn encode(s: &State, ev: &KeyEvent) -> Option<Vec<u8>> {
    let app = s.live_ws().and_then(|w| w.focused().map(|i| w.panes[i].screen.app_cursor())).unwrap_or(false);
    let m = 1 + ev.mods.shift as u8 + 2 * ev.mods.alt as u8 + 4 * ev.mods.ctrl as u8;
    let csi = |c: &str| -> Vec<u8> {
        if m > 1 {
            format!("\x1b[1;{m}{c}").into_bytes()
        } else if app {
            format!("\x1bO{c}").into_bytes()
        } else {
            format!("\x1b[{c}").into_bytes()
        }
    };
    let tilde = |n: u8| format!("\x1b[{n}~").into_bytes();
    let mut out = match &ev.key {
        Key::Enter => vec![b'\r'],
        Key::Backspace => vec![0x7f],
        Key::Tab if ev.mods.shift => b"\x1b[Z".to_vec(),
        Key::Tab => vec![b'\t'],
        Key::Escape => vec![0x1b],
        Key::Up => csi("A"),
        Key::Down => csi("B"),
        Key::Right => csi("C"),
        Key::Left => csi("D"),
        Key::Home => csi("H"),
        Key::End => csi("F"),
        Key::Insert => tilde(2),
        Key::Delete => tilde(3),
        Key::PageUp => tilde(5),
        Key::PageDown => tilde(6),
        Key::Function(n) => match n {
            1..=4 => format!("\x1bO{}", ['P', 'Q', 'R', 'S'][*n as usize - 1]).into_bytes(),
            5 => tilde(15),
            6..=10 => tilde(11 + n),
            11 | 12 => tilde(12 + n),
            _ => return None,
        },
        Key::Command(c) if ev.mods.ctrl => ctrl_byte(*c)?,
        Key::Command(_) => return None, // ⌘ chords are the app's, never the program's
        Key::Text(t) if ev.mods.ctrl && t.chars().count() == 1 => ctrl_byte(t.chars().next()?)?,
        Key::Text(t) => t.as_bytes().to_vec(),
        _ => return None,
    };
    if ev.mods.alt && !matches!(ev.key, Key::Up | Key::Down | Key::Left | Key::Right | Key::Home | Key::End) {
        out.insert(0, 0x1b);
    }
    Some(out)
}

fn ctrl_byte(c: char) -> Option<Vec<u8>> {
    Some(match c.to_ascii_lowercase() {
        c @ 'a'..='z' => vec![c as u8 - b'a' + 1],
        ' ' | '2' | '@' => vec![0],
        '[' | '3' => vec![0x1b],
        '\\' | '4' => vec![0x1c],
        ']' | '5' => vec![0x1d],
        '/' | '7' | '_' => vec![0x1f],
        _ => return None,
    })
}
