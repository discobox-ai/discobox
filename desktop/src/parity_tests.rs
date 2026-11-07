//! The parity scenarios in `cli/internal/tui/testdata/parity`, run here as the TUI runs them
//! there (`parity_test.go`): each step is a command id, pressed with this app's desktop
//! shortcut from the spec, and the workspace has to look the same afterwards.

use rune::{Env, Key, KeyEvent, Mods, Runtime};
use serde::Deserialize;

use crate::{Side, State, on_key, poll, view};

#[derive(Deserialize)]
struct Scenario {
    steps: Vec<Step>,
}

#[derive(Deserialize)]
struct Step {
    command: String,
    expect: Expect,
}

#[derive(Deserialize, Debug, PartialEq)]
struct Expect {
    shells: usize,
    focus: String,
    maximized: bool,
}

/// A spec shortcut as the key event this platform delivers for it.
fn press(binding: &str) -> KeyEvent {
    let parts: Vec<&str> = binding.split('+').collect();
    let (key, mods) = parts.split_last().unwrap();
    let mut m = Mods::default();
    for k in mods {
        match *k {
            "primary" if cfg!(target_os = "macos") => m.cmd = true,
            "primary" => (m.ctrl, m.shift) = (true, true),
            "ctrl" => m.ctrl = true,
            "shift" => m.shift = true,
            "alt" => m.alt = true,
            _ => {}
        }
    }
    if !cfg!(target_os = "macos") && m.ctrl {
        m.cmd = true; // off macOS, cmd mirrors Ctrl
    }
    let key = match *key {
        "enter" => Key::Enter,
        "tab" => Key::Tab,
        "up" => Key::Up,
        "down" => Key::Down,
        k if k.chars().count() == 1 && (m.cmd || m.ctrl) => Key::Command(k.chars().next().unwrap()),
        k => Key::Text(k.into()),
    };
    KeyEvent { key, mods: m }
}

fn settle(rt: &mut Runtime<State>) {
    for _ in 0..20 {
        rt.frame(1.0 / 60.0);
    }
}

fn snapshot(s: &State) -> Expect {
    let ws = s.live_ws().expect("a workspace on screen");
    Expect {
        shells: ws.column(Side::Shells).len(),
        focus: if ws.focus == Side::Shells { "shells" } else { "terminals" }.into(),
        maximized: ws.maximized,
    }
}

#[test]
fn parity_scenarios() {
    let dir = concat!(env!("CARGO_MANIFEST_DIR"), "/../cli/internal/tui/testdata/parity");
    let mut ran = 0;
    for entry in std::fs::read_dir(dir).expect("the parity scenarios") {
        let path = entry.unwrap().path();
        let sc: Scenario = serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
        let mut rt = Runtime::new(State::new(), view::view, (1440.0, 900.0), Env { system_dark: true, scale: 1.0 });
        rt.poll = Some(poll);
        rt.on_key = Some(on_key);
        settle(&mut rt);
        rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() }); // attach, as the TUI's scenario does
        settle(&mut rt);
        for (i, step) in sc.steps.iter().enumerate() {
            let binding = rt.state.spec.command(&step.command)
                .and_then(|c| c.keys.get("desktop")).and_then(|k| k.first()).cloned()
                .unwrap_or_else(|| panic!("{} has no desktop key", step.command));
            rt.key(press(&binding));
            settle(&mut rt);
            assert_eq!(snapshot(&rt.state), step.expect, "{}: step {i} ({} as {binding})", path.display(), step.command);
        }
        ran += 1;
    }
    assert!(ran > 0, "no scenarios in {dir}");
}
