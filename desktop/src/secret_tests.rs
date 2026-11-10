//! The secrets page, a secret read with what is bound to it, a binding read and taken back, a
//! secret bound, edited and deleted through the core's sheets, and a secret's binding history:
//! on the mock, which answers as the core does.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::secrets::{self, Panel};
use crate::wire::{SecretAction, SecretStanding};
use crate::{MenuKind, State, on_key, poll, run_command, theme, view};

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

/// The secrets page, opened by its command.
fn page() -> Runtime<State> {
    let mut rt = runtime();
    run_command(&mut rt.state, "credentials", &mut Tx::default());
    settle(&mut rt);
    rt
}

fn names(s: &State) -> Vec<String> { s.secrets.as_ref().unwrap().rows.iter().map(|x| x.name.clone()).collect() }

fn current(s: &State) -> String { s.secrets.as_ref().and_then(|p| p.current()).map(|x| x.name.clone()).unwrap_or_default() }

// The page is the core's secrets, each row saying where it may go and what stands on it; it is
// a page beside the list, which the commands are asked from, and a box attached takes its place.
#[test]
fn the_page_lists_the_cores_secrets() {
    let mut rt = page();
    assert_eq!(names(&rt.state), ["github", "claude-login", "npm", "openai"]);
    let p = rt.state.secrets.as_ref().unwrap();
    assert_eq!((p.rows[0].standing, p.rows[0].standing_text.as_str()), (SecretStanding::Bound, "2 bindings"));
    assert!(p.rows[2].nowhere && p.rows[2].r#where == "sent nowhere", "{:?}", p.rows[2]);
    assert_eq!(p.rows[3].standing, SecretStanding::Refused);
    assert!(rt.state.context().page_shown, "the core is not told a page is up");
    assert!(!rt.state.workspace_shown());
    key(&mut rt, Key::Down);
    assert_eq!(current(&rt.state), "claude-login");
    key(&mut rt, Key::Escape);
    assert!(rt.state.secrets.is_none(), "Esc leaves the page");

    let mut rt = page();
    run_command(&mut rt.state, "attach", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.secrets.is_none() && rt.state.live.is_some(), "a box attached takes the page's place");

    // One page at a time: the harnesses page takes the secrets page's place.
    let mut rt = page();
    run_command(&mut rt.state, "harnesses", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.secrets.is_none() && rt.state.harnesses.is_some());
}

// The page takes the keys from a workspace it covers: a letter typed reaches no terminal, and a
// box command acts on the list's choice.
#[test]
fn the_page_takes_the_keys_from_the_workspace_it_covers() {
    let mut rt = runtime();
    run_command(&mut rt.state, "attach", &mut Tx::default());
    settle(&mut rt);
    run_command(&mut rt.state, "credentials", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.secrets.is_some());
    assert!(!rt.state.pane_has_keys(), "a pane behind the page has the keys");
    let c = rt.state.context();
    assert!(!c.pane_focused && !c.workspace_shown && c.page_shown, "{c:?}");
}

// Enter reads the secret: the core's card, then what is bound to it, the first row binding it.
// A binding is read in full, and Esc goes back the way it came.
#[test]
fn enter_reads_the_secret_and_its_bindings() {
    let mut rt = page();
    key(&mut rt, Key::Enter);
    assert!(matches!(rt.state.secret_panel, Some(Panel::Secret { ref secret, picked: 0 }) if secret == "sec_gh"));
    key(&mut rt, Key::Down);
    key(&mut rt, Key::Enter);
    assert!(matches!(rt.state.secret_panel, Some(Panel::Binding { ref binding, .. }) if binding == "bind_gh1"));
    key(&mut rt, Key::Escape);
    assert!(matches!(rt.state.secret_panel, Some(Panel::Secret { picked: 1, .. })), "Esc from a binding goes back to its secret, on it");
    key(&mut rt, Key::Escape);
    assert!(rt.state.secret_panel.is_none() && rt.state.secrets.is_some(), "Esc closes the panel, not the page");
}

// Taking a binding back asks first, as the core asks it, with No the quick answer; no takes
// nothing back, yes does, and the panel follows the secrets read again.
#[test]
fn an_unbind_asks_first_and_the_panel_follows() {
    let mut rt = page();
    key(&mut rt, Key::Enter);
    key(&mut rt, Key::Down);
    key(&mut rt, Key::Delete);
    let q = rt.state.sheets.question().expect("the unbind's question");
    assert!(q.question.default_no && q.question.body.contains("as GH_TOKEN"), "{:?}", q.question);
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.secrets.as_ref().unwrap().rows[0].bindings.len(), 2, "Enter on a destruction took it back");

    key(&mut rt, Key::Enter);
    key(&mut rt, Key::Delete);
    key(&mut rt, Key::Left);
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.message.as_deref(), Some("unbound"));
    let gh = &rt.state.secrets.as_ref().unwrap().rows[0];
    assert_eq!((gh.bindings.len(), gh.standing_text.as_str()), (1, "1 binding"));
    assert!(matches!(rt.state.secret_panel, Some(Panel::Secret { .. })), "the panel left the secret");
}

// A binding is the core's form, from the secret's first row; what it made is on the page after.
#[test]
fn a_secret_is_bound_through_the_cores_form() {
    let mut rt = page();
    key(&mut rt, Key::Down);
    key(&mut rt, Key::Down);
    key(&mut rt, Key::Enter);
    key(&mut rt, Key::Enter);
    let f = rt.state.sheets.form().expect("the binding's form");
    assert_eq!(f.title, "Bind npm");
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.message.as_deref(), Some("bound npm as NPM"));
    let npm = &rt.state.secrets.as_ref().unwrap().rows[2];
    assert_eq!((npm.standing, npm.bindings.len()), (SecretStanding::Bound, 1));
    assert_eq!(current(&rt.state), "npm", "the keys left the secret acted on");
}

// The page's menu is its actions; editing and deleting go through the core's sheets, and a
// delete asks first.
#[test]
fn the_menu_edits_and_deletes_through_the_cores_sheets() {
    let mut rt = page();
    secrets::open_menu(&mut rt.state, (400.0, 200.0));
    let m = rt.state.menu.as_ref().expect("the page's menu");
    assert_eq!(m.kind, MenuKind::Secret("sec_gh".into()));
    let commands: Vec<&str> = m.entries.iter().map(|e| e.command.as_str()).collect();
    assert_eq!(commands, ["secret:open", "secret:bind", "secret:edit", "secret:history", "secret:delete", "secret:new", "secret:read again"]);
    rt.state.close_overlays();

    secrets::act(&mut rt.state, SecretAction::Edit, String::new());
    settle(&mut rt);
    assert_eq!(rt.state.sheets.form().map(|f| f.title.clone()).as_deref(), Some("Edit github"));
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.message.as_deref(), Some("github is unchanged"));

    secrets::act(&mut rt.state, SecretAction::Delete, String::new());
    settle(&mut rt);
    assert!(rt.state.sheets.question().is_some_and(|q| q.question.default_no));
    key(&mut rt, Key::Escape);
    assert_eq!(names(&rt.state).len(), 4, "a delete left deleted something");
    secrets::act(&mut rt.state, SecretAction::Delete, String::new());
    settle(&mut rt);
    key(&mut rt, Key::Left);
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.message.as_deref(), Some("deleted github"));
    assert_eq!(names(&rt.state), ["claude-login", "npm", "openai"]);
}

// A new secret is the core's form, and stays up with what was typed when it is refused.
#[test]
fn a_new_secret_is_stored_through_the_cores_form() {
    let mut rt = page();
    secrets::act(&mut rt.state, SecretAction::New, String::new());
    settle(&mut rt);
    assert_eq!(rt.state.sheets.form().map(|f| f.title.clone()).as_deref(), Some("New secret"));
    key(&mut rt, Key::Enter);
    assert!(rt.state.sheets.form().is_some(), "a refused form went away");
    for c in "slack".chars() {
        rt.key(KeyEvent { key: Key::Text(c.to_string()), mods: Mods::default() });
    }
    settle(&mut rt);
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.message.as_deref(), Some("stored slack"));
    assert!(names(&rt.state).contains(&"slack".to_string()));
}

// The history is the core's decisions, each read in full and back to the list; a secret with
// none says so.
#[test]
fn the_history_reads_each_decision() {
    let mut rt = page();
    secrets::act(&mut rt.state, SecretAction::History, String::new());
    settle(&mut rt);
    let Some(Panel::History { entries, .. }) = rt.state.secret_panel.as_ref() else { panic!("no history") };
    assert_eq!(entries.len(), 2);
    key(&mut rt, Key::Down);
    key(&mut rt, Key::Enter);
    assert!(matches!(rt.state.secret_panel, Some(Panel::Decision { at: 1, .. })));
    key(&mut rt, Key::Escape);
    assert!(matches!(rt.state.secret_panel, Some(Panel::History { picked: 1, .. })), "Esc from a decision goes back to the history");
    key(&mut rt, Key::Escape);

    key(&mut rt, Key::Down);
    secrets::act(&mut rt.state, SecretAction::History, String::new());
    settle(&mut rt);
    assert!(rt.state.secret_panel.is_none());
    assert_eq!(rt.state.message.as_deref(), Some("no binding history"));
}

// refresh reads the page again along with the boxes: a secret stored elsewhere since reaches it,
// and the keys stay on the secret they were on.
#[test]
fn refresh_reads_the_page_again() {
    let mut rt = page();
    key(&mut rt, Key::End);
    // Stored from elsewhere: the page has not read it yet.
    secrets::act(&mut rt.state, SecretAction::New, String::new());
    settle(&mut rt);
    for c in "zeta".chars() {
        rt.key(KeyEvent { key: Key::Text(c.to_string()), mods: Mods::default() });
    }
    settle(&mut rt);
    key(&mut rt, Key::Enter);
    rt.state.secrets.as_mut().unwrap().rows.pop();
    assert_eq!(names(&rt.state).len(), 4);
    run_command(&mut rt.state, "refresh", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(names(&rt.state), ["github", "claude-login", "npm", "openai", "zeta"], "refresh did not read the page again");
    assert_eq!(current(&rt.state), "openai", "the keys moved on a read again");
}

// The page's words, and what a form is about, are the core's, as the bridge sends them.
#[test]
fn the_page_says_the_cores_words() {
    let mut rt = page();
    assert_eq!(rt.state.secrets.as_ref().unwrap().words.bind, "Bind this secret…");
    key(&mut rt, Key::End);
    secrets::act(&mut rt.state, SecretAction::Edit, String::new());
    settle(&mut rt);
    let about = rt.state.sheets.form().map(|f| f.about.clone()).unwrap_or_default();
    assert!(about.starts_with("refused at api.openai.com"), "a refused secret's edit does not say why: {about:?}");
}

// Bind from a secret's panel binds that secret, whatever the page's keys are on.
#[test]
fn a_panel_acts_on_its_own_secret() {
    let mut rt = page();
    key(&mut rt, Key::Down);
    key(&mut rt, Key::Enter);
    rt.state.secrets.as_mut().unwrap().cursor = 0;
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.sheets.form().map(|f| f.title.clone()).as_deref(), Some("Bind claude-login"));
}
