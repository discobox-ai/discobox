//! The read-only cards' own keys (#958), as the TUI's text cards have them: the help, a
//! harness's config card and an audit record found through, stepped along, copied and scrolled,
//! on the mock.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use super::*;
use crate::{on_key, poll, run_command, theme, view};

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

fn key(rt: &mut Runtime<State>, key: Key) {
    rt.key(KeyEvent { key, mods: Mods::default() });
    settle(rt);
}

fn typed(rt: &mut Runtime<State>, text: &str) {
    for c in text.chars() {
        key(rt, Key::Text(c.to_string()));
    }
}

/// How many matches the card in front holds for what its find holds.
fn found(rt: &Runtime<State>) -> usize {
    let f = front(&rt.state).expect("a read-only card in front");
    counts(&f.pieces, &rt.state.reader.find.text).iter().sum()
}

// A match is found whatever its letters' case, as byte ranges in its piece.
#[test]
fn ranges_fold_case() {
    assert_eq!(ranges("Stop the Box, then stop", "stop"), vec![0..4, 19..23]);
    assert!(ranges("anything", "").is_empty());
}

// The help is found through: `/` opens the find, what is typed is lit and counted, Enter gives
// the keys back, and n and N step along the matches, wrapping; Esc then closes the help.
#[test]
fn the_help_is_found_through_and_stepped_along() {
    let mut rt = runtime();
    run_command(&mut rt.state, "help", &mut Tx::default());
    settle(&mut rt);
    key(&mut rt, Key::Text("/".into()));
    assert!(rt.state.reader.find.focused, "/ did not open the find");
    typed(&mut rt, "box");
    let n = found(&rt);
    assert!(n > 1, "the help's commands hold {n} matches of box");
    assert!(rt.find("read-found").is_some(), "the match stepped to is not lit");
    assert_eq!(rt.state.reader.at, 0);
    key(&mut rt, Key::Enter);
    assert!(!rt.state.reader.find.focused && rt.state.reader.find.text == "box", "Enter lost the find");
    key(&mut rt, Key::Text("n".into()));
    assert_eq!(rt.state.reader.at, 1);
    key(&mut rt, Key::Text("N".into()));
    key(&mut rt, Key::Text("N".into()));
    assert_eq!(rt.state.reader.at % n, n - 1, "N did not wrap to the last match");
    key(&mut rt, Key::Escape);
    assert!(!rt.state.help, "Esc did not close the help");
}

// The copy copies the card in front whole, and says so; a card put up in its place starts the
// find again.
#[test]
fn a_card_is_copied_and_another_starts_again() {
    let mut rt = runtime();
    run_command(&mut rt.state, "help", &mut Tx::default());
    settle(&mut rt);
    key(&mut rt, Key::Text("/".into()));
    typed(&mut rt, "stop");
    key(&mut rt, Key::Enter);
    key(&mut rt, Key::Text("c".into()));
    assert_eq!(rt.state.reader.said.as_deref(), Some("copied"));
    let copy = front(&rt.state).unwrap().copy;
    assert!(copy.contains("Box\tstop"), "the help's copy: {copy}");
    key(&mut rt, Key::Escape);
    run_command(&mut rt.state, "harnesses", &mut Tx::default());
    settle(&mut rt);
    key(&mut rt, Key::Enter);
    assert!(rt.state.card.is_some(), "the config card");
    assert!(rt.state.reader.find.text.is_empty(), "the help's find followed onto the card");
}

// The line keys scroll a card a line at a time on from wherever it is scrolled to, the page
// keys' scroll included, and Home goes back to its top.
#[test]
fn the_line_keys_scroll_on_from_where_the_card_is() {
    let mut rt = runtime();
    let rows = (0..120).map(|i| crate::wire::CardRow { label: format!("row {i}"), lines: vec![format!("line {i}")], ..Default::default() }).collect();
    rt.update(|s, _| s.card = Some(crate::harnesses::Card { title: "long".into(), rows }));
    settle(&mut rt);
    let at = |rt: &Runtime<State>| rt.state.reader.offset.get();
    assert_eq!(at(&rt), 0.0);
    key(&mut rt, Key::Down);
    assert_eq!(at(&rt), LINE, "Down did not scroll a line");
    key(&mut rt, Key::Text("j".into()));
    key(&mut rt, Key::Text("k".into()));
    assert_eq!(at(&rt), LINE, "j then k did not come back");
    key(&mut rt, Key::PageDown);
    let paged = at(&rt);
    assert!(paged > LINE, "Page Down did not scroll: {paged}");
    key(&mut rt, Key::Down);
    assert_eq!(at(&rt), paged + LINE, "Down after Page Down went from the line keys' own place");
    key(&mut rt, Key::Home);
    assert_eq!(at(&rt), 0.0);
}

// A harness's config card is found through, and Esc still closes it once the find is let go.
#[test]
fn the_config_card_is_found_through() {
    let mut rt = runtime();
    run_command(&mut rt.state, "harnesses", &mut Tx::default());
    settle(&mut rt);
    key(&mut rt, Key::Enter);
    key(&mut rt, Key::Text("/".into()));
    typed(&mut rt, "theme");
    assert!(found(&rt) >= 1, "the card's files hold theme");
    assert!(rt.find("read-found").is_some());
    key(&mut rt, Key::Escape);
    assert!(rt.state.reader.find.text.is_empty() && rt.state.card.is_some(), "Esc in the find closed the card");
    key(&mut rt, Key::Escape);
    assert!(rt.state.card.is_none(), "Esc did not close the card");
}

// An audit record read in full is found through like the others; its recordings keep their
// keys.
#[test]
fn an_audit_record_is_found_through() {
    let mut rt = runtime();
    key(&mut rt, Key::Enter);
    run_command(&mut rt.state, "audit", &mut Tx::default());
    settle(&mut rt);
    key(&mut rt, Key::Enter);
    assert!(rt.state.audit.as_ref().is_some_and(|v| !v.reading.is_empty()), "the record");
    key(&mut rt, Key::Text("/".into()));
    typed(&mut rt, "http");
    assert!(found(&rt) >= 1, "the record says nothing of http");
    assert!(rt.find("read-found").is_some());
    key(&mut rt, Key::Enter);
    key(&mut rt, Key::Right);
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.audit.as_ref().unwrap().reading.len(), 2, "the recordings lost their keys");
    assert!(rt.state.reader.find.text.is_empty(), "the record's find followed onto its recording");
}
