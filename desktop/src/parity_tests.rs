//! The parity scenarios in `cli/internal/console/testdata/parity`, run here as the TUI runs
//! them (`cli/internal/tui/parity_test.go`): each step is a registry command or gesture, pressed
//! the desktop's way — its shortcut from the spec, or for a box's lifecycle its row's menu — and
//! the workspace or the list has to look the same afterwards.

use rune::{Env, Key, KeyEvent, Mods, Runtime};
use serde::Deserialize;

use crate::backend::{Inbox, Mock, MockService, bundled_spec};
use crate::wire::SpecTarget;
use crate::{MenuKind, Side, State, on_key, poll, theme, view};

#[derive(Deserialize)]
struct Scenario {
    #[serde(default)]
    attach: bool,
    /// What the first box declares, each with what it is doing and what its runs have written.
    #[serde(default)]
    services: Vec<ServiceDecl>,
    steps: Vec<Step>,
}

#[derive(Deserialize)]
struct ServiceDecl {
    id: String,
    status: String,
    #[serde(default)]
    logs: String,
}

#[derive(Deserialize, Clone)]
struct Step {
    #[serde(default)]
    command: String,
    #[serde(default)]
    gesture: String,
    /// A service step: the service, and its verb or its output, from the services menu.
    #[serde(default)]
    service: String,
    #[serde(default)]
    verb: String,
    /// What is written in the composer before the command: new creates with it.
    #[serde(default)]
    prompt: String,
    /// The command is reached through the palette: opened with its shortcut, found by its label,
    /// and chosen.
    #[serde(default)]
    palette: bool,
    expect: Expect,
}

/// What a step expects; a field it leaves out is not checked.
#[derive(Deserialize, Debug, PartialEq, Default, Clone)]
struct Expect {
    r#box: Option<i64>,
    terminals: Option<usize>,
    shells: Option<usize>,
    focus: Option<String>,
    maximized: Option<bool>,
    targets: Option<usize>,
    selected: Option<usize>,
    archived: Option<bool>,
    services: Option<usize>,
    output: Option<String>,
    /// The name of the box on screen when a create made it.
    created: Option<String>,
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

fn snapshot(s: &State, want: &Expect, made: &[String]) -> Expect {
    let ws = s.live_ws();
    Expect {
        // The row the box on screen is on, -1 with none.
        r#box: want.r#box.map(|_| s.live.as_ref().and_then(|k| s.row_of(k)).map_or(-1, |i| i as i64)),
        terminals: want.terminals.and(ws.map(|w| w.column(Side::Terminals).len())),
        shells: want.shells.and(ws.map(|w| w.column(Side::Shells).len())),
        focus: want.focus.as_ref().and(ws.map(|w| if w.focus == Side::Shells { "shells" } else { "terminals" }.into())),
        maximized: want.maximized.and(ws.map(|w| w.maximized)),
        targets: want.targets.map(|_| s.list_targets().len()),
        selected: want.selected.map(|_| (0..s.rows().len()).filter(|&i| s.rows().get(i).is_some_and(|r| s.selected.contains(&r.r#box))).count()),
        archived: want.archived.map(|_| s.listing.filter.archived),
        services: want.services.and(ws.map(|w| w.services().len())),
        // What the pane with the keys shows, read as the step expects it: there, or not.
        output: want.output.as_ref().and_then(|o| {
            let shown = ws.and_then(|w| w.focused()).map(|i| ws.unwrap().panes[i].screen.text()).unwrap_or_default();
            Some(if shown.contains(o.as_str()) { o.clone() } else { shown })
        }),
        // The box on screen, when it is the one the last create made: the mock names a box it
        // makes after its prompt.
        created: want.created.as_ref().map(|_| {
            let live = s.live.as_ref().and_then(|k| s.box_by_id(k)).map(|b| b.name.clone()).unwrap_or_default();
            if made.last() == Some(&live) { live } else { String::new() }
        }),
    }
}

/// Moves an open menu's pick to the entry for `id` with its keys, and chooses it.
fn choose_in_menu(rt: &mut Runtime<State>, id: &str, path: &std::path::Path, i: usize) {
    let entries = rt.state.menu.as_ref().map(|m| m.entries.clone()).unwrap_or_else(|| panic!("{}: step {i}: no menu opened", path.display()));
    let at = entries.iter().position(|a| a.command == id).unwrap_or_else(|| panic!("{}: step {i}: {id} is not on the menu", path.display()));
    for _ in 0..entries.len() {
        if rt.state.menu.as_ref().is_none_or(|m| m.picked == at) {
            break;
        }
        rt.key(KeyEvent { key: Key::Down, mods: Mods::default() });
        rt.frame(1.0 / 60.0);
    }
    assert_eq!(rt.state.menu.as_ref().map(|m| m.picked), Some(at), "{}: step {i}: the menu's keys did not reach {id}", path.display());
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(rt);
}

/// Runs a service step the desktop's way: the services command's shortcut, then the service in
/// its menu, then the verb (or its output) in the service's.
fn run_service_step(rt: &mut Runtime<State>, step: &Step, path: &std::path::Path, i: usize) -> String {
    let binding = rt.state.spec.command("services").and_then(|c| c.bindings.desktop.keys.first().cloned())
        .expect("services has a desktop shortcut");
    rt.key(press(&binding));
    settle(rt);
    assert!(matches!(rt.state.menu.as_ref().map(|m| &m.kind), Some(MenuKind::Services(..))), "{}: step {i}: the services menu did not open", path.display());
    choose_in_menu(rt, &step.service, path, i);
    assert!(matches!(rt.state.menu.as_ref().map(|m| &m.kind), Some(MenuKind::Service(..))), "{}: step {i}: {}'s menu did not open", path.display(), step.service);
    choose_in_menu(rt, &step.verb, path, i);
    format!("{binding} and the menus")
}

/// Runs a palette step the desktop's way: the palette's shortcut, the command's label typed, and
/// Enter; a box's command, which Enter makes the chip, Enter again on the box picked first, the
/// one the window is on.
fn run_palette_step(rt: &mut Runtime<State>, step: &Step, path: &std::path::Path, i: usize) -> String {
    let palette = rt.state.spec.command("palette").cloned().expect("the palette is a command");
    let binding = palette.bindings.desktop.keys.first().cloned().expect("the palette has a desktop shortcut");
    rt.key(press(&binding));
    settle(rt);
    assert!(rt.state.palette.is_some(), "{}: step {i}: {binding} opened no palette", path.display());
    let command = rt.state.spec.command(&step.command).cloned().unwrap_or_else(|| panic!("{}: step {i}: no command {}", path.display(), step.command));
    for c in command.label.chars() {
        rt.key(KeyEvent { key: Key::Text(c.to_string()), mods: Mods::default() });
        rt.frame(1.0 / 60.0);
    }
    settle(rt);
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(rt);
    if command.target == SpecTarget::Box {
        assert_eq!(rt.state.palette.as_ref().map(|p| p.command.as_str()), Some(step.command.as_str()), "{}: step {i}: Enter did not make {} the chip", path.display(), step.command);
        rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
        settle(rt);
    }
    assert!(rt.state.palette.is_none(), "{}: step {i}: the palette is still open after {}", path.display(), step.command);
    format!("{binding}, {:?} and Enter", command.label)
}

/// Runs one step the desktop's way: a command's or a gesture's shortcut from the spec; a box's
/// lifecycle command with none, through its row's menu — the context menu key, then the entry.
fn run_step(rt: &mut Runtime<State>, step: &Step, path: &std::path::Path, i: usize) -> String {
    if step.palette {
        return run_palette_step(rt, step, path, i);
    }
    // A prompt is written in the composer, beside the list, before the command; and the create
    // card the command opens is accepted as it opens.
    if !step.prompt.is_empty() {
        rt.update(|s, _| s.composer.focused = true);
        settle(rt);
        for c in step.prompt.chars() {
            rt.key(KeyEvent { key: Key::Text(c.to_string()), mods: Mods::default() });
            rt.frame(1.0 / 60.0);
        }
        let how = run_step(rt, &Step { prompt: String::new(), ..step.clone() }, path, i);
        assert!(rt.state.sheets.form().is_some(), "{}: step {i}: {} opened no create card", path.display(), step.command);
        rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
        settle(rt);
        return format!("{how}, and Enter on the card");
    }
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
    let actions = rt.state.menu.as_ref().map(|m| m.entries.clone()).unwrap_or_else(|| panic!("{}: step {i}: no row menu opened", path.display()));
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
        let inbox = Inbox::default();
        let services = sc.services.iter().map(|v| MockService::new(&v.id, &v.status, &v.logs)).collect();
        let mock = Mock::with_services(inbox.clone(), services);
        let record = mock.record();
        let state = State::with((Box::new(mock), bundled_spec(), inbox, false, String::new()));
        let mut rt = Runtime::with_text(state, view::view, (1440.0, 900.0), Env { system_dark: true, scale: 1.0 }, theme::text_system());
        rt.poll = Some(poll);
        rt.on_key = Some(on_key);
        settle(&mut rt);
        if sc.attach {
            rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() }); // attach, as the TUI's scenario does
            settle(&mut rt);
        }
        for (i, step) in sc.steps.iter().enumerate() {
            let how = if step.service.is_empty() { run_step(&mut rt, step, &path, i) } else { run_service_step(&mut rt, step, &path, i) };
            let id = if !step.service.is_empty() { &format!("{} {}", step.verb, step.service) } else if step.gesture.is_empty() { &step.command } else { &step.gesture };
            let made = record.lock().unwrap().created.clone();
            assert_eq!(snapshot(&rt.state, &step.expect, &made), step.expect, "{}: step {i} ({id} as {how})", path.display());
        }
        ran += 1;
    }
    assert!(ran > 0, "no scenarios in {dir}");
}
