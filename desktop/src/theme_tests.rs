//! The window's theme (#962): dark, light or the system's, picked from the keyboard through the
//! core's question, kept by the CLI for the next window, and a change of palette drawn as one
//! calm spring on every colour, whether picked or the system's.

use std::sync::{Arc, Mutex};

use rune::motion_gate::Run;
use rune::motion_gate::check::paint::C_FILL;
use rune::{Env, Key, KeyEvent, Mods, Runtime};

use crate::backend::{Event, Inbox, Mock, MockRecord, bundled_spec};
use crate::wire::{Appearance, Session};
use crate::{State, on_key, poll, theme, view};

const HZ: f64 = 60.0;

/// A window on the mock, opening on what the last window was picked to draw in, under a system
/// that is dark or not; and what the mock was sent.
fn window(appearance: Appearance, system_dark: bool) -> (Runtime<State>, Arc<Mutex<MockRecord>>) {
    let inbox = Inbox::default();
    let mock = Mock::new(inbox.clone()).opening_on(appearance);
    let record = mock.record();
    let state = State::with((Box::new(mock), bundled_spec(), inbox, false, Session { appearance, ..Default::default() }));
    let mut rt = Runtime::with_text(state, view::view, (1440.0, 900.0), Env { system_dark, scale: 1.0 }, theme::text_system());
    rt.poll = Some(poll);
    rt.on_key = Some(on_key);
    settle(&mut rt);
    (rt, record)
}

fn settle(rt: &mut Runtime<State>) {
    for _ in 0..60 {
        rt.frame(1.0 / HZ);
    }
}

fn press(rt: &mut Runtime<State>, key: Key, mods: Mods) {
    rt.key(KeyEvent { key, mods });
    settle(rt);
}

fn bare() -> Mods { Mods::default() }

/// The theme command, from the palette by its name, up to its question.
fn ask_theme(rt: &mut Runtime<State>) {
    let chord = rt.state.spec.command("palette").and_then(|c| c.bindings.desktop.keys.first().cloned()).expect("the palette's shortcut");
    let key = chord.rsplit('+').next().unwrap().chars().next().unwrap();
    let mods = if cfg!(target_os = "macos") { Mods { cmd: true, ..bare() } } else { Mods { ctrl: true, shift: true, cmd: true, ..bare() } };
    press(rt, Key::Command(key), mods);
    for c in "theme".chars() {
        press(rt, Key::Text(c.to_string()), bare());
    }
    press(rt, Key::Enter, bare());
    let q = rt.state.sheets.question().expect("the theme's question");
    assert_eq!(q.question.title, "Theme");
}

/// Walks the open question to the choice keyed `key`.
fn choose(rt: &mut Runtime<State>, key: &str) {
    for _ in 0..3 {
        let q = rt.state.sheets.question().expect("the theme's question");
        if q.question.choices[q.picked].key == key {
            return;
        }
        press(rt, Key::Down, bare());
    }
    panic!("{key} is not to be chosen");
}

// The palette finds the theme command, whose question opens on a change from the window's
// palette; the pick is drawn at once and kept for the next window, which opens on it whatever the
// system's appearance does, as a window on the system's follows it.
#[test]
fn a_theme_is_picked_from_the_keys_and_outlasts_the_window() {
    let (mut rt, record) = window(Appearance::System, true);
    assert!(theme::dark(), "a dark system's window opened light");
    ask_theme(&mut rt);
    let q = rt.state.sheets.question().unwrap();
    let closed: Vec<_> = q.question.choices.iter().filter(|c| !c.why.is_empty()).map(|c| c.key.as_str()).collect();
    assert_eq!(closed, ["system"], "the question does not open on a change");
    choose(&mut rt, "light");
    press(&mut rt, Key::Enter, bare());
    assert!(!rt.state.sheets.is_open(), "the question stayed");
    assert!(!theme::dark(), "the pick was not drawn");
    assert_eq!(record.lock().unwrap().appearances, [Appearance::Light], "the pick was not kept");

    let (mut rt, _) = window(Appearance::Light, true);
    assert!(!theme::dark(), "the next window did not open on the pick");
    ask_theme(&mut rt);
    let q = rt.state.sheets.question().unwrap();
    let closed: Vec<_> = q.question.choices.iter().filter(|c| !c.why.is_empty()).map(|c| c.key.as_str()).collect();
    assert_eq!(closed, ["light"], "the next window's question does not open on a change from the pick");
    press(&mut rt, Key::Escape, bare());
    rt.set_system_dark(false);
    settle(&mut rt);
    rt.set_system_dark(true);
    settle(&mut rt);
    assert!(!theme::dark(), "a picked palette followed the system's");

    let (mut rt, _) = window(Appearance::System, true);
    rt.set_system_dark(false);
    settle(&mut rt);
    assert!(!theme::dark(), "the system's palette did not follow it");
}

/// Every element painting its fill at the run's start and end whose fill the change moved: the
/// frame its fill first moved, and the frame it last moved.
fn fills_moved(run: &Run<State>) -> Vec<(String, usize, usize)> {
    let last = run.rec.frames - 1;
    let mut out = vec![];
    for t in &run.rec.tracks {
        let (Some(a), Some(b)) = (t.paint_at(0), t.paint_at(last)) else { continue };
        let (a, b) = (a.colors[C_FILL], b.colors[C_FILL]);
        if (a.r - b.r).abs() + (a.g - b.g).abs() + (a.b - b.b).abs() < 1e-3 {
            continue;
        }
        let fill = |k: usize| t.paint_at(k).map(|p| p.colors[C_FILL]);
        let moved: Vec<usize> = (1..=last).filter(|&k| fill(k) != fill(k - 1)).collect();
        out.push((t.path.clone(), moved[0], *moved.last().unwrap()));
    }
    out
}

/// Asserts the fills a change of palette moved all moved on one spring, the calm one: they set off
/// together, and each settles as the calm spring does, not as an element's own (snappy, 0.25 s;
/// the default, 0.5 s; none) would. A spring settles a short way sooner the nearer its colours
/// are, so they settle in a band about its length.
fn one_calm_spring(what: &str, run: &Run<State>) {
    let moved = fills_moved(run);
    assert!(moved.len() >= 5, "{what}: only {} fills moved: {moved:?}", moved.len());
    let start = moved.iter().map(|m| m.1).min().unwrap();
    let late: Vec<_> = moved.iter().filter(|m| m.1 > start).collect();
    assert!(late.is_empty(), "{what}: fills that set off after the rest: {late:?}");
    let calm = theme::CALM.duration as f64 * HZ;
    let off: Vec<_> = moved.iter().filter(|m| !(calm * 0.75..=calm * 1.75).contains(&((m.2 - start) as f64))).collect();
    assert!(off.is_empty(), "{what}: fills that did not settle as the calm spring does (~{calm} frames): {off:?}");
}

// A change of palette is one calm spring across the window: picked through the theme's question,
// or the system's appearance changing under a window that follows it.
#[test]
fn a_theme_change_is_one_calm_spring() {
    let (mut rt, _) = window(Appearance::System, true);
    ask_theme(&mut rt);
    choose(&mut rt, "light");
    let mut run = Run::new(rt, HZ);
    run.idle(0.5);
    run.step();
    run.act("Enter on light", |rt| rt.key(KeyEvent { key: Key::Enter, mods: bare() }));
    run.play(1.5);
    assert!(!theme::dark());
    one_calm_spring("picked", &run);

    let (rt, _) = window(Appearance::System, true);
    let mut run = Run::new(rt, HZ);
    run.idle(0.5);
    run.step();
    run.act("the system turns light", |rt| rt.set_system_dark(false));
    run.play(1.5);
    assert!(!theme::dark());
    one_calm_spring("the system's", &run);
}

// What a program writes as the palette changes lands as written: the change's calm spring is a
// commit of its own, never one a pane's output is taken into, whichever frame after the change the
// output arrives in.
#[test]
fn a_programs_output_never_rides_a_theme_change() {
    let red = theme::xterm(1);
    for after in 0..3 {
        let (mut rt, _) = window(Appearance::System, true);
        rt.update(|s, _| s.open_row(0));
        settle(&mut rt);
        let stream = rt.state.focused_pane().expect("a pane").stream;
        let mut run = Run::new(rt, HZ);
        run.idle(0.5);
        run.step();
        run.act("the system turns light", |rt| rt.set_system_dark(false));
        run.step_n(after);
        run.act("a program paints red", move |rt| rt.state.inbox.push(Event::Data(stream, b"\x1b[41m  red  \x1b[0m\r\n".to_vec())));
        run.play(1.0);
        assert!(!theme::dark());
        let last = run.rec.frames - 1;
        let mut painted = 0;
        for t in &run.rec.tracks {
            if t.paint_at(last).map(|p| p.colors[C_FILL]) != Some(red) {
                continue;
            }
            painted += 1;
            let fills: Vec<_> = t.frames.iter().filter_map(|(k, _)| t.paint_at(*k)).map(|p| p.colors[C_FILL]).collect();
            let blended: Vec<_> = fills.iter().filter(|c| **c != red && **c != fills[0]).collect();
            assert!(blended.is_empty(), "{after} frames after the change: {} sprang to red through {blended:?}", t.path);
        }
        assert!(painted > 0, "{after} frames after the change: nothing was painted red");
    }
}
