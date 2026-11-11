//! The workspace as the app holds it: parking and unparking, what an ending does, chips dragged
//! along their strip, the columns' split, and the header's links.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::backend::Event;
use crate::wire::{Result as CommandResult, Status, Stream, StreamState, Tab, TabEnd, TabKind};
use crate::{Focus, MIN_SPLIT, Pending, Side, State, on_key, poll, run_command, theme, view};

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

/// The first box attached, with a shell beside its primary.
fn attached_with_a_shell() -> Runtime<State> {
    let mut rt = runtime();
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    run_command(&mut rt.state, "shell", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.live_ws().map(|w| w.column(Side::Shells).len()), Some(1), "a shell beside the primary");
    rt
}

/// Whether the app follows anything about a box: its sessions or its header.
fn follows(s: &State, key: &str) -> bool { s.subs.values().chain(s.headers.values()).any(|b| b == key) }

// Switching to another box parks the workspace: its tabs and their streams are kept, and it
// polls nothing and forwards nothing until it is back on screen.
#[test]
fn switching_away_parks_and_switching_back_unparks() {
    let mut rt = attached_with_a_shell();
    let first = rt.state.live.clone().unwrap();
    let streams: Vec<u32> = rt.state.live_ws().unwrap().panes.iter().map(|p| p.stream).collect();
    run_command(&mut rt.state, "next box", &mut Tx::default());
    settle(&mut rt);
    assert_ne!(rt.state.live.as_ref(), Some(&first));
    let parked = &rt.state.workspaces[&first];
    assert!(parked.parked(), "the first box's workspace is parked");
    assert_eq!(parked.panes.iter().map(|p| p.stream).collect::<Vec<_>>(), streams, "its tabs are kept");
    assert!(!follows(&rt.state, &first), "a parked workspace still polls or forwards");

    run_command(&mut rt.state, "previous box", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.live.as_ref(), Some(&first));
    let ws = rt.state.live_ws().unwrap();
    assert!(!ws.parked() && follows(&rt.state, &first), "back on screen, it follows its sessions and header again");
    assert_eq!(ws.panes.iter().map(|p| p.stream).collect::<Vec<_>>(), streams, "the same tabs, on the same streams");
    assert!(ws.header.is_some(), "its header is read again");
}

// A workspace whose primary never arrived has nothing to keep: switching away lets it go.
#[test]
fn a_workspace_without_its_primary_is_let_go() {
    let mut rt = runtime();
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    let first = rt.state.live.clone().unwrap();
    rt.state.workspaces.get_mut(&first).unwrap().panes.retain(|p| p.exec.kind != TabKind::Primary);
    run_command(&mut rt.state, "next box", &mut Tx::default());
    settle(&mut rt);
    assert!(!rt.state.workspaces.contains_key(&first) && !follows(&rt.state, &first));
}

// Detaching closes the workspace, as the TUI's does: its sessions go on in the box.
#[test]
fn detaching_lets_the_workspace_go() {
    let mut rt = attached_with_a_shell();
    let first = rt.state.live.clone().unwrap();
    run_command(&mut rt.state, "detach", &mut Tx::default());
    settle(&mut rt);
    assert_eq!((rt.state.live.as_ref(), rt.state.focus), (None, Focus::List));
    assert!(!rt.state.workspaces.contains_key(&first) && !follows(&rt.state, &first));
}

fn ended(stream: u32, end: TabEnd, why: &str, status: &str) -> Stream {
    Stream { stream, state: StreamState::Ended, end: Some(end), why: why.into(), status: status.into() }
}

// What an ending does is the core's: a finished shell is held with its last screen until it is
// dismissed, a broken one goes and says why, and the primary takes the workspace with it.
#[test]
fn endings_do_what_the_core_says() {
    let mut rt = attached_with_a_shell();
    let first = rt.state.live.clone().unwrap();
    let shell = rt.state.live_ws().unwrap().panes.iter().find(|p| p.side() == Side::Shells).unwrap().stream;
    rt.update(|s, _| s.take_stream(ended(shell, TabEnd::Held, "", "finished")));
    let p = rt.state.live_ws().unwrap().panes.iter().find(|p| p.stream == shell).unwrap();
    assert!(p.exited && p.status.as_deref() == Some("finished"), "held: {:?}", p.status);
    settle(&mut rt);
    assert!(rt.find(&format!("status-{shell}")).is_some(), "the held tab says so over its screen");
    // Its close dismisses it: there is no session left to end.
    run_command(&mut rt.state, "end", &mut Tx::default());
    assert!(rt.state.live_ws().unwrap().panes.iter().all(|p| p.stream != shell), "the held tab was not dismissed");

    run_command(&mut rt.state, "shell", &mut Tx::default());
    settle(&mut rt);
    let shell = rt.state.live_ws().unwrap().panes.iter().find(|p| p.side() == Side::Shells).unwrap().stream;
    rt.update(|s, _| s.take_stream(ended(shell, TabEnd::Tab, "connection lost", "")));
    assert!(rt.state.live_ws().unwrap().panes.iter().all(|p| p.stream != shell), "a broken tab goes");
    assert!(rt.state.message.as_deref().is_some_and(|m| m.contains("connection lost")), "{:?}", rt.state.message);

    let primary = rt.state.live_ws().unwrap().panes.iter().find(|p| p.exec.kind == TabKind::Primary).unwrap().stream;
    rt.update(|s, _| s.take_stream(ended(primary, TabEnd::Workspace, "", "")));
    assert_eq!((rt.state.live.as_ref(), rt.state.focus), (None, Focus::List));
    assert!(!rt.state.workspaces.contains_key(&first));
    assert!(rt.state.message.as_deref().is_some_and(|m| m.ends_with("the session ended")), "{:?}", rt.state.message);
}

// Reconnecting is said on the tab and over its screen, and goes once it is back.
#[test]
fn reconnecting_is_said_until_it_is_back() {
    let mut rt = attached_with_a_shell();
    let shell = rt.state.live_ws().unwrap().panes.iter().find(|p| p.side() == Side::Shells).unwrap().stream;
    rt.update(|s, _| s.take_stream(Stream { stream: shell, state: StreamState::Reconnecting, ..Default::default() }));
    settle(&mut rt);
    assert!(rt.find(&format!("status-{shell}")).is_some());
    rt.update(|s, _| s.take_stream(Stream { stream: shell, state: StreamState::Reconnected, ..Default::default() }));
    settle(&mut rt);
    assert!(rt.find(&format!("status-{shell}")).is_none());
}

fn drag(rt: &mut Runtime<State>, from: (f32, f32), to: (f32, f32)) {
    rt.pointer_move(from.0, from.1);
    rt.pointer_down(from.0, from.1);
    rt.frame(1.0 / 60.0);
    for i in 1..=10 {
        let f = i as f32 / 10.0;
        rt.pointer_move(from.0 + (to.0 - from.0) * f, from.1 + (to.1 - from.1) * f);
        rt.frame(1.0 / 60.0);
    }
    rt.pointer_up(to.0, to.1);
    settle(rt);
}

// A chip dragged along its strip takes its new place, and the tab shown stays the one shown.
#[test]
fn a_chip_dragged_along_its_strip_moves_its_tab() {
    let mut rt = attached_with_a_shell();
    run_command(&mut rt.state, "shell", &mut Tx::default());
    settle(&mut rt);
    let order = |s: &State| -> Vec<u32> { s.live_ws().unwrap().panes.iter().filter(|p| p.side() == Side::Shells).map(|p| p.stream).collect() };
    let before = order(&rt.state);
    assert_eq!(before.len(), 2);
    let (a, b) = (before[0], before[1]);
    let from = rt.find(&format!("tab-{a}")).expect("the first shell's chip");
    let to = rt.find(&format!("tab-{b}")).expect("the second shell's chip");
    drag(&mut rt, from, (to.0 + 20.0, to.1));
    assert_eq!(order(&rt.state), vec![b, a], "the chip did not move");
    assert_eq!(rt.state.dragging, None);
    let ws = rt.state.live_ws().unwrap();
    // The press that ended the drag shows the chip dragged.
    assert_eq!(ws.active[Side::Shells as usize].map(|i| ws.panes[i].stream), Some(a));
    // A terminal's chip is not dropped among the shells.
    let primary = ws.panes.iter().position(|p| p.exec.kind == TabKind::Primary).unwrap();
    let shell = ws.panes.iter().position(|p| p.stream == a).unwrap();
    let mut panes: Vec<u32> = ws.panes.iter().map(|p| p.stream).collect();
    rt.state.live_ws_mut().unwrap().move_pane(primary, shell);
    panes.sort();
    let mut after: Vec<u32> = rt.state.live_ws().unwrap().panes.iter().map(|p| p.stream).collect();
    after.sort();
    assert_eq!(panes, after);
    assert_eq!(rt.state.live_ws().unwrap().panes[primary].exec.kind, TabKind::Primary, "a chip crossed columns");
}

// The divider between the columns moves where they split, as far as each keeps its least share;
// a double press puts them back half and half.
#[test]
fn the_split_follows_the_divider() {
    let mut rt = attached_with_a_shell();
    let at = rt.find("split").expect("the divider between the columns");
    let w = rt.state.columns_w;
    assert!(w > 0.0);
    drag(&mut rt, at, (at.0 + w * 0.1, at.1));
    let split = rt.state.live_ws().unwrap().split;
    assert!((split - 0.6).abs() < 0.02, "split {split}");
    let at = rt.find("split").unwrap();
    drag(&mut rt, at, (at.0 + w, at.1));
    assert_eq!(rt.state.live_ws().unwrap().split, 1.0 - MIN_SPLIT);
    let at = rt.find("split").unwrap();
    rt.pointer_down(at.0, at.1);
    rt.pointer_up(at.0, at.1);
    rt.frame(1.0 / 60.0);
    rt.pointer_down(at.0, at.1);
    rt.pointer_up(at.0, at.1);
    settle(&mut rt);
    assert_eq!(rt.state.live_ws().unwrap().split, 0.5);
}

// A forwarded web port in the header opens in the browser; a port that is not a page is text.
#[test]
fn a_forwarded_port_opens_in_the_browser() {
    let mut rt = attached_with_a_shell();
    let h = rt.state.live_ws().unwrap().header.clone().expect("the header");
    let (g, i) = h.ports.iter().enumerate().find_map(|(g, grp)| grp.ports.iter().position(|p| !p.url.is_empty()).map(|i| (g, i))).expect("a linked port");
    let url = h.ports[g].ports[i].url.clone();
    let at = rt.find(&format!("port-{g}-{i}")).expect("the port's link");
    rt.pointer_down(at.0, at.1);
    rt.pointer_up(at.0, at.1);
    settle(&mut rt);
    assert_eq!(rt.take_url_requests(), vec![url]);
}

// Escape during a chip's drag puts the chip back where it was.
#[test]
fn a_cancelled_chip_drag_puts_the_chip_back() {
    let mut rt = attached_with_a_shell();
    run_command(&mut rt.state, "shell", &mut Tx::default());
    settle(&mut rt);
    let order = |s: &State| -> Vec<u32> { s.live_ws().unwrap().panes.iter().map(|p| p.stream).collect() };
    let before = order(&rt.state);
    let shells: Vec<u32> = rt.state.live_ws().unwrap().panes.iter().filter(|p| p.side() == Side::Shells).map(|p| p.stream).collect();
    let from = rt.find(&format!("tab-{}", shells[0])).unwrap();
    let to = rt.find(&format!("tab-{}", shells[1])).unwrap();
    rt.pointer_move(from.0, from.1);
    rt.pointer_down(from.0, from.1);
    rt.frame(1.0 / 60.0);
    for i in 1..=10 {
        rt.pointer_move(from.0 + (to.0 + 20.0 - from.0) * i as f32 / 10.0, from.1);
        rt.frame(1.0 / 60.0);
    }
    assert_ne!(order(&rt.state), before, "the drag moved nothing");
    rt.key(KeyEvent { key: Key::Escape, mods: Mods::default() });
    rt.pointer_up(to.0 + 20.0, to.1);
    settle(&mut rt);
    assert_eq!(order(&rt.state), before, "a cancelled drag left the chip moved");
    assert_eq!(rt.state.dragging, None);
}

fn shell_tab(exec: &str) -> Tab { Tab { exec: exec.into(), kind: TabKind::Shell, title: "bash".into(), live: true, argv: vec![], ..Default::default() } }

// A shell's stream is attached before its result says what it is, so its ending can come
// first: it is kept with the stream's frames and applied once the pane is made.
#[test]
fn an_ending_before_its_pane_is_applied_once_the_pane_is_made() {
    let mut rt = attached_with_a_shell();
    let key = rt.state.live.clone().unwrap();
    rt.update(|s, tx| {
        s.pending.insert(990, Pending::Session(770, key.clone(), s.live.clone()));
        s.take_stream_event(Event::Stream(ended(770, TabEnd::Tab, "connection lost", "")));
        s.take_stream_event(Event::Exited(770, "connection lost".into()));
        s.take_result(CommandResult { id: 990, status: Status::Done, r#box: key.clone(), session: Some(shell_tab("sh9")), ..Default::default() }, tx);
    });
    assert!(rt.state.live_ws().unwrap().panes.iter().all(|p| p.stream != 770), "a broken stream's tab stayed");
    assert!(rt.state.message.as_deref().is_some_and(|m| m.contains("connection lost")), "{:?}", rt.state.message);
}

// A session whose result lands after the person moved to another box goes into its own box's
// workspace, parked, and leaves the screen where it is; with no workspace there, it waits in
// the box.
#[test]
fn a_late_session_does_not_take_the_screen() {
    let mut rt = attached_with_a_shell();
    let first = rt.state.live.clone().unwrap();
    rt.update(|s, _| { s.pending.insert(991, Pending::Session(771, first.clone(), Some(first.clone()))); });
    run_command(&mut rt.state, "next box", &mut Tx::default());
    settle(&mut rt);
    let second = rt.state.live.clone().unwrap();
    assert_ne!(second, first);
    rt.update(|s, tx| s.take_result(CommandResult { id: 991, status: Status::Done, r#box: first.clone(), session: Some(shell_tab("sh8")), ..Default::default() }, tx));
    assert_eq!(rt.state.live.as_ref(), Some(&second), "the late shell took the screen");
    assert!(rt.state.workspaces.contains_key(&second), "the box on screen was let go");
    let parked = &rt.state.workspaces[&first];
    assert!(parked.parked() && parked.panes.iter().any(|p| p.stream == 771), "the shell is not in its own box's workspace");

    let third = (0..rt.state.rows().len()).filter_map(|i| rt.state.row_box(i)).map(|b| b.key.clone()).find(|k| *k != first && *k != second).unwrap();
    rt.update(|s, _| { s.pending.insert(992, Pending::Session(772, third.clone(), Some(first.clone()))); });
    rt.update(|s, tx| s.take_result(CommandResult { id: 992, status: Status::Done, r#box: third.clone(), session: Some(shell_tab("sh7")), ..Default::default() }, tx));
    assert_eq!(rt.state.live.as_ref(), Some(&second));
    assert!(!rt.state.workspaces.contains_key(&third), "a workspace was made for a box nobody opened");
}

/// The window on the mock, and what it was sent.
fn recorded() -> (Runtime<State>, std::sync::Arc<std::sync::Mutex<crate::backend::MockRecord>>) {
    let inbox = crate::backend::Inbox::default();
    let mock = crate::backend::Mock::new(inbox.clone());
    let record = mock.record();
    let state = State::with((Box::new(mock), crate::backend::bundled_spec(), inbox, false, String::new()));
    let mut rt = Runtime::with_text(state, view::view, (1440.0, 900.0), Env { system_dark: true, scale: 1.0 }, theme::text_system());
    rt.poll = Some(poll);
    rt.on_key = Some(on_key);
    settle(&mut rt);
    (rt, record)
}

fn pushed(said: u64, text: &str, warning: bool, elsewhere: &str) -> crate::wire::Push {
    crate::wire::Push { said, text: text.into(), warning, elsewhere: elsewhere.into() }
}

// A workspace pushes for as long as it is attached, parked included, as the TUI's does: its push
// is followed from when it is made until it is let go, and what the core says of it is on the
// status line until something else is said there, a parked box named.
#[test]
fn a_workspace_pushes_while_it_is_attached_and_says_what_moved() {
    let (mut rt, record) = recorded();
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    let first = rt.state.live.clone().unwrap();
    let sub = rt.state.workspaces[&first].push_sub;
    assert_eq!(record.lock().unwrap().pushing.get(&sub), Some(&first), "its push is followed as it is made");
    assert_eq!(rt.state.message, None, "the first snapshot says nothing");

    rt.state.inbox.push(Event::Push(sub, pushed(1, "pushed c1 to origin/main in the discobox", false, "one: pushed c1 to origin/main in the discobox")));
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("pushed c1 to origin/main in the discobox"));

    // Said once: the same snapshot again is not said again.
    rt.state.message = None;
    rt.state.inbox.push(Event::Push(sub, pushed(1, "pushed c1 to origin/main in the discobox", false, "")));
    settle(&mut rt);
    assert_eq!(rt.state.message, None);

    // A refusal is said, and what is said after it replaces it, as the TUI's status line does:
    // it is ambient, never a command's warning that stands.
    rt.state.inbox.push(Event::Push(sub, pushed(2, "the lease moved", true, "one: the lease moved")));
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("the lease moved"));
    rt.state.inbox.push(Event::Push(sub, pushed(3, "pushed c3 to origin/main in the discobox", false, "")));
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("pushed c3 to origin/main in the discobox"));

    // A running command's warning stands over it.
    rt.state.take_progress(crate::wire::Progress { command: 1, text: "uncommitted changes left out".into(), warning: true, ..Default::default() });
    rt.state.inbox.push(Event::Push(sub, pushed(4, "the lease moved", true, "one: the lease moved")));
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("uncommitted changes left out"));

    // Parked, it still pushes, and what it says names it.
    run_command(&mut rt.state, "next box", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.workspaces[&first].parked());
    assert_eq!(record.lock().unwrap().pushing.get(&sub), Some(&first), "a parked workspace still pushes");
    rt.state.inbox.push(Event::Push(sub, pushed(5, "pushed c4 to origin/main in the discobox", false, "one: pushed c4 to origin/main in the discobox")));
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("one: pushed c4 to origin/main in the discobox"));

    // Let go, it pushes no more.
    run_command(&mut rt.state, "previous box", &mut Tx::default());
    settle(&mut rt);
    run_command(&mut rt.state, "detach", &mut Tx::default());
    settle(&mut rt);
    assert!(!rt.state.workspaces.contains_key(&first));
    assert!(record.lock().unwrap().pushing.values().all(|b| *b != first), "a workspace let go still pushes");
}

/// The window's title as Rune holds it: its accessibility root's name.
fn window_title(rt: &Runtime<State>) -> String {
    rt.accessibility_json()["name"].as_str().unwrap_or_default().to_string()
}

// The window is titled by the core's title for the box on screen, the TUI's title bar's text
// (`Header.title`), and follows it as the core says it again; with no box on screen it is
// `discobox`.
#[test]
fn the_window_title_follows_the_box_on_screen() {
    let mut rt = runtime();
    assert_eq!(window_title(&rt), "discobox", "no box on screen: {}", rt.accessibility_json());
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    let first = rt.state.live.clone().expect("a box on screen");
    let ws = rt.state.live_ws().unwrap();
    let (sub, mut h) = (ws.header_sub, ws.header.clone().expect("the core's header"));
    assert!(!h.title.is_empty(), "the mock titles its header");
    assert_eq!(window_title(&rt), h.title, "the box on screen titles the window");

    h.title = "fixer: which branch?".into();
    rt.state.inbox.push(Event::Header(sub, Ok(h)));
    settle(&mut rt);
    assert_eq!(window_title(&rt), "fixer: which branch?", "the title follows the core's");

    run_command(&mut rt.state, "next box", &mut Tx::default());
    settle(&mut rt);
    assert_ne!(rt.state.live.as_ref(), Some(&first));
    let other = rt.state.live_ws().and_then(|w| w.header.clone()).expect("the next box's header").title;
    assert_eq!(window_title(&rt), other, "another box on screen titles it");

    run_command(&mut rt.state, "detach", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.live.is_none(), "detached");
    assert_eq!(window_title(&rt), "discobox", "no box on screen again");
}
