//! Keys, the desktop's way: the spec's commands carry `desktop` shortcuts, where `primary` is
//! Cmd on macOS and Ctrl+Shift elsewhere (Ctrl belongs to the terminal there, as in GNOME
//! Terminal and Windows Terminal). There is no leader. A key no shortcut takes goes to the
//! list while it has focus, and otherwise to the focused pane, as the bytes an xterm sends.

use rune::{Key, KeyEvent, Shortcut, Tx};

use crate::{Focus, State, overlay, run_command};

const MAC: bool = cfg!(target_os = "macos");

/// A key's name as the spec spells it: `t`, `enter`, `tab`, `up`, `f2`, `backslash`.
fn key_name(ev: &KeyEvent) -> Option<String> {
    Some(match &ev.key {
        Key::Command(c) => c.to_lowercase().to_string(),
        Key::Text(t) if t.chars().count() == 1 => t.to_lowercase(),
        Key::Enter => "enter".into(),
        Key::Tab => "tab".into(),
        Key::Escape => "escape".into(),
        Key::Up => "up".into(),
        Key::Down => "down".into(),
        Key::Left => "left".into(),
        Key::Right => "right".into(),
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

/// The command a key press runs, if any: the spec's shortcuts, then ⌘1–9 for tabs.
fn command_for(s: &State, ev: &KeyEvent) -> Option<String> {
    for c in &s.spec.commands {
        if c.keys.get("desktop").into_iter().flatten().any(|b| matches(b, ev)) {
            return Some(c.id.clone());
        }
    }
    for n in 1..=9 {
        if matches(&format!("primary+{n}"), ev) {
            return Some(format!("pane.goto.{}", n - 1));
        }
    }
    None
}

/// A shortcut as the platform writes it: `⌘⇧T`, or `Ctrl+Shift+T`.
pub fn label(binding: &str) -> String {
    let parts: Vec<&str> = binding.split('+').collect();
    let (key, mods) = parts.split_last().unwrap();
    let key = match *key {
        "enter" => if MAC { "↩".into() } else { "Enter".into() },
        "tab" => "Tab".into(),
        "up" => "↑".into(),
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
pub fn shortcut_of(s: &State, id: &str) -> Option<String> {
    s.spec.command(id).and_then(|c| c.keys.get("desktop")).and_then(|k| k.first()).map(|b| label(b))
}

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
    if s.palette.is_some() {
        if ev.key == Key::Escape {
            s.palette = None;
        }
        return true;
    }
    if let Some(id) = command_for(s, ev) {
        // Enter attaches from the list; in a pane it is the program's.
        let in_pane = s.focus == Focus::Panes && s.live_ws().is_some_and(|w| !w.panes.is_empty());
        if !(id == "box.attach" && in_pane) {
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

fn list_key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool {
    match &ev.key {
        Key::Up => s.cursor = s.cursor.saturating_sub(1),
        Key::Down => s.cursor = (s.cursor + 1).min(s.boxes.len().saturating_sub(1)),
        Key::Home => s.cursor = 0,
        Key::End => s.cursor = s.boxes.len().saturating_sub(1),
        Key::Enter => run_command(s, "box.attach", tx),
        Key::Escape | Key::Right if s.live.is_some() => s.focus = Focus::Panes,
        Key::ContextMenu => {
            if let Some(b) = s.boxes.get(s.cursor) {
                let id = b.id.clone();
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
