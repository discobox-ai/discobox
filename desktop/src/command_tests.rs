//! Commands that must not reach past what they name: a bare shortcut in a pane, and a shell
//! for a box that is not on screen.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::{State, on_key, poll, run_command, view};

fn runtime() -> Runtime<State> {
    let mut rt = Runtime::new(State::new(), view::view, (1440.0, 900.0), Env { system_dark: true, scale: 1.0 });
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

fn panes(s: &State) -> usize { s.live_ws().map_or(0, |w| w.panes.len()) }

// F2 renames from the list; in a pane it is the program's (htop's setup, mc's menu).
#[test]
fn a_bare_shortcut_in_a_pane_is_the_programs() {
    let mut rt = runtime();
    rt.key(KeyEvent { key: Key::Function(2), mods: Mods::default() });
    settle(&mut rt);
    assert!(rt.state.message.as_deref().is_some_and(|m| m.contains("needs a dialog")), "F2 in the list renames: {:?}", rt.state.message);

    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    assert!(panes(&rt.state) > 0, "attached");
    rt.state.message = None;
    rt.key(KeyEvent { key: Key::Function(2), mods: Mods::default() });
    settle(&mut rt);
    assert_eq!(rt.state.message, None, "F2 in a pane ran a command");
}

// "shell" on a stopped box only points at it; it never opens a shell in the box on screen.
#[test]
fn a_shell_for_a_stopped_box_opens_nowhere() {
    let mut rt = runtime();
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    let live = rt.state.live.clone().expect("a box on screen");
    let before = panes(&rt.state);
    let stopped = rt.state.boxes.iter().position(|b| !b.running()).expect("a box that is not running");
    rt.state.cursor = stopped;
    run_command(&mut rt.state, "box.shell", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.live.as_ref(), Some(&live));
    assert_eq!(panes(&rt.state), before, "a shell opened in {live}");
}
