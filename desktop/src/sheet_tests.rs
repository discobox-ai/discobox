//! The core's forms and questions as sheets (#808): rename, settings and purge's confirmation,
//! driven through the keys on the mock, which puts them as the core does.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::backend::Event;
use crate::wire::{Choice, Question, Result as CommandResult, State as BoxState, Status};
use crate::sheet::{QuestionSheet, Sheet, dismissal};
use crate::{Focus, Pending, State, on_key, poll, run_command, run_on, theme, view};

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

fn press(rt: &mut Runtime<State>, key: Key, mods: Mods) {
    rt.key(KeyEvent { key, mods });
    settle(rt);
}

fn bare() -> Mods { Mods::default() }

fn typed(rt: &mut Runtime<State>, text: &str) {
    for c in text.chars() {
        press(rt, Key::Text(c.to_string()), bare());
    }
}

fn erase(rt: &mut Runtime<State>, n: usize) {
    for _ in 0..n {
        press(rt, Key::Backspace, bare());
    }
}

/// The box a row of the list shows, by its name.
fn key_named(rt: &Runtime<State>, name: &str) -> String {
    rt.state.boxes.iter().find(|b| b.name == name).map(|b| b.key.clone()).unwrap_or_else(|| panic!("no box {name}"))
}

/// The field the keys are on, by its key.
fn at(rt: &Runtime<State>) -> String {
    let f = rt.state.sheets.form().expect("a form");
    f.fields[f.at()].key.clone()
}

// Rename opens on the name, takes what is typed, and the core's answer says what came of it.
#[test]
fn rename_is_a_sheet_that_renames() {
    let mut rt = runtime();
    rt.state.focus = Focus::List;
    run_command(&mut rt.state, "rename", &mut Tx::default());
    settle(&mut rt);
    let f = rt.state.sheets.form().expect("the rename sheet");
    assert_eq!((f.title.as_str(), f.edits[0].text.as_str(), f.edits[0].focused), ("Rename", "rune console port", true));
    typed(&mut rt, " two");
    press(&mut rt, Key::Enter, bare());
    assert!(!rt.state.sheets.is_open(), "the sheet stayed after the rename");
    assert_eq!(rt.state.message.as_deref(), Some("renamed to rune console port two"));
    assert!(rt.state.boxes.iter().any(|b| b.name == "rune console port two"), "the list did not take the name");
}

// Esc dismisses a form: the command is canceled and says nothing.
#[test]
fn esc_cancels_a_form() {
    let mut rt = runtime();
    run_command(&mut rt.state, "rename", &mut Tx::default());
    settle(&mut rt);
    typed(&mut rt, "x");
    press(&mut rt, Key::Escape, bare());
    assert!(!rt.state.sheets.is_open());
    assert_eq!(rt.state.message, None);
    assert!(rt.state.boxes.iter().any(|b| b.name == "rune console port"), "a dismissed rename renamed");
}

// Settings: a field that does not apply says why and is stepped over; a picker turns with the
// arrows; the core's refusal is said under its field, with what was typed kept, and the keys go
// there.
#[test]
fn settings_steps_over_what_does_not_apply_and_shows_a_refusal() {
    let mut rt = runtime();
    run_command(&mut rt.state, "settings", &mut Tx::default());
    settle(&mut rt);
    let f = rt.state.sheets.form().expect("the settings sheet");
    let harness = f.fields.iter().find(|x| x.key == "harness").unwrap();
    assert!(!harness.applies && harness.why.contains("stop the box"), "{harness:?}");
    assert_eq!(at(&rt), "name");
    press(&mut rt, Key::Tab, bare());
    assert_eq!(at(&rt), "deny", "Tab did not step over the harness");
    press(&mut rt, Key::Right, bare());
    let f = rt.state.sheets.form().unwrap();
    let deny = f.fields.iter().position(|x| x.key == "deny").unwrap();
    assert_eq!(f.chosen[deny], "yes", "Right did not turn the picker");
    assert!(!f.edits.iter().any(|e| e.focused), "a picker's field kept the caret");
    // Only what the person changed is answered; the core reads the rest as they opened.
    assert_eq!(f.answers(), std::collections::HashMap::from([("deny".to_string(), "yes".to_string())]));
    typed(&mut rt, "z");
    assert_eq!(rt.state.sheets.form().unwrap().edits[0].text, "rune console port", "a key on the picker typed into the name");

    press(&mut rt, Key::Tab, bare());
    press(&mut rt, Key::Tab, bare());
    press(&mut rt, Key::Tab, bare());
    assert_eq!(at(&rt), "name", "Tab did not wrap to the first field");
    erase(&mut rt, 40);
    press(&mut rt, Key::Enter, bare());
    let f = rt.state.sheets.form().expect("the refused form stays up");
    assert_eq!(f.error("name"), Some("a discobox needs a configured name"));
    assert!(!f.sent);
    assert_eq!(f.chosen[deny], "yes", "the refusal lost what was chosen");
    assert_eq!(at(&rt), "name");

    typed(&mut rt, "renamed");
    assert_eq!(rt.state.sheets.form().unwrap().error("name"), None, "the refusal stayed after its field changed");
    press(&mut rt, Key::Enter, bare());
    assert!(!rt.state.sheets.is_open());
    assert_eq!(rt.state.message.as_deref(), Some("settings saved"));
}

// Purge asks first, and Enter on the question is no: a destruction is chosen, never fallen into.
#[test]
fn purge_asks_and_defaults_to_no() {
    let mut rt = runtime();
    let old = key_named(&rt, "old experiment");
    assert_eq!(rt.state.box_by_id(&old).map(|b| b.state), Some(BoxState::Archived));
    run_on(&mut rt.state, "purge", vec![old.clone()], &mut Tx::default());
    settle(&mut rt);
    let q = rt.state.sheets.question().expect("purge's question");
    assert!(q.question.default_no && q.question.body.contains("old experiment"), "{:?}", q.question);
    assert_eq!(q.picked, 1, "the question opened on yes");
    press(&mut rt, Key::Enter, bare());
    assert!(!rt.state.sheets.is_open());
    assert!(rt.state.box_by_id(&old).is_some(), "Enter purged");

    run_on(&mut rt.state, "purge", vec![old.clone()], &mut Tx::default());
    settle(&mut rt);
    press(&mut rt, Key::Left, bare());
    press(&mut rt, Key::Enter, bare());
    assert!(!rt.state.sheets.is_open());
    assert!(rt.state.box_by_id(&old).is_none(), "yes did not purge");
}

// A purge none of its boxes can take is refused before anything is asked.
#[test]
fn purge_of_a_live_box_asks_nothing() {
    let mut rt = runtime();
    let live = key_named(&rt, "rune console port");
    run_on(&mut rt.state, "purge", vec![live], &mut Tx::default());
    settle(&mut rt);
    assert!(!rt.state.sheets.is_open());
    assert!(rt.state.message.as_deref().is_some_and(|m| m.contains("archive it first")), "{:?}", rt.state.message);
}

// A pick of one: the keys move between its choices, Enter answers the one picked, by key; Esc
// declines.
#[test]
fn a_pick_of_one_answers_by_key() {
    let mut rt = runtime();
    let q = |id| Question {
        id, command: 99, title: "Git credentials".into(), body: "github.com asked for credentials".into(),
        choices: vec![Choice { key: "token".into(), label: "use a token".into(), hint: String::new(), ..Default::default() }, Choice { key: "skip".into(), label: "skip".into(), hint: "push nothing".into(), ..Default::default() }],
        default_no: false, ..Default::default()
    };
    rt.state.inbox.push(Event::Question(q(1)));
    rt.state.inbox.push(Event::Question(q(2)));
    settle(&mut rt);
    assert_eq!(rt.state.sheets.queue.len(), 2, "the questions did not queue");
    assert_eq!(rt.state.sheets.question().unwrap().question.id, 1, "the first to arrive is not on top");
    press(&mut rt, Key::Down, bare());
    assert_eq!(rt.state.sheets.question().unwrap().picked, 1);
    press(&mut rt, Key::Enter, bare());
    assert_eq!(rt.state.sheets.question().unwrap().question.id, 2, "the answered question stayed");
    press(&mut rt, Key::Escape, bare());
    assert!(!rt.state.sheets.is_open());
}

// While a sheet is up nothing behind it takes a key: a shortcut runs no command.
#[test]
fn a_sheet_takes_every_key() {
    let mut rt = runtime();
    run_command(&mut rt.state, "rename", &mut Tx::default());
    settle(&mut rt);
    press(&mut rt, Key::Function(1), bare());
    assert!(!rt.state.help, "F1 opened the help behind a sheet");
    assert!(rt.state.sheets.is_open());
}

// A sheet goes when its command finishes, answered or not: what it asked is moot.
#[test]
fn a_finished_command_takes_its_sheet() {
    let mut rt = runtime();
    run_command(&mut rt.state, "rename", &mut Tx::default());
    settle(&mut rt);
    let command = rt.state.sheets.form().unwrap().command;
    rt.state.inbox.push(Event::Result(CommandResult { id: command, status: Status::Failed, why: "the server went away".into(), ..Default::default() }));
    settle(&mut rt);
    assert!(!rt.state.sheets.is_open());
    assert_eq!(rt.state.message.as_deref(), Some("the server went away"));
}

// Dismissed, a yes or no is answered no — leaving it is no, as the core says and the TUI answers
// it — and only a pick of one is declined.
#[test]
fn a_dismissed_yes_or_no_is_no() {
    let sent = |choices: Vec<Choice>| {
        let question = Question { id: 3, command: 1, title: "Upgrade".into(), choices, ..Default::default() };
        serde_json::to_value(dismissal(&Sheet::Question(QuestionSheet { question, picked: 0, since: None, settled: false, held: false })).message()).unwrap()
    };
    assert_eq!(sent(vec![])["answer"], serde_json::json!({"question": 3, "key": "no"}));
    let pick = vec![Choice { key: "a".into(), label: "a".into(), hint: String::new(), ..Default::default() }];
    assert_eq!(sent(pick)["answer"], serde_json::json!({"question": 3, "declined": true}));
}

// A shell whose command was canceled (its upgrade question answered no) lets its stream go and
// opens the tabs held back while it was being made.
#[test]
fn a_canceled_session_lets_its_stream_go() {
    let mut rt = runtime();
    rt.state.pending.insert(77, Pending::Session(9, "b".into(), None));
    rt.state.early.insert(9, vec![Event::Data(9, b"x".to_vec())]);
    rt.state.inbox.push(Event::Result(CommandResult { id: 77, status: Status::Canceled, ..Default::default() }));
    settle(&mut rt);
    assert!(!rt.state.early.contains_key(&9), "the canceled session's early frames were kept");
}

// The copy question is sent again, the same question, as its directory is counted: the sheet up
// is updated where it is, its pick kept, and one already answered is not asked again when its
// last count crosses the answer.
#[test]
fn a_question_sent_again_is_updated_in_place() {
    let mut rt = runtime();
    let q = |count: &str| Question {
        id: 5, command: 4, title: "Copy this directory?".into(), subject: "/tmp/x".into(),
        choices: vec![Choice { key: "true".into(), label: "Copy".into(), hint: String::new(), ..Default::default() }, Choice { key: "false".into(), label: "Do not copy".into(), hint: String::new(), ..Default::default() }],
        count: count.into(), ..Default::default()
    };
    rt.state.inbox.push(Event::Question(q("calculating…")));
    settle(&mut rt);
    press(&mut rt, Key::Down, bare());
    rt.state.inbox.push(Event::Question(q("5.0 MiB in 3 files, still counting…")));
    settle(&mut rt);
    assert_eq!(rt.state.sheets.queue.len(), 1, "the count was asked as another question");
    let up = rt.state.sheets.question().unwrap();
    assert_eq!(up.picked, 1, "the pick moved with the count");
    assert_eq!(up.question.count, "5.0 MiB in 3 files, still counting…");

    press(&mut rt, Key::Enter, bare());
    rt.state.inbox.push(Event::Question(q("7.0 MiB in 4 files")));
    settle(&mut rt);
    assert!(!rt.state.sheets.is_open(), "a count that crossed the answer asked again");
}

/// Lets a question that has come up stand long enough to take a typed answer.
fn stand(rt: &mut Runtime<State>) {
    settle(rt);
    rt.frame(rt.state.spec.answer_quiet);
    settle(rt);
}

// A question is answered by the key the core gives each answer, as the TUI's is (#958): purge's
// yes or no by y and n, a pick of one by its place; one the question offers with why it cannot
// be chosen is pressed by nothing. A key typed the moment it comes up, meant for what was in
// front a moment before, answers nothing.
#[test]
fn a_question_is_answered_by_its_keys() {
    let mut rt = runtime();
    let old = key_named(&rt, "old experiment");
    // Typing goes on as the question comes up.
    rt.key(KeyEvent { key: Key::Down, mods: bare() });
    rt.frame(1.0 / 60.0);
    run_on(&mut rt.state, "purge", vec![old.clone()], &mut Tx::default());
    rt.frame(1.0 / 60.0);
    let y = || KeyEvent { key: Key::Text("y".into()), mods: bare() };
    rt.key(y());
    rt.frame(1.0 / 60.0);
    assert!(rt.state.sheets.is_open() && rt.state.box_by_id(&old).is_some(), "a key typed as the question came up answered it");
    // Typing that runs on into the question, past the time it has been up, keeps it waiting. (A
    // key is taken on the frame after it is sent, so the keys come a step and a bit apart.)
    let quiet = rt.state.spec.answer_quiet;
    for _ in 0..6 {
        rt.frame(quiet * 0.3);
        rt.key(y());
    }
    settle(&mut rt);
    assert!(rt.state.sheets.is_open() && rt.state.box_by_id(&old).is_some(), "a run of typing answered the question");
    assert_eq!(rt.state.sheets.question().unwrap().question.presses.get("yes").map(String::as_str), Some("y"));
    stand(&mut rt);
    typed(&mut rt, "n");
    assert!(!rt.state.sheets.is_open() && rt.state.box_by_id(&old).is_some(), "n did not answer no");
    run_on(&mut rt.state, "purge", vec![old.clone()], &mut Tx::default());
    stand(&mut rt);
    typed(&mut rt, "Y");
    assert!(!rt.state.sheets.is_open() && rt.state.box_by_id(&old).is_none(), "Y did not answer yes");

    // The theme's question offers the palette drawn now shut: its key chooses nothing. (Picking
    // one would repaint every window the tests draw; the harnesses page's reset takes a pick of
    // one by its key, `harness_tests`.)
    run_command(&mut rt.state, "theme", &mut Tx::default());
    stand(&mut rt);
    let q = rt.state.sheets.question().expect("the theme question").question.clone();
    let shut = q.choices.iter().find(|c| !c.why.is_empty()).expect("the palette drawn now, offered shut");
    typed(&mut rt, &q.presses[&shut.key]);
    assert!(rt.state.sheets.is_open(), "the key of an answer that cannot be chosen chose it");
    press(&mut rt, Key::Escape, bare());
}

// Typing begun just after a question comes up holds it too, by someone who has not looked up
// yet; an arrow does not, so one before the answer never does; an answer pressed too soon is
// held, which the sheet says, and taken once pressed again after a pause.
#[test]
fn typing_under_a_question_holds_it_and_arrows_do_not() {
    let mut rt = runtime();
    let old = key_named(&rt, "old experiment");
    run_on(&mut rt.state, "purge", vec![old.clone()], &mut Tx::default());
    rt.frame(1.0 / 60.0);
    let quiet = rt.state.spec.answer_quiet;
    let k = |key: Key| KeyEvent { key, mods: bare() };
    rt.frame(quiet * 0.2);
    rt.key(k(Key::Text("x".into())));
    for _ in 0..5 {
        rt.frame(quiet * 0.3);
        rt.key(k(Key::Text("y".into())));
    }
    rt.frame(1.0 / 60.0);
    assert!(rt.state.sheets.is_open() && rt.state.box_by_id(&old).is_some(), "typing begun under the question answered it");
    assert!(rt.find("sheet-held").is_some(), "the sheet does not say a key was held");
    rt.frame(quiet * 1.2);
    rt.key(k(Key::Down));
    rt.frame(quiet * 0.1);
    rt.key(k(Key::Up));
    rt.frame(quiet * 0.1);
    rt.key(k(Key::Text("y".into())));
    settle(&mut rt);
    assert!(!rt.state.sheets.is_open() && rt.state.box_by_id(&old).is_none(), "y after a pause and the arrows did not answer");
}

// A form's picker takes a choice by the key the core gives it, its place, while the picker has
// the keys; a typed field's are its text.
#[test]
fn a_picker_takes_a_choice_by_its_number() {
    let mut rt = runtime();
    run_command(&mut rt.state, "settings", &mut Tx::default());
    settle(&mut rt);
    typed(&mut rt, "2");
    assert!(rt.state.sheets.form().unwrap().edits[0].text.ends_with('2'), "a number did not type into the name");
    press(&mut rt, Key::Tab, bare());
    assert_eq!(at(&rt), "deny");
    let f = rt.state.sheets.form().unwrap();
    let deny = f.fields.iter().position(|x| x.key == "deny").unwrap();
    let (first, second) = (f.fields[deny].choices[0].clone(), f.fields[deny].choices[1].clone());
    assert_eq!((first.press.as_str(), second.press.as_str()), ("1", "2"));
    typed(&mut rt, "2");
    assert_eq!(rt.state.sheets.form().unwrap().chosen[deny], second.key, "2 did not choose the second");
    typed(&mut rt, "1");
    assert_eq!(rt.state.sheets.form().unwrap().chosen[deny], first.key);
    assert!(rt.state.sheets.is_open(), "a choice submitted the form");
}
