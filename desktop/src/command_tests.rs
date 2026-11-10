//! Commands that must not reach past what they name: a bare shortcut in a pane, and a shell
//! for a box that is not on screen.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::{ANSWERED, Focus, State, on_key, poll, run_command, theme, view};

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
    let stopped = (0..rt.state.rows().len()).find(|&i| rt.state.row_box(i).is_some_and(|b| !b.running())).expect("a box that is not running");
    rt.state.cursor = stopped;
    rt.state.focus = Focus::List;
    run_command(&mut rt.state, "shell", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.live.as_ref(), Some(&live));
    assert_eq!(panes(&rt.state), before, "a shell opened in {live}");
}

// Every registry command is answered here, or the parity ledger says why it is not and which
// issue brings it (ADR 26-11-07-598 §1); and a command the app lacks says so when run.
#[test]
fn every_registry_command_is_answered_or_in_the_ledger() {
    let ledger = include_str!("../../docs/console/parity.md");
    let section = ledger.split("## The desktop app lacks").nth(1).expect("the ledger's desktop section");
    let section = section.split("\n## ").next().unwrap();
    let lacks: Vec<&str> = section.lines()
        .filter_map(|l| l.strip_prefix("| `")).filter_map(|l| l.split('`').next()).collect();
    let mut rt = runtime();
    let ids: Vec<String> = rt.state.spec.commands.iter().map(|c| c.id.clone()).collect();
    for id in &lacks {
        assert!(ids.iter().any(|i| i == id), "the ledger lists {id}, which is no registry command");
        assert!(!ANSWERED.contains(id), "the ledger lists {id}, which the app answers");
    }
    for id in &ids {
        if id == "quit" {
            continue; // it would end the runtime; it is answered.
        }
        let answered = ANSWERED.contains(&id.as_str());
        assert!(answered || lacks.contains(&id.as_str()), "{id} is neither answered nor in the ledger");
        run_command(&mut rt.state, id, &mut Tx::default());
        settle(&mut rt);
        let says_lacking = rt.state.message.as_deref().is_some_and(|m| m.contains("desktop app does not have") || m.contains("not in the desktop app"));
        assert_eq!(says_lacking, !answered, "{id}: {:?}", rt.state.message);
    }
}

// Every shortcut is the registry's (ADR 26-11-07-598 §3): the app matches a key press only
// against the spec's bindings — a command's, or a gesture's — so one written here instead, as
// the navigation and the ⌘-digits once were, fails.
#[test]
fn no_shortcut_is_written_outside_the_spec() {
    let files = [
        ("keys.rs", include_str!("keys.rs")),
        ("main.rs", include_str!("main.rs")),
        ("view.rs", include_str!("view.rs")),
        ("overlay.rs", include_str!("overlay.rs")),
    ];
    for (file, src) in files {
        for (i, line) in src.lines().enumerate() {
            let code = line.split("//").next().unwrap();
            // A spec binding is converted by its `primary+` prefix (menu_shortcut); a chord is
            // something more after it.
            let chord = code.contains("\"primary+") && !code.contains("(\"primary+\")");
            let by_hand = code.contains("matches(\"") || code.contains("matches(&format!") || chord;
            assert!(!by_hand, "{file}:{}: a shortcut written by hand: {line}", i + 1);
        }
    }
    let keys = include_str!("keys.rs");
    let list_key = keys.split("fn list_key").nth(1).unwrap().split("\nfn ").next().unwrap();
    assert!(!list_key.contains("Key::"), "the list matches a key by hand rather than by a gesture");
    // And every gesture the spec binds here is one the app answers: an arm of list_key's match,
    // or on_key's numbered gesture.
    let on_key = keys.split("pub fn on_key").nth(1).unwrap().split("\nfn ").next().unwrap();
    let rt = runtime();
    for g in rt.state.spec.gestures.iter().filter(|g| !g.bindings.desktop.keys.is_empty()) {
        let answered = list_key.contains(&format!("\"{}\" =>", g.id)) || list_key.contains(&format!("\"{}\" if", g.id))
            || on_key.contains(&format!("Some((\"{}\"", g.id));
        assert!(answered, "the gesture {} is bound but not answered", g.id);
    }
}
