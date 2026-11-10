//! The parity scenarios in `cli/internal/console/testdata/parity`, run here as the TUI runs
//! them (`cli/internal/tui/parity_test.go`): each step is a registry command or gesture, pressed
//! the desktop's way — its shortcut from the spec, or for a box's lifecycle its row's menu — and
//! the workspace or the list has to look the same afterwards.

use rune::{Env, Key, KeyEvent, Mods, Runtime};
use serde::Deserialize;

use crate::{Side, State, on_key, poll, theme, view};

#[derive(Deserialize)]
struct Scenario {
    #[serde(default)]
    attach: bool,
    steps: Vec<Step>,
}

#[derive(Deserialize)]
struct Step {
    #[serde(default)]
    command: String,
    #[serde(default)]
    gesture: String,
    expect: Expect,
}

/// What a step expects; a field it leaves out is not checked.
#[derive(Deserialize, Debug, PartialEq, Default)]
struct Expect {
    shells: Option<usize>,
    focus: Option<String>,
    maximized: Option<bool>,
    targets: Option<usize>,
    selected: Option<usize>,
    archived: Option<bool>,
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
        "home" => Key::Home,
        "end" => Key::End,
        "escape" => Key::Escape,
        "contextmenu" => Key::ContextMenu,
        "space" => Key::Text(" ".into()),
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

fn snapshot(s: &State, want: &Expect) -> Expect {
    let ws = s.live_ws();
    Expect {
        shells: want.shells.and(ws.map(|w| w.column(Side::Shells).len())),
        focus: want.focus.as_ref().and(ws.map(|w| if w.focus == Side::Shells { "shells" } else { "terminals" }.into())),
        maximized: want.maximized.and(ws.map(|w| w.maximized)),
        targets: want.targets.map(|_| s.list_targets().len()),
        selected: want.selected.map(|_| (0..s.rows().len()).filter(|&i| s.rows().get(i).is_some_and(|r| s.selected.contains(&r.r#box))).count()),
        archived: want.archived.map(|_| s.listing.filter.archived),
    }
}

/// Runs one step the desktop's way: a command's or a gesture's shortcut from the spec; a box's
/// lifecycle command with none, through its row's menu — the context menu key, then the entry.
fn run_step(rt: &mut Runtime<State>, step: &Step, path: &std::path::Path, i: usize) -> String {
    let keys = if step.gesture.is_empty() {
        rt.state.spec.command(&step.command).map(|c| c.bindings.desktop.keys.clone())
    } else {
        rt.state.spec.gestures.iter().find(|g| g.id == step.gesture).map(|g| g.bindings.desktop.keys.clone())
    }
    .unwrap_or_default();
    if let Some(binding) = keys.first() {
        rt.key(press(binding));
        settle(rt);
        return binding.clone();
    }
    let menu = rt.state.spec.gestures.iter().find(|g| g.id == "context menu").and_then(|g| g.bindings.desktop.keys.first().cloned())
        .expect("the context menu gesture has a desktop key");
    rt.key(press(&menu));
    settle(rt);
    let actions = rt.state.menu.as_ref().map(|m| m.actions.clone()).unwrap_or_else(|| panic!("{}: step {i}: no row menu opened", path.display()));
    let at = actions.iter().position(|a| a.command == step.command).unwrap_or_else(|| panic!("{}: step {i}: {} is not on the row's menu", path.display(), step.command));
    for _ in 0..actions.len() {
        if rt.state.menu.as_ref().is_none_or(|m| m.picked == at) {
            break;
        }
        rt.key(KeyEvent { key: Key::Down, mods: Mods::default() });
        rt.frame(1.0 / 60.0);
    }
    assert_eq!(rt.state.menu.as_ref().map(|m| m.picked), Some(at), "{}: step {i}: the menu's keys did not reach {}", path.display(), step.command);
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(rt);
    format!("{menu} and the menu")
}

#[test]
fn parity_scenarios() {
    let dir = concat!(env!("CARGO_MANIFEST_DIR"), "/../cli/internal/console/testdata/parity");
    let mut ran = 0;
    for entry in std::fs::read_dir(dir).expect("the parity scenarios") {
        let path = entry.unwrap().path();
        let sc: Scenario = serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
        let mut rt = Runtime::with_text(State::new(), view::view, (1440.0, 900.0), Env { system_dark: true, scale: 1.0 }, theme::text_system());
        rt.poll = Some(poll);
        rt.on_key = Some(on_key);
        settle(&mut rt);
        if sc.attach {
            rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() }); // attach, as the TUI's scenario does
            settle(&mut rt);
        }
        for (i, step) in sc.steps.iter().enumerate() {
            let how = run_step(&mut rt, step, &path, i);
            let id = if step.gesture.is_empty() { &step.command } else { &step.gesture };
            assert_eq!(snapshot(&rt.state, &step.expect), step.expect, "{}: step {i} ({id} as {how})", path.display());
        }
        ran += 1;
    }
    assert!(ran > 0, "no scenarios in {dir}");
}
