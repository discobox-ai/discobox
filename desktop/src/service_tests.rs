//! A box's services in the workspace: their tabs in the core's order, opened and closed as the
//! core's listing says, numbered apart from the other tabs, stopped rather than ended; and the
//! services menu, its verbs and a service's output.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::backend::{Inbox, Mock, MockService, bundled_spec};
use crate::wire::{Sessions, Tab, TabKind};
use crate::{MenuKind, Side, State, on_key, poll, run_command, theme, view};

fn runtime(state: State) -> Runtime<State> {
    let mut rt = Runtime::with_text(state, view::view, (1440.0, 900.0), Env { system_dark: true, scale: 1.0 }, theme::text_system());
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

/// The first box attached: the mock's declares a dev server running, a worker stopped and a
/// migration that ran.
fn attached() -> Runtime<State> {
    let mut rt = runtime(State::new());
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    rt
}

fn terminals(s: &State) -> Vec<String> {
    let ws = s.live_ws().unwrap();
    ws.column(Side::Terminals).into_iter().map(|i| ws.panes[i].exec.exec.clone()).collect()
}

fn pane<'a>(s: &'a State, exec: &str) -> &'a crate::Pane { s.live_ws().unwrap().panes.iter().find(|p| p.exec.exec == exec).unwrap() }

fn focused(s: &State) -> String {
    let ws = s.live_ws().unwrap();
    ws.focused().map(|i| ws.panes[i].exec.exec.clone()).unwrap_or_default()
}

fn press(rt: &mut Runtime<State>, binding: &str) {
    let mut mods = Mods { alt: binding.contains("alt+"), ..Default::default() };
    if cfg!(target_os = "macos") {
        mods.cmd = true;
    } else {
        (mods.ctrl, mods.shift, mods.cmd) = (true, true, true);
    }
    let key = binding.chars().last().unwrap();
    rt.key(KeyEvent { key: Key::Command(key), mods });
    settle(rt);
}

// The services worth a tab are tabs ahead of the primary, in their declaration order; one not
// running says its state over its card, and none takes the keys.
#[test]
fn services_are_tabs_in_the_cores_order() {
    let rt = attached();
    assert_eq!(terminals(&rt.state), ["service:web", "service:migrate", "primary"]);
    assert_eq!(focused(&rt.state), "primary", "a service took the keys");
    let migrate = pane(&rt.state, "service:migrate");
    assert!(migrate.settled() && migrate.status.as_deref() == Some("exited"), "{:?}", migrate.status);
    // Its last output, written with bare line feeds, is drawn a line at a time from the left.
    assert!(migrate.screen.text().lines().any(|l| l == "migrated 3 tables"), "the card's last output:\n{}", migrate.screen.text());
    let web = pane(&rt.state, "service:web");
    assert!(web.screen.text().lines().any(|l| l == "  ➜  Local:   http://localhost:5173/"), "a running service's output:\n{}", web.screen.text());
    assert!(!pane(&rt.state, "service:web").settled());
}

// The services are numbered apart: the first numbered tab is the primary, and the services
// have their own numbers.
#[test]
fn services_are_numbered_apart() {
    let mut rt = attached();
    press(&mut rt, "primary+alt+2");
    assert_eq!(focused(&rt.state), "service:migrate");
    press(&mut rt, "primary+1");
    assert_eq!(focused(&rt.state), "primary");
    let ws = rt.state.live_ws().unwrap();
    assert_eq!(ws.numbered().len(), 1, "a service wears a tab's number");
}

// A service's tab goes when its listing no longer lists it, and opens again on a stream of its
// own when its run moves; the other tabs stay as they were.
#[test]
fn a_service_tab_follows_its_listing() {
    let mut rt = attached();
    let sub = rt.state.live_ws().unwrap().sessions_sub;
    let web = pane(&rt.state, "service:web").stream;
    let primary = pane(&rt.state, "primary").stream;
    let tab = |exec: &str, run: &str| Tab { exec: exec.into(), kind: TabKind::Service, title: exec.into(), live: true, service: exec.into(), run: run.into(), ..Default::default() };
    let primary_tab = Tab { exec: "primary".into(), kind: TabKind::Primary, live: true, ..Default::default() };
    rt.update(|s, _| s.take_sessions(sub, Ok(Sessions { running: true, sessions: vec![tab("service:web", "2|running"), primary_tab.clone()] })));
    assert_eq!(terminals(&rt.state), ["service:web", "primary"], "the migration's tab stayed");
    assert_ne!(pane(&rt.state, "service:web").stream, web, "a service on another run kept its old stream");
    assert_eq!(pane(&rt.state, "primary").stream, primary);
    assert_eq!(focused(&rt.state), "primary");
}

// A service is stopped from its menu, never ended: end says so, and its chip has no close.
#[test]
fn a_service_is_stopped_not_ended() {
    let mut rt = attached();
    press(&mut rt, "primary+alt+1");
    let web = pane(&rt.state, "service:web").stream;
    run_command(&mut rt.state, "end", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.live_ws().unwrap().panes.iter().any(|p| p.stream == web), "the service's tab was ended");
    assert!(rt.state.message.as_deref().is_some_and(|m| m.contains("stopped rather than ended")), "{:?}", rt.state.message);
    assert!(rt.find(&format!("tab-close-{web}")).is_none(), "a service's chip has a close");
}

// The services menu lists every service; a service's menu offers its output only while it has a
// tab, and its verbs as the core does; a verb runs, and its tab follows.
#[test]
fn the_services_menu_runs_a_verb() {
    let mut rt = attached();
    run_command(&mut rt.state, "services", &mut Tx::default());
    settle(&mut rt);
    let m = rt.state.menu.as_ref().expect("the services menu");
    assert!(matches!(m.kind, MenuKind::Services(..)));
    assert_eq!(m.entries.iter().map(|e| e.command.as_str()).collect::<Vec<_>>(), ["web", "worker", "migrate"]);
    let MenuKind::Services(key, services) = m.kind.clone() else { unreachable!() };
    rt.state.open_service_menu(key, services[1].clone());
    let m = rt.state.menu.as_ref().unwrap();
    assert_eq!(m.entries.iter().map(|e| e.command.as_str()).collect::<Vec<_>>(), ["output", "start", "stop", "restart"]);
    assert!(!m.entries[0].enabled && m.entries[0].why == "no output pane is available", "{:?}", m.entries[0]);
    assert_eq!(m.picked, 1, "the menu opened on what it cannot do");
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("started worker"));
    assert_eq!(terminals(&rt.state), ["service:web", "service:worker", "service:migrate", "primary"]);
}

// A box that declares no services says so, and opens no menu.
#[test]
fn a_box_with_no_services_says_so() {
    let inbox = Inbox::default();
    let state = State::with((Box::new(Mock::with_services(inbox.clone(), Vec::<MockService>::new())), bundled_spec(), inbox, false, String::new()));
    let mut rt = runtime(state);
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    run_command(&mut rt.state, "services", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.menu.is_none());
    assert!(rt.state.message.as_deref().is_some_and(|m| m.contains("declares no services")), "{:?}", rt.state.message);
}
