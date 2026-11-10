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
    assert!(rt.state.sheets.form().is_some_and(|f| f.title == "Rename"), "F2 in the list renames");
    rt.key(KeyEvent { key: Key::Escape, mods: Mods::default() });
    settle(&mut rt);
    assert!(!rt.state.sheets.is_open(), "Esc left the rename up");

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
        ("sheet.rs", include_str!("sheet.rs")),
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

fn filter_menu_on(rt: &mut Runtime<State>, label: &str) {
    run_command(&mut rt.state, "servers", &mut Tx::default());
    settle(rt);
    let at = rt.state.listing.choices.iter().position(|c| c.label == label).unwrap_or_else(|| panic!("no choice {label}"));
    rt.state.filter_menu.as_mut().expect("the filter menu").picked = at;
}

// Enter on a tag marked already keeps it, as the TUI's card does; Space lets it go.
#[test]
fn enter_only_marks_a_tag_and_space_lets_it_go() {
    let mut rt = runtime();
    filter_menu_on(&mut rt, "#wip");
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    assert_eq!(rt.state.listing.filter.tags, vec!["wip".to_string()]);
    filter_menu_on(&mut rt, "#wip");
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    assert_eq!(rt.state.listing.filter.tags, vec!["wip".to_string()], "Enter let a marked tag go");
    filter_menu_on(&mut rt, "#wip");
    rt.key(KeyEvent { key: Key::Text(" ".into()), mods: Mods::default() });
    settle(&mut rt);
    assert!(rt.state.listing.filter.tags.is_empty(), "Space kept a marked tag: {:?}", rt.state.listing.filter.tags);
}

// While a filter asked for is not listed yet, no choice is made against the one it replaces.
#[test]
fn no_choice_is_taken_while_a_filter_is_out() {
    let mut rt = runtime();
    filter_menu_on(&mut rt, "#wip");
    let asked = rt.state.listing.choices.iter().find(|c| c.label == "#tui").unwrap().mark.clone();
    rt.state.set_filter(asked.clone());
    // Not polled: the listing for it has not landed.
    let menu = rt.state.listing.choices.iter().position(|c| c.label == "#wip").unwrap();
    rt.state.filter_menu = Some(crate::FilterMenu { picked: menu });
    crate::overlay::filter_key(&mut rt.state, &KeyEvent { key: Key::Text(" ".into()), mods: Mods::default() }, &mut Tx::default());
    assert_eq!(rt.state.asked.as_ref(), Some(&asked), "a choice was made against the filter being replaced");
    settle(&mut rt);
    assert_eq!(rt.state.listing.filter, asked);
}

// Showing the archived before any listing has landed changes nothing — there is no filter to
// change, and one changed from nothing would be saved over the machine's.
#[test]
fn the_archived_are_not_toggled_before_a_listing() {
    let mut rt = runtime();
    rt.state.loaded = false;
    rt.state.listing = Default::default();
    let sub = rt.state.asked.clone();
    run_command(&mut rt.state, "archived", &mut Tx::default());
    for _ in 0..3 {
        rt.frame(1.0 / 60.0);
    }
    assert_eq!(rt.state.asked, sub);
    assert!(rt.state.message.as_deref().is_some_and(|m| m.contains("not been read")), "{:?}", rt.state.message);
}

// ---------------------------------------------------------------------------------------------
// A pane is a terminal (`term.rs`): what reaches its program, and what reaches the clipboard.

use std::sync::{Arc, Mutex};

use rune::{Ime, clipboard::MemoryClipboard};

use crate::backend::{Backend, Event, Out};
use crate::wire::Command;

/// What the app sent past the window: program input by stream, commands, and resizes.
#[derive(Clone, Default)]
struct Sent {
    input: Arc<Mutex<Vec<(u32, Vec<u8>)>>>,
    commands: Arc<Mutex<Vec<Command>>>,
    resizes: Arc<Mutex<Vec<(u32, u16, u16)>>>,
}

impl Sent {
    fn input_to(&self, stream: u32) -> Vec<u8> {
        self.input.lock().unwrap().iter().filter(|(s, _)| *s == stream).flat_map(|(_, b)| b.clone()).collect()
    }
    fn clear(&self) {
        self.input.lock().unwrap().clear();
        self.commands.lock().unwrap().clear();
        self.resizes.lock().unwrap().clear();
    }
}

/// The mock, with everything sent to it written down.
struct Recorder {
    inner: Box<dyn Backend + Send>,
    sent: Sent,
}

impl Backend for Recorder {
    fn send(&mut self, m: Out) {
        if let Out::Command(c) = &m {
            self.sent.commands.lock().unwrap().push(c.clone());
        }
        self.inner.send(m);
    }
    fn input(&mut self, stream: u32, bytes: &[u8]) {
        self.sent.input.lock().unwrap().push((stream, bytes.to_vec()));
        self.inner.input(stream, bytes);
    }
    fn resize(&mut self, stream: u32, cols: u16, rows: u16) {
        self.sent.resizes.lock().unwrap().push((stream, cols, rows));
        self.inner.resize(stream, cols, rows);
    }
}

struct Null;

impl Backend for Null {
    fn send(&mut self, _: Out) {}
    fn input(&mut self, _: u32, _: &[u8]) {}
    fn resize(&mut self, _: u32, _: u16, _: u16) {}
}

/// A box attached, its primary focused, with what is sent recorded and a clipboard of its own.
fn attached() -> (Runtime<State>, Sent, MemoryClipboard, u32) {
    let mut rt = runtime();
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    let sent = Sent::default();
    let inner = std::mem::replace(&mut rt.state.backend, Box::new(Null));
    rt.state.backend = Box::new(Recorder { inner, sent: sent.clone() });
    let clip = rune::clipboard::memory();
    rt.use_clipboard(&clip);
    let stream = rt.state.focused_pane().expect("a focused pane").stream;
    (rt, sent, clip, stream)
}

/// The focused pane's screen, written to as its program would, and its rect in the window.
fn write(rt: &mut Runtime<State>, bytes: &[u8]) -> [f32; 4] {
    let p = rt.state.focused_pane_mut().unwrap();
    p.screen.feed(bytes);
    let stream = p.stream;
    settle(rt);
    rt.presented(&format!("screen-{stream}")).expect("the screen is drawn").0
}

/// A key with the platform's primary modifier: ⌘ on macOS, Ctrl+Shift elsewhere.
fn primary(c: char) -> KeyEvent {
    let mods = if cfg!(target_os = "macos") { Mods { cmd: true, ..Mods::default() } } else { Mods { ctrl: true, shift: true, cmd: true, ..Mods::default() } };
    KeyEvent { key: Key::Command(c), mods }
}

fn cell(r: [f32; 4], col: usize, row: usize) -> (f32, f32) {
    (r[0] + (col as f32 + 0.25) * theme::cell_w(), r[1] + (row as f32 + 0.5) * theme::CELL_H)
}

// A drag in a pane selects, and its release copies; the copy chord copies a selection and lets
// it go, and with none it is nothing — not the program's; the paste chord pastes the clipboard
// to the program, as it asked to be pasted to.
#[test]
fn the_copy_and_paste_gestures_are_the_panes() {
    let (mut rt, sent, clip, stream) = attached();
    let r = write(&mut rt, b"\x1b[2J\x1b[Hcopy this text");
    let (x0, y) = cell(r, 0, 0);
    let (x1, _) = cell(r, 8, 0);
    rt.pointer_down(x0, y);
    rt.pointer_move(x1 - 0.1 * theme::cell_w(), y);
    rt.pointer_move(x1 + 0.6 * theme::cell_w(), y);
    rt.pointer_up(x1 + 0.6 * theme::cell_w(), y);
    settle(&mut rt);
    assert_eq!(clip.text().as_deref().map(String::as_str), Some("copy this"), "the release copied the selection");
    assert!(rt.state.focused_pane().unwrap().screen.has_selection(), "the selection stays until it is copied again");

    clip.set_text("nothing");
    rt.key(primary('c'));
    settle(&mut rt);
    assert_eq!(clip.text().as_deref().map(String::as_str), Some("copy this"));
    assert!(!rt.state.focused_pane().unwrap().screen.has_selection(), "the copy chord let the selection go");
    sent.clear();
    rt.key(primary('c'));
    settle(&mut rt);
    assert!(sent.input_to(stream).is_empty(), "the copy chord with nothing selected reached the program");

    clip.set_text("pasted\nline");
    rt.key(primary('v'));
    settle(&mut rt);
    let want = rt.state.focused_pane().unwrap().screen.paste("pasted\nline");
    assert_eq!(sent.input_to(stream), want, "the clipboard, as a paste");

    // The secondary button copies a showing selection too.
    let (xa, _) = cell(r, 5, 0);
    rt.pointer_down(xa, y);
    rt.pointer_move(x1 + 0.6 * theme::cell_w(), y);
    rt.pointer_up(x1 + 0.6 * theme::cell_w(), y);
    settle(&mut rt);
    clip.set_text("other");
    rt.secondary_down(x0, y);
    rt.secondary_up(x0, y);
    settle(&mut rt);
    assert_eq!(clip.text().as_deref().map(String::as_str), Some("this"));
    assert!(!rt.state.focused_pane().unwrap().screen.has_selection());
}

// A program's own copy (OSC 52) reaches the clipboard.
#[test]
fn a_programs_copy_reaches_the_clipboard() {
    let (mut rt, _, clip, stream) = attached();
    rt.state.inbox.push(Event::Data(stream, b"\x1b]52;c;ZnJvbSB2aW0=\x07".to_vec()));
    settle(&mut rt);
    assert_eq!(clip.text().as_deref().map(String::as_str), Some("from vim"));
}

// The mouse is the program's once it asks, and the `mouse` command takes it back so a press
// selects; `repaint` asserts the pane's size again, then asks the core for its screen.
#[test]
fn mouse_and_repaint_act_on_the_focused_pane() {
    let (mut rt, sent, _, stream) = attached();
    let r = write(&mut rt, b"\x1b[2J\x1b[2;1Hsome text here\x1b[?1000h\x1b[?1006h");
    let (x, y) = cell(r, 2, 1);
    sent.clear();
    rt.pointer_down(x, y);
    rt.pointer_up(x, y);
    settle(&mut rt);
    assert_eq!(sent.input_to(stream), b"\x1b[<0;3;2M\x1b[<0;3;2m".to_vec(), "the program heard the click");
    assert!(!rt.state.focused_pane().unwrap().screen.has_selection());

    run_command(&mut rt.state, "mouse", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.focused_pane().unwrap().screen.seized, "{:?}", rt.state.message);
    sent.clear();
    let (x2, _) = cell(r, 6, 1);
    rt.pointer_down(x, y);
    rt.pointer_move(x2, y);
    rt.pointer_up(x2, y);
    settle(&mut rt);
    assert!(sent.input_to(stream).is_empty(), "a taken-back mouse reached the program");
    assert!(rt.state.focused_pane().unwrap().screen.has_selection(), "a taken-back mouse selects");
    run_command(&mut rt.state, "mouse", &mut Tx::default());
    settle(&mut rt);
    assert!(!rt.state.focused_pane().unwrap().screen.seized);

    sent.clear();
    run_command(&mut rt.state, "repaint", &mut Tx::default());
    settle(&mut rt);
    let (cols, rows) = { let p = &rt.state.focused_pane().unwrap().screen; (p.cols as u16, p.rows as u16) };
    assert_eq!(sent.resizes.lock().unwrap().as_slice(), &[(stream, cols, rows)], "the size asserted again");
    let commands = sent.commands.lock().unwrap();
    assert!(commands.iter().any(|c| c.command == "repaint" && c.args.stream == stream), "{commands:?}");
    assert_eq!(rt.state.message, None);
}

// The find opens on the focused pane and keeps the keys while it is open; Escape closes it.
#[test]
fn find_opens_on_the_focused_pane_and_keeps_its_keys() {
    let (mut rt, sent, _, stream) = attached();
    let lines: String = (0..80).map(|i| format!("row {i}\r\n")).collect();
    write(&mut rt, format!("{lines}\x1b[?1h").as_bytes());
    rt.key(primary('f'));
    settle(&mut rt);
    assert_eq!(rt.state.find.as_ref().map(|f| f.stream), Some(stream));
    rt.state.find.as_mut().unwrap().query = rune::TextEdit::new("row 12");
    rt.state.find_changed();
    settle(&mut rt);
    let screen = &rt.state.focused_pane().unwrap().screen;
    assert!(screen.offset() > 0, "scrolled back to the match");
    assert!(screen.find.as_ref().unwrap().hit.is_some());
    sent.clear();
    rt.key(KeyEvent { key: Key::Up, mods: Mods::default() });
    settle(&mut rt);
    assert!(sent.input_to(stream).is_empty(), "a key typed into the find reached the program");
    rt.key(KeyEvent { key: Key::Escape, mods: Mods::default() });
    settle(&mut rt);
    assert!(rt.state.find.is_none());
    assert!(rt.state.focused_pane().unwrap().screen.find.is_none());
    // Back in the pane, keys are the program's again.
    rt.key(KeyEvent { key: Key::Up, mods: Mods::default() });
    settle(&mut rt);
    assert_eq!(sent.input_to(stream), b"\x1bOA".to_vec());
}

// An input method's composition is drawn at the cursor, not sent; what it commits is typed.
#[test]
fn an_input_method_composes_at_the_cursor_and_commits_to_the_program() {
    let (mut rt, sent, _, stream) = attached();
    assert!(rt.state.ime(&Ime::Preedit("かん".into(), None)));
    assert_eq!(rt.state.focused_pane().unwrap().screen.preedit.as_deref(), Some("かん"));
    assert!(sent.input_to(stream).is_empty(), "a composition reached the program");
    assert!(rt.state.ime(&Ime::Commit("漢".into())));
    assert_eq!(rt.state.focused_pane().unwrap().screen.preedit, None);
    assert_eq!(sent.input_to(stream), "漢".as_bytes().to_vec());
    // With the list's keys, the pane hears nothing.
    rt.state.focus = Focus::List;
    assert!(!rt.state.ime(&Ime::Commit("x".into())));
}

// The header says what the core's header says of the box's agent, and nothing of its own: a
// header with none draws none.
#[test]
fn the_header_says_what_the_core_sent() {
    let (mut rt, _, _, _) = attached();
    let key = rt.state.live.clone().unwrap();
    let ws = rt.state.live_ws().unwrap();
    let (sub, mut h) = (ws.header_sub, ws.header.clone().expect("the core's header"));
    let listed = rt.state.box_by_id(&key).map(|b| b.agent.clone()).unwrap();
    assert_eq!(h.agent, listed, "the mock's header says its poll");
    h.agent = "blocked".into();
    h.agent_msg = "which branch?".into();
    rt.state.inbox.push(Event::Header(sub, Ok(h.clone())));
    settle(&mut rt);
    assert!(rt.presented("agent").is_some(), "the header drew no agent");
    h.agent.clear();
    h.agent_msg.clear();
    rt.state.inbox.push(Event::Header(sub, Ok(h)));
    settle(&mut rt);
    assert!(rt.presented("agent").is_none_or(|(_, opacity)| opacity == 0.0), "nothing to say is said as nothing");
}

// A program's copy (OSC 52) reaches the clipboard only from the box on screen: a box switched
// away from keeps running, and what it copies is dropped rather than replacing the user's.
#[test]
fn only_the_box_on_screen_copies() {
    let (mut rt, _, clip, stream) = attached();
    let parked = rt.state.live.clone().unwrap();
    let other = (0..rt.state.rows().len()).find_map(|i| rt.state.row_box(i).filter(|b| b.running() && b.key != parked).map(|b| b.key.clone()))
        .expect("another running box");
    rt.state.open_box(&other);
    settle(&mut rt);
    assert_ne!(rt.state.live.as_ref(), Some(&parked));
    clip.set_text("the user's");
    rt.state.inbox.push(Event::Data(stream, b"\x1b]52;c;cGFya2Vk\x07".to_vec()));
    settle(&mut rt);
    assert_eq!(clip.text().as_deref().map(String::as_str), Some("the user's"));
    // Nor is it held until the box is back on screen.
    rt.state.open_box(&parked);
    settle(&mut rt);
    assert_eq!(clip.text().as_deref().map(String::as_str), Some("the user's"));
}

// A find whose pane loses the keys closes, so it never keeps them from the pane that has them.
#[test]
fn a_find_closes_when_its_pane_loses_the_keys() {
    let (mut rt, sent, _, _) = attached();
    rt.key(primary('f'));
    settle(&mut rt);
    assert!(rt.state.find.is_some());
    // The list takes the keys (a click there, say): the bar over the pane is no longer drawn.
    rt.update(|s, _| s.focus = Focus::List);
    settle(&mut rt);
    let cursor = rt.state.cursor;
    rt.key(KeyEvent { key: Key::Down, mods: Mods::default() });
    settle(&mut rt);
    assert!(rt.state.find.is_none(), "the find outlived its pane's keys");
    assert_ne!(rt.state.cursor, cursor, "the find kept the list's keys");
    let _ = sent;
}

// Whatever is in front of the panes takes the input method as it takes the keys: help, a
// menu or the harnesses page up, a composition reaches no pane (one test for both, as both ask
// `State::pane_has_keys`).
#[test]
fn an_input_method_reaches_no_pane_behind_anything() {
    let (mut rt, sent, _, stream) = attached();
    rt.update(|s, _| s.help = true);
    settle(&mut rt);
    assert!(!rt.state.ime(&Ime::Commit("x".into())), "help took no composition");
    rt.update(|s, _| s.help = false);
    rt.update(|s, _| s.harnesses = Some(crate::harnesses::HarnessPage { server: String::new(), rows: vec![], cursor: 0 }));
    settle(&mut rt);
    assert!(!rt.state.ime(&Ime::Preedit("か".into(), None)));
    assert!(!rt.state.ime(&Ime::Commit("漢".into())));
    rt.key(KeyEvent { key: Key::Text("a".into()), mods: Mods::default() });
    settle(&mut rt);
    assert!(sent.input_to(stream).is_empty(), "the page let a key or a composition through to the pane");
}

// A box covered by the harnesses page is not on screen: what its program copies is dropped.
#[test]
fn a_box_under_a_page_copies_nothing() {
    let (mut rt, _, clip, stream) = attached();
    rt.update(|s, _| s.harnesses = Some(crate::harnesses::HarnessPage { server: String::new(), rows: vec![], cursor: 0 }));
    settle(&mut rt);
    clip.set_text("the user's");
    rt.state.inbox.push(Event::Data(stream, b"\x1b]52;c;Y292ZXJlZA==\x07".to_vec()));
    settle(&mut rt);
    assert_eq!(clip.text().as_deref().map(String::as_str), Some("the user's"));
}
