//! A box's tools: the tools menu as the core lists it, a tool's session given the window and put
//! away, one already running shown rather than started again, a tool on this machine handed the
//! box, a tool's file edited through the person's editor, and an address copied.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::sheet::{OPENED, Sheet};
use crate::backend::Event;
use crate::wire::{Stream, StreamState, TabEnd, TabKind};
use crate::{Focus, MenuKind, Side, State, on_key, poll, run_command, theme, view};

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

fn key(rt: &mut Runtime<State>, key: Key) {
    rt.key(KeyEvent { key, mods: Mods::default() });
    settle(rt);
}

/// The first box attached, and its tools menu open.
fn menu() -> Runtime<State> {
    let mut rt = runtime(State::new());
    key(&mut rt, Key::Enter);
    run_command(&mut rt.state, "tools", &mut Tx::default());
    settle(&mut rt);
    rt
}

fn entries(rt: &Runtime<State>) -> Vec<String> {
    rt.state.menu.as_ref().expect("a menu").entries.iter().map(|e| e.command.clone()).collect()
}

/// Picks a menu's entry by its ID with the keys, as a person would.
fn pick(rt: &mut Runtime<State>, id: &str) {
    let m = rt.state.menu.as_mut().expect("a menu");
    m.picked = m.entries.iter().position(|e| e.command == id).unwrap_or_else(|| panic!("no entry {id}"));
    key(rt, Key::Enter);
}

fn tools(s: &State) -> Vec<usize> {
    let ws = s.live_ws().unwrap();
    ws.column(Side::Shells).into_iter().filter(|&i| ws.panes[i].exec.kind == TabKind::Tool).collect()
}

// The menu lists the box's tools as the core does, each saying what it is or why it cannot run;
// then its addresses to copy; then the files of a tool that carries any.
#[test]
fn the_menu_is_the_cores_tools_and_addresses() {
    let rt = menu();
    let m = rt.state.menu.as_ref().expect("the tools menu");
    assert!(matches!(m.kind, MenuKind::Tools(..)));
    assert_eq!(entries(&rt), ["tool:diff", "tool:fresh", "tool:vscode", "tool:broken", "copy:ssh", "copy:git url", "config:fresh"]);
    let broken = &m.entries[3];
    assert!(!broken.enabled && broken.why == "its command is empty", "{broken:?}");
    assert_eq!(m.entries[4].detail, "ssh sbx_k2m9q4tz7wbf1xay");
    assert_eq!(m.picked, 0);
    assert!(rt.find("menu-tool:diff").is_some(), "the menu is not drawn");
}

// A tool's session is a tab after the shells, given the window; maximize gives the workspace
// back with the tool still on its tab, and choosing it again shows that tab rather than starting
// another.
#[test]
fn a_tool_is_given_the_window_and_put_away() {
    let mut rt = menu();
    pick(&mut rt, "tool:diff");
    let ws = rt.state.live_ws().unwrap();
    assert_eq!(tools(&rt.state).len(), 1, "no tool's tab");
    let i = tools(&rt.state)[0];
    assert_eq!(ws.panes[i].exec.tool, "diff");
    assert!(ws.maximized && ws.focus == Side::Shells && ws.focused() == Some(i), "the tool was not given the window");
    assert_eq!(rt.state.focus, Focus::Panes);

    run_command(&mut rt.state, "maximize", &mut Tx::default());
    settle(&mut rt);
    assert!(!rt.state.live_ws().unwrap().maximized, "maximize did not give the workspace back");
    assert_eq!(tools(&rt.state).len(), 1, "putting it away ended it");

    run_command(&mut rt.state, "tools", &mut Tx::default());
    settle(&mut rt);
    pick(&mut rt, "tool:diff");
    assert_eq!(tools(&rt.state).len(), 1, "a running tool was started again");
    assert!(rt.state.live_ws().unwrap().maximized);
}

// A tool's tab given the window gives it back when it goes, ended from the workspace or from
// inside it: the layout it took, not the workspace maximized onto whatever is left.
#[test]
fn a_tool_that_goes_gives_the_window_back() {
    for inside in [false, true] {
        let mut rt = menu();
        let ws = rt.state.live_ws().unwrap();
        let before = (ws.maximized, ws.focus, ws.active.map(|a| a.map(|i| ws.panes[i].stream)));
        assert!(!before.0, "the workspace opened maximized");
        pick(&mut rt, "tool:diff");
        let ws = rt.state.live_ws().unwrap();
        let stream = ws.panes[tools(&rt.state)[0]].stream;
        assert!(ws.maximized, "the tool was not given the window");
        if inside {
            rt.state.take_stream_event(Event::Stream(Stream { stream, state: StreamState::Ended, end: Some(TabEnd::Tab), ..Default::default() }));
        } else {
            run_command(&mut rt.state, "end", &mut Tx::default());
        }
        settle(&mut rt);
        let ws = rt.state.live_ws().unwrap();
        assert!(tools(&rt.state).is_empty(), "the tool's tab is still there");
        assert_eq!((ws.maximized, ws.focus, ws.active.map(|a| a.map(|i| ws.panes[i].stream))), before, "the layout the tool took was not given back (ended from inside: {inside})");
    }
}

// Maximizing or restoring by hand while a tool has the window is the person's layout: the tool
// going leaves it as they left it.
#[test]
fn a_tool_going_keeps_a_layout_set_by_hand() {
    let mut rt = menu();
    pick(&mut rt, "tool:diff");
    run_command(&mut rt.state, "maximize", &mut Tx::default());
    run_command(&mut rt.state, "maximize", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.live_ws().unwrap().maximized);
    run_command(&mut rt.state, "end", &mut Tx::default());
    settle(&mut rt);
    assert!(tools(&rt.state).is_empty(), "the tool's tab is still there");
    assert!(rt.state.live_ws().unwrap().maximized, "the workspace maximized by hand was restored");
}

// Another tab put in front while a tool has the window is the person's choice too, even once
// they switch back to the tool: the tool going later, from inside, leaves their layout as it is.
#[test]
fn a_tool_going_keeps_a_tab_switched_to() {
    for back in [false, true] {
        let mut rt = menu();
        pick(&mut rt, "tool:diff");
        let ws = rt.state.live_ws().unwrap();
        let stream = ws.panes[tools(&rt.state)[0]].stream;
        run_command(&mut rt.state, "previous pane", &mut Tx::default());
        settle(&mut rt);
        let ws = rt.state.live_ws().unwrap();
        let switched = ws.focused().map(|i| ws.panes[i].stream);
        assert!(ws.maximized && switched.is_some() && switched != Some(stream), "no other tab put in front");
        if back {
            run_command(&mut rt.state, "next pane", &mut Tx::default());
            settle(&mut rt);
            let ws = rt.state.live_ws().unwrap();
            assert_eq!(ws.focused().map(|i| ws.panes[i].stream), Some(stream), "not back on the tool");
        }
        let layout = |rt: &Runtime<State>| {
            let ws = rt.state.live_ws().unwrap();
            (ws.maximized, ws.focus, ws.active.map(|a| a.map(|i| ws.panes[i].stream)).map(|s| s.filter(|&s| s != stream)))
        };
        let before = layout(&rt);
        rt.state.take_stream_event(Event::Stream(Stream { stream, state: StreamState::Ended, end: Some(TabEnd::Tab), ..Default::default() }));
        settle(&mut rt);
        assert!(tools(&rt.state).is_empty(), "the tool's tab is still there");
        let (maximized, _, shown) = layout(&rt);
        assert!(maximized, "the workspace was restored under the person's layout (switched back: {back})");
        assert_eq!(shown[Side::Terminals as usize], before.2[Side::Terminals as usize], "the terminals column moved (switched back: {back})");
    }
}

// A tool on this machine is handed the box, and says so: nothing opens in the workspace.
#[test]
fn a_host_tool_is_handed_the_box() {
    let mut rt = menu();
    pick(&mut rt, "tool:vscode");
    assert!(tools(&rt.state).is_empty(), "a host tool made a tab");
    assert_eq!(rt.state.message.as_deref(), Some("opened rune console port in VS Code"));
}

// A tool that cannot run says why, and runs nothing.
#[test]
fn a_tool_that_cannot_run_says_why() {
    let mut rt = menu();
    pick(&mut rt, "tool:broken");
    assert!(tools(&rt.state).is_empty());
    assert_eq!(rt.state.message.as_deref(), Some("its command is empty"));
}

// A tool's files are picked and edited in the person's editor; Done says what saving does, and
// Esc gives it up.
#[test]
fn a_tools_file_is_edited_in_the_persons_editor() {
    let mut rt = menu();
    pick(&mut rt, "config:fresh");
    assert!(matches!(rt.state.menu.as_ref().map(|m| &m.kind), Some(MenuKind::ToolFiles(..))));
    assert_eq!(entries(&rt), ["config.json", "keys.json"]);
    pick(&mut rt, "config.json");

    let Some(Sheet::Edit(e)) = rt.state.sheets.queue.first() else { panic!("no edit sheet") };
    let path = e.edit.path.clone();
    assert!(path.ends_with(".json") && e.edit.title.starts_with("Editing config.json"), "{path} {}", e.edit.title);
    assert!(OPENED.with_borrow(|o| o.contains(&path)), "the file was not opened in the editor");
    std::fs::write(&path, "{\"theme\":\"mine\"}\n").unwrap();
    key(&mut rt, Key::Enter);
    assert!(!rt.state.sheets.is_open());
    assert_eq!(rt.state.message.as_deref(), Some("config.json saved — the next discobox to open fresh gets it"));

    run_command(&mut rt.state, "tools", &mut Tx::default());
    settle(&mut rt);
    pick(&mut rt, "config:fresh");
    pick(&mut rt, "keys.json");
    assert!(rt.state.sheets.edit_sheet().is_some());
    key(&mut rt, Key::Escape);
    assert!(!rt.state.sheets.is_open() && rt.state.message.is_none(), "{:?}", rt.state.message);
}

// An address is copied as the menu showed it.
#[test]
fn an_address_is_copied() {
    let mut rt = menu();
    pick(&mut rt, "copy:ssh");
    assert!(rt.state.menu.is_none());
    assert_eq!(rt.state.message.as_deref(), Some("copied ssh sbx_k2m9q4tz7wbf1xay"));
}

// A tool the box is running, chosen from the list with its workspace let go, is its session
// opened again: one tab, never a second session.
#[test]
fn a_running_tool_chosen_from_the_list_is_not_started_again() {
    let mut rt = menu();
    pick(&mut rt, "tool:diff");
    let exec = rt.state.live_ws().unwrap().panes[tools(&rt.state)[0]].exec.exec.clone();
    run_command(&mut rt.state, "detach", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.live.is_none(), "detach left the workspace up");

    run_command(&mut rt.state, "tools", &mut Tx::default());
    settle(&mut rt);
    pick(&mut rt, "tool:diff");
    assert_eq!(tools(&rt.state).len(), 1, "a running tool was started again");
    let ws = rt.state.live_ws().unwrap();
    assert_eq!(ws.panes[tools(&rt.state)[0]].exec.exec, exec, "the tool's tab is another session");
    assert!(ws.maximized);
}
