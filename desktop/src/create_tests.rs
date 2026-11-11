//! Creating a box (#810): the composer, the create card and the run options as the core's forms,
//! the create's steps, the box it made and its workspace, and the composer's draft — driven
//! through the keys on the mock, which answers as the core does, roughly.

use std::sync::{Arc, Mutex};

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::backend::{Inbox, Mock, MockRecord, bundled_spec};
use crate::{Focus, State, on_key, poll, run_command, theme, view};

/// The window on the mock, opening on `draft` as the prompt the last window left, and what the
/// mock was sent.
fn runtime(draft: &str) -> (Runtime<State>, Arc<Mutex<MockRecord>>) {
    let inbox = Inbox::default();
    let mock = Mock::new(inbox.clone());
    let record = mock.record();
    let state = State::with((Box::new(mock), bundled_spec(), inbox, false, crate::wire::Session { draft: draft.to_string(), ..Default::default() }));
    let mut rt = Runtime::with_text(state, view::view, (1440.0, 900.0), Env { system_dark: true, scale: 1.0 }, theme::text_system());
    rt.poll = Some(poll);
    rt.on_key = Some(on_key);
    settle(&mut rt);
    (rt, record)
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

/// Types into the composer, as a person would, once it has the keys.
fn compose(rt: &mut Runtime<State>, text: &str) {
    rt.update(|s, _| s.composer.focused = true);
    settle(rt);
    for c in text.chars() {
        press(rt, Key::Text(c.to_string()), bare());
    }
}

fn field_keys(rt: &Runtime<State>) -> Vec<String> {
    rt.state.sheets.form().expect("a form").fields.iter().map(|f| f.key.clone()).collect()
}

// The composer's prompt, sent with Return, opens the create card: where it is cut from and the
// card's settings, with the command it would run. Creating takes the prompt out of the composer
// the moment the box is taken, and opens the new box's workspace, which says what the box is
// being made to do.
#[test]
fn return_in_the_composer_creates_through_the_card() {
    let (mut rt, record) = runtime("");
    compose(&mut rt, "fix the reaper");
    press(&mut rt, Key::Enter, bare());

    let form = rt.state.sheets.form().expect("the create card");
    assert_eq!((form.title.as_str(), form.action.as_str()), ("New discobox", "Create"));
    assert!(form.note.contains("-p 'fix the reaper'"), "{}", form.note);
    let keys = field_keys(&rt);
    assert!(keys.starts_with(&["source".to_string(), "source:typed".to_string(), "harness".to_string()]), "{keys:?}");

    press(&mut rt, Key::Enter, bare());
    assert!(!rt.state.sheets.is_open(), "the card stayed after the create");
    assert_eq!(rt.state.composer.text, "", "the prompt stayed in the composer once the box was taken");
    let live = rt.state.live.clone().expect("the new box's workspace");
    let made = rt.state.box_by_id(&live).expect("the new box listed");
    assert_eq!(made.name, "fix the reaper");
    assert_eq!(record.lock().unwrap().created, ["fix the reaper"]);
}

// A source typed by hand is refused under its field when it is not there, with what was typed
// kept; a repeated row grows a field as its last is filled, keeping what the others hold.
#[test]
fn the_card_refuses_a_source_and_grows_its_rows() {
    let (mut rt, record) = runtime("");
    run_command(&mut rt.state, "new", &mut Tx::default());
    settle(&mut rt);
    let n = field_keys(&rt).len();
    {
        let f = rt.state.sheets.form().unwrap();
        assert!(!f.fields[1].applies, "the typed source applies before another… is chosen");
    }
    // Choose another…, then type a source that is not there.
    let other = rt.state.sheets.form().unwrap().fields[0].choices.iter().position(|c| c.label == "another…").unwrap();
    for _ in 0..other {
        press(&mut rt, Key::Right, bare());
    }
    assert!(rt.state.sheets.form().unwrap().fields[1].applies, "the typed source does not apply once another… is chosen");
    press(&mut rt, Key::Tab, bare());
    for c in "nowhere".chars() {
        press(&mut rt, Key::Text(c.to_string()), bare());
    }
    press(&mut rt, Key::Enter, bare());
    let f = rt.state.sheets.form().expect("the card stays up, refused");
    assert_eq!(f.error("source:typed"), Some("nowhere is not a directory"));
    assert_eq!(f.edits[1].text, "nowhere");

    // A skills directory typed into its row grows another, and the typed source is kept.
    let skills = field_keys(&rt).iter().position(|k| k == "skills:0").unwrap();
    while rt.state.sheets.form().unwrap().at() != skills {
        press(&mut rt, Key::Tab, bare());
    }
    for c in "./team".chars() {
        press(&mut rt, Key::Text(c.to_string()), bare());
    }
    let keys = field_keys(&rt);
    assert_eq!(keys.len(), n + 1, "{keys:?}");
    assert!(keys.contains(&"skills:1".to_string()), "{keys:?}");
    let f = rt.state.sheets.form().unwrap();
    assert_eq!((f.edits[1].text.as_str(), f.at()), ("nowhere", skills));
    assert!(record.lock().unwrap().created.is_empty());
}

// The run options are a sheet of every row; what is chosen there is what the line under the
// composer says, and what the next create is made with — left detached, it is done, not entered.
#[test]
fn the_run_options_say_what_a_create_is_made_with() {
    let (mut rt, record) = runtime("");
    assert_eq!(rt.state.summary.iter().map(|c| c.text.as_str()).collect::<Vec<_>>(), ["claude"]);
    run_command(&mut rt.state, "options", &mut Tx::default());
    settle(&mut rt);
    let keys = field_keys(&rt);
    for k in ["harness", "dirty", "detach", "env:0"] {
        assert!(keys.contains(&k.to_string()), "the run options lack {k}: {keys:?}");
    }
    // The harness: one along, to codex; then detach, on.
    press(&mut rt, Key::Right, bare());
    let detach = keys.iter().position(|k| k == "detach").unwrap();
    while rt.state.sheets.form().unwrap().at() != detach {
        press(&mut rt, Key::Tab, bare());
    }
    press(&mut rt, Key::Right, bare());
    press(&mut rt, Key::Enter, bare());
    assert!(!rt.state.sheets.is_open());
    assert_eq!(rt.state.message.as_deref(), Some("run options saved"));
    let summary: Vec<_> = rt.state.summary.iter().map(|c| (c.text.as_str(), c.chosen)).collect();
    assert_eq!(summary, [("codex", true), ("detached", true)]);

    compose(&mut rt, "look around");
    press(&mut rt, Key::Enter, bare());
    press(&mut rt, Key::Enter, bare());
    assert!(rt.state.live.is_none(), "a detached create opened its workspace");
    assert_eq!(rt.state.message.as_deref(), Some("created look around, detached"));
    assert_eq!(record.lock().unwrap().created, ["look around"]);
}

// New from a workspace goes back to the composer, with the keys, rather than creating with a
// prompt nobody can see.
#[test]
fn new_from_a_workspace_goes_to_the_composer() {
    let (mut rt, record) = runtime("");
    rt.state.open_row(0);
    settle(&mut rt);
    assert!(rt.state.live.is_some());
    run_command(&mut rt.state, "new", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.live.is_none() && rt.state.focus == Focus::List);
    assert!(rt.state.composer.focused, "the composer does not have the keys");
    assert!(!rt.state.sheets.is_open(), "the card opened over a workspace");
    assert!(record.lock().unwrap().created.is_empty());
}

// The prompt the last window left is back; what is typed is told to the CLI once it has been
// still a moment, and once more on the way out.
#[test]
fn the_draft_comes_back_and_is_kept() {
    let (mut rt, record) = runtime("half a thought");
    assert_eq!(rt.state.composer.text, "half a thought");
    assert_eq!(rt.state.message.as_deref(), Some("the prompt you left is back"));
    compose(&mut rt, "!");
    assert!(record.lock().unwrap().drafts.is_empty(), "the draft was told before it was still");
    for _ in 0..90 {
        rt.frame(1.0 / 60.0);
    }
    assert_eq!(record.lock().unwrap().drafts, ["half a thought!"]);
    run_command(&mut rt.state, "quit", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(record.lock().unwrap().drafts, ["half a thought!", "half a thought!"]);
}

// A harness's own way out typed into the composer leaves the window, creating nothing, with the
// composer emptied for the next window.
#[test]
fn exit_in_the_composer_leaves() {
    let (mut rt, record) = runtime("");
    compose(&mut rt, "/exit");
    press(&mut rt, Key::Enter, bare());
    assert!(!rt.state.sheets.is_open(), "/exit opened the create card");
    let record = record.lock().unwrap();
    assert!(record.created.is_empty());
    assert_eq!(record.drafts.last().map(String::as_str), Some(""));
}

// A create's warning stays on the status line past the steps after it and the workspace it opens.
#[test]
fn a_create_warning_stays() {
    let (mut rt, _) = runtime("");
    run_command(&mut rt.state, "options", &mut Tx::default());
    settle(&mut rt);
    let dirty = field_keys(&rt).iter().position(|k| k == "dirty").unwrap();
    while rt.state.sheets.form().unwrap().at() != dirty {
        press(&mut rt, Key::Tab, bare());
    }
    press(&mut rt, Key::Left, bare());
    press(&mut rt, Key::Enter, bare());
    compose(&mut rt, "x");
    press(&mut rt, Key::Enter, bare());
    press(&mut rt, Key::Enter, bare());
    assert!(rt.state.live.is_some());
    assert_eq!(rt.state.message.as_deref(), Some("uncommitted changes left out"));
}

// A question a create asks after its card is answered is on top: the card the core took goes,
// rather than staying over the question until the create's result lands.
#[test]
fn a_question_after_the_card_is_not_hidden_by_it() {
    use crate::backend::Event;
    use crate::wire::Question;
    let (mut rt, _) = runtime("");
    run_command(&mut rt.state, "new", &mut Tx::default());
    settle(&mut rt);
    let command = rt.state.sheets.form().expect("the card").command;
    // The card submitted, the mock holds its answer back, as a create does while it asks.
    rt.state.sheets.queue.iter_mut().for_each(|s| if let crate::sheet::Sheet::Form(f) = s { f.sent = true });
    rt.state.inbox.push(Event::Question(Question { id: 77, command, title: "Which harness should this project run?".into(), ..Default::default() }));
    settle(&mut rt);
    assert!(rt.state.sheets.form().is_none(), "the answered card stayed over the question");
    assert_eq!(rt.state.sheets.question().map(|q| q.question.id), Some(77));
}

// The sheet's fields scroll, so the keys moving to a field ask for it to be scrolled into view,
// and the next frame sends that once: Tab down to the run options' last row asks for each row.
#[test]
fn the_keys_moving_reveal_their_field() {
    let (mut rt, _) = runtime("");
    run_command(&mut rt.state, "options", &mut Tx::default());
    settle(&mut rt);
    let last = field_keys(&rt).last().cloned().unwrap();
    for _ in 0..field_keys(&rt).len() {
        let mut tx = Tx::default();
        crate::sheet::key(&mut rt.state, &KeyEvent { key: Key::Tab, mods: bare() }, &mut tx);
        let f = rt.state.sheets.form().unwrap();
        let at = f.fields[f.at()].key.clone();
        assert_eq!(f.reveal.as_deref(), Some(at.as_str()), "moving the keys asked for no reveal");
        rt.state.sheets.reveal(&mut tx);
        assert_eq!(rt.state.sheets.form().unwrap().reveal, None, "the reveal was not taken");
        if at == last {
            return;
        }
    }
    panic!("Tab never reached {last}");
}

// A workspace says what its box is being made to do until its primary attaches: the watch
// behind it runs until it is stopped, and the attach connecting is what ends it.
#[test]
fn provisioning_ends_when_the_primary_attaches() {
    use crate::backend::Event;
    use crate::wire::{Stream, StreamState, TabKind};
    let (mut rt, _) = runtime("");
    compose(&mut rt, "fix the reaper");
    press(&mut rt, Key::Enter, bare());
    press(&mut rt, Key::Enter, bare());
    let live = rt.state.live.clone().expect("the new box's workspace");
    let primary = {
        // Still opening: what the watch says, on its subscription.
        let ws = rt.state.workspaces.get_mut(&live).unwrap();
        ws.step = "connecting to the terminal".into();
        ws.provisioning_sub = 99;
        ws.panes.iter().find(|p| p.exec.kind == TabKind::Primary).expect("its primary").stream
    };
    rt.state.inbox.push(Event::Stream(Stream { stream: primary, state: StreamState::Attached, ..Default::default() }));
    settle(&mut rt);
    let ws = rt.state.live_ws().unwrap();
    assert_eq!((ws.step.as_str(), ws.provisioning_sub), ("", 0), "provisioning outlived the primary's attach");
}
