//! The harnesses page, a harness's card and actions, a file edited through the person's editor,
//! and a box's harness changes reviewed, saved and discarded: on the mock, which answers as the
//! core does.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::backend::Out;
use crate::sheet::{OPENED, Sheet};
use crate::wire::{HarnessAction, HarnessChange, Submit};
use crate::{MenuKind, State, harnesses, on_key, poll, run_command, theme, view};

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

/// The harnesses page, opened by its command.
fn page() -> Runtime<State> {
    let mut rt = runtime();
    run_command(&mut rt.state, "harnesses", &mut Tx::default());
    settle(&mut rt);
    rt
}

fn names(s: &State) -> Vec<String> { s.harnesses.as_ref().unwrap().rows.iter().map(|h| h.name.clone()).collect() }

fn current(s: &State) -> String { s.harnesses.as_ref().and_then(|p| p.current()).map(|h| h.name.clone()).unwrap_or_default() }

// The page is the core's harnesses, the keys on the default; the commands are asked from a page
// beside the list, and attaching a box puts its workspace back in the page's place.
#[test]
fn the_page_lists_the_cores_harnesses() {
    let mut rt = page();
    assert_eq!(names(&rt.state), ["Claude", "Codex", "OpenCode"]);
    assert_eq!(current(&rt.state), "Claude");
    assert!(rt.state.context().page_shown, "the core is not told a page is up");
    key(&mut rt, Key::Down);
    assert_eq!(current(&rt.state), "Codex");
    key(&mut rt, Key::Escape);
    assert!(rt.state.harnesses.is_none(), "Esc leaves the page");

    let mut rt = page();
    run_command(&mut rt.state, "attach", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.harnesses.is_none() && rt.state.live.is_some(), "a box attached takes the page's place");
}

// A harness's menu is its actions as the core offers them, one that does not apply with why;
// taking one acts on that harness, and the page is the harnesses read again after it.
#[test]
fn actions_are_the_cores_and_the_page_follows_them() {
    let mut rt = page();
    harnesses::open_menu(&mut rt.state, (400.0, 200.0));
    let m = rt.state.menu.as_ref().expect("the harness's menu");
    assert_eq!(m.kind, MenuKind::Harness("hc_claude".into()));
    let default = m.entries.iter().find(|e| e.command == "default").unwrap();
    assert!(!default.enabled && default.why == "already the default", "{default:?}");
    rt.state.close_overlays();

    harnesses::act(&mut rt.state, HarnessAction::Default, String::new(), (0.0, 0.0));
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("cannot default Claude: already the default"));

    key(&mut rt, Key::Down);
    harnesses::open_menu(&mut rt.state, (400.0, 200.0));
    let i = rt.state.menu.as_ref().unwrap().entries.iter().position(|e| e.command == "default").unwrap();
    rt.state.menu.as_mut().unwrap().picked = i;
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.message.as_deref(), Some("Codex is now the default"));
    let p = rt.state.harnesses.as_ref().unwrap();
    assert!(p.rows[1].default && !p.rows[0].default, "the page did not take the harnesses read again");
    assert_eq!(current(&rt.state), "Codex", "the keys left the harness acted on");
}

// Enter reads the harness's config, drawn as the core's card until it is closed.
#[test]
fn enter_shows_the_config_card() {
    let mut rt = page();
    key(&mut rt, Key::Enter);
    let card = rt.state.card.as_ref().expect("the config card");
    assert_eq!(card.title, "Claude");
    assert!(card.rows.iter().any(|r| r.label == "Files" && r.lines.iter().any(|l| l.contains("theme"))), "{:?}", card.rows);
    key(&mut rt, Key::Escape);
    assert!(rt.state.card.is_none() && rt.state.harnesses.is_some(), "Esc closes the card, not the page");
}

// A reset asks first, as the core asks it: a harness with a person's own files is a pick of
// one, keeping them first; one without is a yes or no. Leaving either resets nothing.
#[test]
fn a_reset_asks_first() {
    let mut rt = page();
    harnesses::act(&mut rt.state, HarnessAction::Reset, String::new(), (0.0, 0.0));
    settle(&mut rt);
    let q = rt.state.sheets.question().expect("the reset's question");
    assert!(q.question.body.starts_with("Reset Claude"), "{}", q.question.body);
    let keys: Vec<&str> = q.question.choices.iter().map(|c| c.key.as_str()).collect();
    assert_eq!(keys, ["reset", "reset with your files"], "a harness with your files keeps them first");
    key(&mut rt, Key::Escape);
    assert!(!rt.state.sheets.is_open() && rt.state.message.is_none(), "a reset left reset something: {:?}", rt.state.message);

    harnesses::act(&mut rt.state, HarnessAction::Reset, String::new(), (0.0, 0.0));
    settle(&mut rt);
    key(&mut rt, Key::Down);
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.message.as_deref(), Some("reset Claude to its bundle's files alone"));

    // Codex carries no files of yours: a yes or no.
    key(&mut rt, Key::Down);
    harnesses::act(&mut rt.state, HarnessAction::Reset, String::new(), (0.0, 0.0));
    settle(&mut rt);
    assert!(rt.state.sheets.question().is_some_and(|q| q.question.choices.is_empty()));
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.message.as_deref(), Some("reset Codex to its bundle's defaults, keeping your files"));
}

// The page takes the keys from a workspace it covers: a letter typed goes to no terminal, and
// a box command acts on the list's choice, not on the box behind the page.
#[test]
fn the_page_takes_the_keys_from_the_workspace_it_covers() {
    let mut rt = runtime();
    run_command(&mut rt.state, "attach", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.focus, crate::Focus::Panes);
    run_command(&mut rt.state, "harnesses", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.harnesses.is_some());
    assert_eq!(rt.state.focus, crate::Focus::List, "the page did not take the keys");
    let c = rt.state.context();
    assert!(!c.pane_focused && !c.workspace_shown && c.page_shown, "{c:?}");
}

// The page's actions go to the server it was read from, whatever the list's filter says since.
#[test]
fn the_page_keeps_the_server_it_was_read_from() {
    let mut rt = runtime();
    run_command(&mut rt.state, "harnesses", &mut Tx::default());
    rt.state.listing.filter.server = "elsewhere".into();
    settle(&mut rt);
    assert_eq!(rt.state.harnesses.as_ref().unwrap().server, "");
}

// A file is picked from the harness's files and opened in the person's editor; Done reads back
// what they saved there, and Cancel saves nothing.
#[test]
fn a_file_is_edited_in_the_persons_editor() {
    let mut rt = page();
    harnesses::act(&mut rt.state, HarnessAction::Files, String::new(), (0.0, 0.0));
    let m = rt.state.menu.as_ref().expect("the file picker");
    assert_eq!(m.entries.iter().map(|e| e.label.as_str()).collect::<Vec<_>>(), [".claude.json", ".claude/settings.json"]);
    assert!(m.entries[0].detail.ends_with("saved from a discobox · over the image's"), "{}", m.entries[0].detail);
    key(&mut rt, Key::Enter);

    let Some(Sheet::Edit(e)) = rt.state.sheets.queue.first() else { panic!("no edit sheet") };
    let path = e.edit.path.clone();
    assert!(e.edit.title.starts_with("Editing .claude.json"), "{}", e.edit.title);
    assert!(OPENED.with_borrow(|o| o.contains(&path)), "the file was not opened in the editor");
    std::fs::write(&path, "{}\n").unwrap();
    key(&mut rt, Key::Enter);
    assert!(!rt.state.sheets.is_open());
    assert_eq!(rt.state.message.as_deref(), Some("updated .claude.json"));

    harnesses::act(&mut rt.state, HarnessAction::Files, ".claude.json".into(), (0.0, 0.0));
    settle(&mut rt);
    assert!(rt.state.sheets.edit_sheet().is_some());
    key(&mut rt, Key::Escape);
    assert!(!rt.state.sheets.is_open() && rt.state.message.is_none(), "{:?}", rt.state.message);
}

// A variant is the core's form, and the page is on it once it is made.
#[test]
fn a_variant_is_made_through_its_form() {
    let mut rt = page();
    harnesses::act(&mut rt.state, HarnessAction::Variant, String::new(), (0.0, 0.0));
    settle(&mut rt);
    let form = rt.state.sheets.form().expect("the variant's form").id;
    let answers = [("name", "Claude work"), ("slug", "claude-work")].map(|(k, v)| (k.to_string(), v.to_string())).into();
    rt.state.backend.send(Out::Submit(Submit { form, answers, ..Default::default() }));
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("created variant \"Claude work\""));
    assert!(current(&rt.state).starts_with("Claude work"), "{}", current(&rt.state));
}

// A box's harness changes are reviewed as the core reads them; saving names the candidate the
// review showed, and the review goes once it is answered.
#[test]
fn harness_changes_are_reviewed_and_saved() {
    let mut rt = runtime();
    run_command(&mut rt.state, "review config", &mut Tx::default());
    settle(&mut rt);
    let rv = rt.state.review.as_ref().expect("the review");
    assert_eq!((rv.changes.candidate.as_str(), rv.changes.files.len()), ("hcc_7f3a", 1));
    assert!(rv.changes.files[0].diff.contains("+  \"model\": \"opus\""));
    assert_eq!(rv.picked, rv.changes.offers.len(), "the review opened on saving, which Enter would take");
    harnesses::change(&mut rt.state, HarnessChange::Save);
    settle(&mut rt);
    assert!(rt.state.review.is_none());
    assert_eq!(rt.state.message.as_deref(), Some("harness changes: saved"));

    // Read again, the box has nothing left to save: neither offer acts, and each says why.
    run_command(&mut rt.state, "review config", &mut Tx::default());
    settle(&mut rt);
    let rv = rt.state.review.as_ref().unwrap();
    assert!(!rv.changes.nothing.is_empty() && rv.changes.offers.iter().all(|o| !o.enabled && !o.why.is_empty()), "{:?}", rv.changes.offers);
    key(&mut rt, Key::Escape);
    assert!(rt.state.review.is_none());
}
