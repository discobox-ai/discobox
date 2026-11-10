//! A box's audit view: the core's timeline followed, a row held while newer records arrive, a run
//! opened and folded, a record read in full and its recordings over it, what could not be read,
//! and the view giving the screen back.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::backend::{Event, mock_audit};
use crate::wire::{AuditRecord, AuditRun};
use crate::{MenuKind, State, audit, on_key, poll, run_command, theme, view};

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

/// The first box attached, and its audit view open.
fn view() -> Runtime<State> {
    let mut rt = runtime(State::new());
    key(&mut rt, Key::Enter);
    run_command(&mut rt.state, "audit", &mut Tx::default());
    settle(&mut rt);
    rt
}

/// The record each row is drawn from, by ID, and whether it is an opened run's record.
fn rows(rt: &Runtime<State>) -> Vec<(String, bool)> {
    let v = rt.state.audit.as_ref().expect("the audit view");
    v.rows.iter().map(|r| (v.timeline.records[r.record].id.clone(), r.member)).collect()
}

fn cursor(rt: &Runtime<State>) -> String {
    let v = rt.state.audit.as_ref().expect("the audit view");
    v.timeline.records[v.rows[v.cursor].record].id.clone()
}

/// The mock's timeline with one more record at its end, as the core sends it whole.
fn arrives(rt: &mut Runtime<State>, id: &str) {
    let mut a = mock_audit();
    a.records.push(AuditRecord { id: id.into(), clock: "14:05:00".into(), source: "http".into(), summary: format!("GET 200 {id}"), ..Default::default() });
    let n = a.records.len() as i64 - 1;
    a.runs.push(AuditRun { first: n, last: n, summary: String::new() });
    let sub = rt.state.audit.as_ref().unwrap().sub;
    rt.state.inbox.push(Event::Audit(sub, Ok(a)));
    settle(rt);
}

// The view is the core's timeline, beside the list in place of the workspace: a run folded into
// one row, the cursor riding the newest, and the keys the view's; what arrives is followed.
#[test]
fn the_view_follows_the_cores_timeline() {
    let mut rt = view();
    let ids: Vec<_> = rows(&rt).into_iter().map(|(id, _)| id).collect();
    assert_eq!(ids, ["creds-1", "http-1", "execs-1", "http-4"], "a run of three is one row");
    let v = rt.state.audit.as_ref().unwrap();
    assert!(v.following && cursor(&rt) == "http-4", "it opens on the newest, following");
    assert!(rt.state.page_shown() && !rt.state.workspace_shown() && rt.state.live.is_some(), "the workspace is held under the view");
    assert!(!rt.state.context().pane_focused, "the panes do not have the keys");
    arrives(&mut rt, "http-5");
    assert_eq!(cursor(&rt), "http-5", "following rides what arrives");
}

// Moving off the newest stops the cursor on its row while newer records arrive, and says how many
// are below it; End follows again.
#[test]
fn moving_off_the_newest_holds_the_row() {
    let mut rt = view();
    key(&mut rt, Key::Up);
    key(&mut rt, Key::Up);
    assert_eq!(cursor(&rt), "http-1");
    assert!(!rt.state.audit.as_ref().unwrap().following);
    arrives(&mut rt, "http-5");
    assert_eq!(cursor(&rt), "http-1", "a record arriving moved the row being read");
    let v = rt.state.audit.as_ref().unwrap();
    assert!(!v.following);
    key(&mut rt, Key::End);
    assert!(rt.state.audit.as_ref().unwrap().following && cursor(&rt) == "http-5");
    key(&mut rt, Key::Home);
    assert_eq!(cursor(&rt), "creds-1");
    key(&mut rt, Key::PageDown);
    assert!(rt.state.audit.as_ref().unwrap().following, "paging down to the newest follows again");
}

// A folded run opens to its records with Enter or Right, and folds again with Left — from one of
// its records too, which puts the cursor back on the run.
#[test]
fn a_run_opens_and_folds() {
    let mut rt = view();
    key(&mut rt, Key::Up);
    key(&mut rt, Key::Up);
    key(&mut rt, Key::Enter);
    let want: Vec<(String, bool)> = [("creds-1", false), ("http-1", false), ("http-1", true), ("http-2", true), ("http-3", true), ("execs-1", false), ("http-4", false)]
        .iter().map(|(id, m)| (id.to_string(), *m)).collect();
    assert_eq!(rows(&rt), want);
    assert_eq!(cursor(&rt), "http-1", "the cursor stays on the run");
    key(&mut rt, Key::Down);
    key(&mut rt, Key::Down);
    assert_eq!(cursor(&rt), "http-2");
    arrives(&mut rt, "http-5");
    assert_eq!(cursor(&rt), "http-2", "an opened run stays open, and the row held, as records arrive");
    key(&mut rt, Key::Left);
    assert_eq!(rows(&rt).len(), 5, "folded again");
    assert_eq!(cursor(&rt), "http-1", "folding from a record puts the cursor on its run");
    key(&mut rt, Key::Right);
    assert_eq!(rows(&rt).len(), 8);
}

// Enter on a record reads it in full, over the window, with the recordings it offers; one of
// those is read over the record, and Esc goes back a step at a time: to the record, to the view,
// to the workspace the view covered.
#[test]
fn a_record_is_read_and_its_recordings_over_it() {
    let mut rt = view();
    key(&mut rt, Key::Enter);
    let v = rt.state.audit.as_ref().unwrap();
    let top = v.reading.last().expect("the record, read");
    assert_eq!((top.text.title.as_str(), top.text.record.as_str()), ("Audit record", "http-4"));
    let labels: Vec<_> = top.text.recordings.iter().map(|r| r.label.as_str()).collect();
    assert_eq!(labels, ["response body", "request body"]);
    key(&mut rt, Key::Right);
    key(&mut rt, Key::Enter);
    let v = rt.state.audit.as_ref().unwrap();
    assert_eq!(v.reading.len(), 2);
    assert_eq!(v.reading[1].text.title, "Request body");
    key(&mut rt, Key::Escape);
    assert_eq!(rt.state.audit.as_ref().unwrap().reading.len(), 1, "Esc goes back to the record");
    key(&mut rt, Key::Escape);
    assert!(rt.state.audit.as_ref().is_some_and(|v| v.reading.is_empty()), "then to the view");
    key(&mut rt, Key::Escape);
    assert!(rt.state.audit.is_none() && rt.state.workspace_shown(), "then to the workspace");
}

// A record with nothing recorded offers nothing to read; a recording that lands after the record
// it was read from has gone is for nobody.
#[test]
fn a_recording_for_a_record_gone_is_dropped() {
    let mut rt = view();
    key(&mut rt, Key::Up);
    key(&mut rt, Key::Enter);
    let top = rt.state.audit.as_ref().unwrap().reading.last().expect("the record").text.clone();
    assert_eq!(top.record, "execs-1");
    assert!(top.recordings.is_empty());
    key(&mut rt, Key::Escape);
    key(&mut rt, Key::Down);
    key(&mut rt, Key::Enter);
    // The body is asked for, and the record closed before it lands.
    crate::audit::reading_key(&mut rt.state, &KeyEvent { key: Key::Enter, mods: Mods::default() });
    crate::audit::reading_key(&mut rt.state, &KeyEvent { key: Key::Escape, mods: Mods::default() });
    settle(&mut rt);
    assert!(rt.state.audit.as_ref().unwrap().reading.is_empty(), "a body landed with its record gone");
}

// What the last read could not reach is said, and so is a read that failed whole; the timeline
// it had stands.
#[test]
fn what_could_not_be_read_is_said() {
    let mut rt = view();
    let sub = rt.state.audit.as_ref().unwrap().sub;
    let mut a = mock_audit();
    a.missing = vec!["the execs trail could not be read".into()];
    rt.state.inbox.push(Event::Audit(sub, Ok(a)));
    rt.state.inbox.push(Event::Audit(sub, Err("the audit trails could not be read: no route".into())));
    settle(&mut rt);
    let v = rt.state.audit.as_ref().unwrap();
    assert_eq!(v.timeline.missing, ["the execs trail could not be read"]);
    assert_eq!(v.error.as_deref(), Some("the audit trails could not be read: no route"));
    assert_eq!(v.rows.len(), 4, "the timeline stands");
    // A snapshot from a subscription the view no longer follows is dropped.
    rt.state.inbox.push(Event::Audit(sub + 100, Ok(Default::default())));
    settle(&mut rt);
    assert_eq!(rt.state.audit.as_ref().unwrap().rows.len(), 4);
}

// A box taking the screen takes it from the view, and the follow ends with it.
#[test]
fn a_box_taking_the_screen_closes_the_view() {
    let mut rt = view();
    let first = rt.state.live.clone().unwrap();
    rt.state.open_row(1);
    settle(&mut rt);
    assert!(rt.state.audit.is_none() && rt.state.live.as_deref() != Some(first.as_str()) && rt.state.workspace_shown());
}

// Before any read the view says what the core says, and follows; once a read fails whole, the
// failure is said in its place, and the view no longer says it follows.
#[test]
fn an_empty_timeline_says_what_the_core_says() {
    let mut rt = runtime(State::new());
    key(&mut rt, Key::Enter);
    let sandbox = rt.state.live.clone().unwrap();
    audit::open(&mut rt.state, &sandbox);
    // The mock answers as the bridge does: what it says before a read, then the read's records.
    let mut events = rt.state.inbox.drain().into_iter();
    let Some(Event::Audit(sub, first)) = events.next() else { panic!("no first snapshot") };
    rt.state.inbox.push(Event::Audit(sub, first));
    settle(&mut rt);
    let v = rt.state.audit.as_ref().unwrap();
    assert!(v.rows.is_empty() && v.timeline.note == "reading the audit trails…");
    assert!(rt.find("audit-note").is_some() && rt.find("audit-following").is_some());
    rt.state.inbox.push(Event::Audit(sub, Err("the audit trails could not be read: no route".into())));
    settle(&mut rt);
    assert!(rt.find("audit-note").is_none(), "the note still drawn over the failure");
    assert!(rt.find("audit-following").is_none(), "a failed follow still says it follows");
    assert!(rt.find("audit-gap-0").is_some(), "the failure is not said");
}

// Opening the harnesses page takes the audit view down, and its follow with it: Esc on the page
// leaves to the workspace, never back to the view.
#[test]
fn a_page_opening_closes_the_view() {
    let mut rt = view();
    let sub = rt.state.audit.as_ref().unwrap().sub;
    run_command(&mut rt.state, "harnesses", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.harnesses.is_some() && rt.state.audit.is_none());
    // What the old follow still sends is for nobody.
    rt.state.inbox.push(Event::Audit(sub, Ok(mock_audit())));
    settle(&mut rt);
    assert!(rt.state.audit.is_none());
    key(&mut rt, Key::Escape);
    assert!(rt.state.harnesses.is_none() && rt.state.audit.is_none() && rt.state.workspace_shown());
}

// A second Enter before the first read lands reads the record once; a read that lands after
// something else has come in front is for nobody, and takes back what it said it was doing.
#[test]
fn a_read_lands_only_on_what_asked_it() {
    let mut rt = view();
    crate::audit::page_key(&mut rt.state, &KeyEvent { key: Key::Enter, mods: Mods::default() });
    crate::audit::page_key(&mut rt.state, &KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    assert_eq!(rt.state.audit.as_ref().unwrap().reading.len(), 1, "the record was stacked twice");
    key(&mut rt, Key::Escape);
    crate::audit::page_key(&mut rt.state, &KeyEvent { key: Key::Enter, mods: Mods::default() });
    assert_eq!(rt.state.message.as_deref(), Some("reading http-4…"));
    rt.state.open_commands_menu(MenuKind::Bar(crate::wire::Menu::Box), (0.0, 0.0));
    settle(&mut rt);
    assert!(rt.state.menu.is_some() && rt.state.audit.as_ref().unwrap().reading.is_empty(), "the record landed over the menu");
    assert_eq!(rt.state.message, None, "the status line still says it is reading");
    // What somebody else said since is never taken back by a read landing.
    rt.state.close_overlays();
    crate::audit::page_key(&mut rt.state, &KeyEvent { key: Key::Enter, mods: Mods::default() });
    rt.state.message = Some("something else".into());
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("something else"));
    assert_eq!(rt.state.audit.as_ref().unwrap().reading.len(), 1);
}

// A recording being read, and one that could not be, is said on the record's panel, which is all
// that is in front: never only on the status line behind it.
#[test]
fn a_recordings_read_is_said_on_its_panel() {
    let mut rt = view();
    key(&mut rt, Key::Enter);
    // The mock records no stream: asking for one fails, as a recording the pool did not keep.
    let v = rt.state.audit.as_mut().unwrap();
    let top = v.reading.last_mut().unwrap();
    top.text.recordings.push(crate::wire::AuditRecording { part: crate::wire::AuditPart::Stream, label: "stream".into() });
    top.picked = 2;
    crate::audit::reading_key(&mut rt.state, &KeyEvent { key: Key::Enter, mods: Mods::default() });
    let status = rt.state.audit.as_ref().unwrap().reading.last().unwrap().status.clone();
    assert_eq!(status, Some(("reading the stream…".to_string(), false)));
    settle(&mut rt);
    let v = rt.state.audit.as_ref().unwrap();
    assert_eq!(v.reading.len(), 1);
    assert_eq!(v.reading[0].status, Some(("stream: not recorded".to_string(), true)));
    assert!(rt.find("audit-reading-status").is_some(), "the panel does not say why");
}

// The secrets page and the audit view are one page at a time: each opening takes the other down,
// the audit view's follow with it, and the one up is the one drawn.
#[test]
fn the_secrets_page_and_the_view_take_the_screen_from_each_other() {
    let mut rt = view();
    let key = rt.state.live.clone().unwrap();
    run_command(&mut rt.state, "credentials", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.secrets.is_some() && rt.state.audit.is_none(), "the secrets page left the audit view up");
    assert!(rt.find("audit-page").is_none());
    audit::open(&mut rt.state, &key);
    rt.state.inbox.push(Event::Audit(rt.state.audit.as_ref().unwrap().sub, Ok(mock_audit())));
    settle(&mut rt);
    assert!(rt.state.secrets.is_none() && rt.state.audit.is_some(), "the audit view left the secrets page up");
    assert!(rt.find("audit-page").is_some(), "the audit view is up but another page is drawn");
}
