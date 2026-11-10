//! The command surfaces (ADR 26-11-07-598 §2): every registry command is in a menu and found by
//! the palette, the palette composes in both orders of ADR 26-11-06-671 §2, and the menu bar, a
//! pane's menu and the help are the registry's.

use rune::{Env, Key, KeyEvent, Mods, Runtime};

use crate::wire::State as BoxState;
use crate::wire::Menu as DesktopMenu;
use crate::{MenuKind, State, on_key, poll, theme, view};

fn runtime() -> Runtime<State> {
    let mut rt = Runtime::with_text(State::new(), view::view, (1440.0, 900.0), Env { system_dark: true, scale: 1.0 }, theme::text_system());
    rt.poll = Some(poll);
    rt.on_key = Some(on_key);
    settle(&mut rt);
    rt
}

fn settle(rt: &mut Runtime<State>) {
    for _ in 0..20 {
        rt.frame(1.0 / 60.0);
    }
}

fn press(rt: &mut Runtime<State>, key: Key, mods: Mods) {
    rt.key(KeyEvent { key, mods });
    settle(rt);
}

fn bare() -> Mods { Mods::default() }

/// Opens the palette with its shortcut from the spec.
fn open_palette(rt: &mut Runtime<State>) {
    let chord = rt.state.spec.command("palette").and_then(|c| c.bindings.desktop.keys.first().cloned()).expect("the palette's shortcut");
    let key = chord.rsplit('+').next().unwrap().chars().next().unwrap();
    let mods = if cfg!(target_os = "macos") { Mods { cmd: true, ..bare() } } else { Mods { ctrl: true, shift: true, cmd: true, ..bare() } };
    press(rt, Key::Command(key), mods);
    assert!(rt.state.palette.is_some(), "{chord} opened no palette");
}

fn typed(rt: &mut Runtime<State>, text: &str) {
    for c in text.chars() {
        press(rt, Key::Text(c.to_string()), bare());
    }
}

fn picked(rt: &Runtime<State>) -> String { rt.state.palette.as_ref().map(|p| p.picked.clone()).unwrap_or_default() }

// Every command is in one of the menus the menu bar draws, and the menus together list every one.
#[test]
fn every_command_is_in_a_menu() {
    let mut rt = runtime();
    let mut listed = vec![];
    let menus = rt.state.spec.menus.clone();
    for name in menus {
        rt.state.open_commands_menu(MenuKind::Bar(name), (0.0, 0.0));
        settle(&mut rt);
        let m = rt.state.menu.as_ref().unwrap_or_else(|| panic!("the {name:?} menu did not open"));
        assert!(!m.entries.is_empty(), "the {name:?} menu is empty");
        listed.extend(m.entries.iter().map(|e| e.command.clone()));
    }
    for c in &rt.state.spec.commands {
        assert!(c.bindings.desktop.menu.is_some_and(|m| rt.state.spec.menus.contains(&m)), "{} is in the menu {:?}, which the menu bar does not draw", c.id, c.bindings.desktop.menu);
        assert_eq!(listed.iter().filter(|id| **id == c.id).count(), 1, "{} is not in exactly one menu", c.id);
    }
}

// Every command is found by the palette by its name.
#[test]
fn every_command_is_found_in_the_palette() {
    let mut rt = runtime();
    let commands: Vec<(String, String)> = rt.state.spec.commands.iter().map(|c| (c.id.clone(), c.label.clone())).collect();
    for (id, label) in commands {
        open_palette(&mut rt);
        rt.state.palette.as_mut().unwrap().set_text(&label);
        rt.state.ask_palette();
        settle(&mut rt);
        let rows = &rt.state.palette.as_ref().unwrap().rows;
        assert!(rows.iter().any(|r| r.command.as_ref().is_some_and(|c| c.command == id)), "{label:?} does not find {id}");
        press(&mut rt, Key::Escape, bare());
        press(&mut rt, Key::Escape, bare());
    }
}

// A command first: its alias and a space make it the chip, and Enter runs it on the box picked —
// not on the box the list's cursor is on.
#[test]
fn a_command_chip_runs_on_the_box_chosen() {
    let mut rt = runtime();
    open_palette(&mut rt);
    typed(&mut rt, "stop ");
    let p = rt.state.palette.as_ref().unwrap();
    assert_eq!((p.command.as_str(), p.query.text.as_str()), ("stop", ""), "`stop ` did not commit the chip");
    assert!(p.rows.iter().all(|r| r.command.is_none()), "a command chip lists commands");
    typed(&mut rt, "reaper");
    assert!(picked(&rt).starts_with("box:"), "nothing picked: {:?}", picked(&rt));
    let key = picked(&rt).trim_start_matches("box:").to_string();
    assert_ne!(rt.state.rows()[rt.state.cursor].r#box, key, "the cursor is already on the box");
    press(&mut rt, Key::Enter, bare());
    assert!(rt.state.palette.is_none(), "Enter left the palette open");
    assert_eq!(rt.state.box_by_id(&key).map(|b| b.state), Some(BoxState::Stopped), "stop did not reach {key}");
}

// A box first: Tab makes it the chip, and the commands it can take follow; Enter runs one on it.
#[test]
fn a_box_chip_offers_its_commands() {
    let mut rt = runtime();
    open_palette(&mut rt);
    typed(&mut rt, "quiet");
    press(&mut rt, Key::Tab, bare());
    let p = rt.state.palette.as_ref().unwrap();
    assert!(!p.sandbox.is_empty(), "Tab made no box chip: {}", p.picked);
    let key = p.sandbox.clone();
    assert!(p.rows.iter().all(|r| r.r#box.is_empty()), "a box chip lists boxes");
    assert!(!p.rows.iter().any(|r| r.command.as_ref().is_some_and(|c| c.command == "stop")), "stop is offered for a stopped box");
    typed(&mut rt, "start");
    assert_eq!(picked(&rt), "command:start");
    press(&mut rt, Key::Enter, bare());
    assert_eq!(rt.state.box_by_id(&key).map(|b| b.state), Some(BoxState::Running), "start did not reach {key}");
}

// Esc drops the chip keeping the query, then the query, then the palette.
#[test]
fn escape_steps_back() {
    let mut rt = runtime();
    open_palette(&mut rt);
    typed(&mut rt, "stop rea");
    press(&mut rt, Key::Escape, bare());
    let p = rt.state.palette.as_ref().expect("Esc closed the palette with a chip");
    assert_eq!((p.command.as_str(), p.query.text.as_str()), ("", "rea"));
    press(&mut rt, Key::Escape, bare());
    assert_eq!(rt.state.palette.as_ref().map(|p| p.query.text.as_str()), Some(""));
    press(&mut rt, Key::Escape, bare());
    assert!(rt.state.palette.is_none());
}

// Enter on a box with no chip goes to it; a box the filter hides is shown outside it, and the
// filter comes back when the window leaves it.
#[test]
fn going_to_a_hidden_box_reveals_it_and_leaving_puts_the_filter_back() {
    let mut rt = runtime();
    let wip = rt.state.listing.choices.iter().find(|c| c.label == "#wip").unwrap().mark.clone();
    rt.state.set_filter(wip.clone());
    settle(&mut rt);
    open_palette(&mut rt);
    typed(&mut rt, "reaper");
    let key = picked(&rt).trim_start_matches("box:").to_string();
    assert!(rt.state.row_of(&key).is_none(), "the filter shows the box already");
    press(&mut rt, Key::Enter, bare());
    assert_eq!(rt.state.live.as_deref(), Some(key.as_str()), "Enter did not go to the box");
    assert!(rt.state.row_of(&key).is_some(), "the box is not shown");
    assert_eq!(rt.state.reveal.as_ref().map(|(k, _)| k.as_str()), Some(key.as_str()));
    assert_eq!(rt.state.recent.first(), Some(&key), "the visit was not noted");
    let detach = rt.state.spec.command("detach").and_then(|c| c.bindings.desktop.keys.first().cloned()).unwrap();
    let digit = detach.rsplit('+').next().unwrap().to_string();
    let mods = if cfg!(target_os = "macos") { Mods { cmd: true, ..bare() } } else { Mods { ctrl: true, shift: true, cmd: true, ..bare() } };
    press(&mut rt, Key::Text(digit), mods);
    assert!(rt.state.live.is_none(), "{detach} did not leave the box");
    assert!(rt.state.reveal.is_none());
    assert_eq!(rt.state.listing.filter.tags, wip.tags, "the filter did not come back");
}

// F10 opens the menu bar's first menu off macOS; the arrows walk the menus and Enter runs one.
#[cfg(not(target_os = "macos"))]
#[test]
fn the_menu_bar_is_reached_from_the_keyboard() {
    let mut rt = runtime();
    let menus = rt.state.spec.menus.clone();
    press(&mut rt, Key::Function(10), bare());
    assert_eq!(rt.state.menu.as_ref().map(|m| m.kind.clone()), Some(MenuKind::Bar(menus[0])));
    press(&mut rt, Key::Right, bare());
    assert_eq!(rt.state.menu.as_ref().map(|m| m.kind.clone()), Some(MenuKind::Bar(menus[1])));
    press(&mut rt, Key::Left, bare());
    press(&mut rt, Key::Left, bare());
    assert_eq!(rt.state.menu.as_ref().map(|m| m.kind.clone()), Some(MenuKind::Bar(menus[menus.len() - 1])));
    let help = rt.state.menu.as_ref().unwrap().entries.iter().position(|e| e.command == "help").expect("help in the last menu");
    for _ in 0..help {
        press(&mut rt, Key::Down, bare());
    }
    press(&mut rt, Key::Enter, bare());
    assert!(rt.state.menu.is_none() && rt.state.help, "the menu did not run help");
    press(&mut rt, Key::Escape, bare());
    assert!(!rt.state.help);
}

// A menu's entries are the core's: one that cannot act where the menu was opened says why, and
// one the app does not answer yet says so.
#[test]
fn a_menus_entries_say_why_not() {
    let mut rt = runtime();
    rt.state.open_commands_menu(MenuKind::Bar(DesktopMenu::Box), (0.0, 0.0));
    settle(&mut rt);
    let m = rt.state.menu.as_ref().unwrap();
    let issue = m.entries.iter().find(|e| e.command == "issue").unwrap();
    assert!(!issue.enabled && issue.why.contains("not in the desktop app yet"), "{issue:?}");
    let attach = m.entries.iter().find(|e| e.command == "attach").unwrap();
    assert!(attach.enabled, "{attach:?}");
}

// A pane's menu is the session commands.
#[test]
fn a_panes_menu_is_the_session_commands() {
    let mut rt = runtime();
    press(&mut rt, Key::Enter, bare());
    rt.state.open_commands_menu(MenuKind::Pane, (0.0, 0.0));
    settle(&mut rt);
    let ids: Vec<String> = rt.state.menu.as_ref().unwrap().entries.iter().map(|e| e.command.clone()).collect();
    let want: Vec<String> = rt.state.spec.commands.iter().filter(|c| c.bindings.desktop.menu == Some(DesktopMenu::Session)).map(|c| c.id.clone()).collect();
    assert_eq!(ids, want);
    assert!(ids.contains(&"terminal".to_string()) && ids.contains(&"end".to_string()));
}

// While a box the palette revealed is shown, the person's filter is the one the list's own
// controls change: showing the archived keeps their tags, and lets the reveal go.
#[test]
fn the_archived_toggle_during_a_reveal_keeps_the_persons_filter() {
    let mut rt = runtime();
    let wip = rt.state.listing.choices.iter().find(|c| c.label == "#wip").unwrap().mark.clone();
    rt.state.set_filter(wip.clone());
    settle(&mut rt);
    open_palette(&mut rt);
    typed(&mut rt, "reaper");
    press(&mut rt, Key::Enter, bare());
    assert!(rt.state.reveal.is_some(), "no reveal");
    crate::run_command(&mut rt.state, "archived", &mut rune::Tx::default());
    settle(&mut rt);
    assert!(rt.state.reveal.is_none());
    assert_eq!((rt.state.listing.filter.tags.clone(), rt.state.listing.filter.archived), (wip.tags.clone(), true));
}

// Attaching another box leaves the revealed one, and the filter comes back.
#[test]
fn attaching_elsewhere_puts_the_filter_back() {
    let mut rt = runtime();
    let wip = rt.state.listing.choices.iter().find(|c| c.label == "#wip").unwrap().mark.clone();
    rt.state.set_filter(wip.clone());
    settle(&mut rt);
    open_palette(&mut rt);
    typed(&mut rt, "reaper");
    press(&mut rt, Key::Enter, bare());
    let other = rt.state.rows().iter().map(|r| r.r#box.clone()).find(|k| Some(k) != rt.state.live.as_ref()).expect("another row");
    crate::run_on(&mut rt.state, "attach", vec![other.clone()], &mut rune::Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.live.as_ref(), Some(&other));
    assert!(rt.state.reveal.is_none());
    assert_eq!(rt.state.listing.filter.tags, wip.tags, "the filter did not come back");
}

// An open menu takes the core's every ruling: an entry it greyed is enabled again when the
// core says it can act now; one the app does not answer stays as the app says.
#[test]
fn a_menu_takes_each_ruling() {
    let mut rt = runtime();
    rt.state.open_commands_menu(MenuKind::Bar(DesktopMenu::Box), (0.0, 0.0));
    settle(&mut rt);
    let sub = rt.state.menu.as_ref().unwrap().sub;
    let ruled = |enabled: bool| crate::wire::Commands {
        entries: ["start", "issue"].iter().map(|id| crate::wire::Entry {
            command: id.to_string(), label: id.to_string(), enabled, why: if enabled { String::new() } else { "not now".into() }, ..Default::default()
        }).collect(),
    };
    let entry = |rt: &Runtime<State>, id: &str| rt.state.menu.as_ref().unwrap().entries.iter().find(|e| e.command == id).cloned().unwrap();
    rt.state.take_commands(sub, Ok(ruled(false)));
    assert!(!entry(&rt, "start").enabled);
    rt.state.take_commands(sub, Ok(ruled(true)));
    assert!(entry(&rt, "start").enabled, "start stayed greyed");
    assert!(!entry(&rt, "issue").enabled && entry(&rt, "issue").why.contains("not in the desktop app yet"), "issue took a ruling the app cannot act on");
}
