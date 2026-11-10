//! Requests and approvals: a box's notice and its dismissal, a request answered through the
//! core's card on a sheet, the secrets page's requests table, and a request that arrived raised
//! as a notification that answers it: on the mock, which answers as the core does.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::backend::Event;
use crate::notify::RAISED;
use crate::wire::{Arrival, CardAction, NoticeKind, Requests};
use crate::{State, on_key, poll, run_command, run_on, theme, view};

/// The mock's box whose agent asked for github.
const ASKING: &str = "sbx_p8c3n0vd6rgs2hkq";

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

fn press(rt: &mut Runtime<State>, element: &str) {
    let (x, y) = rt.find(element).unwrap_or_else(|| panic!("{element} is not drawn"));
    rt.pointer_down(x + 4.0, y + 4.0);
    rt.pointer_up(x + 4.0, y + 4.0);
    settle(rt);
}

/// The asking box's workspace, on screen.
fn asking() -> Runtime<State> {
    let mut rt = runtime();
    run_on(&mut rt.state, "attach", vec![ASKING.into()], &mut Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.live.as_deref(), Some(ASKING));
    rt
}

// The box a request waits on says so under its header, in the core's words, and its close
// takes the notice down until what it is about changes; another box says nothing.
#[test]
fn a_boxs_notice_is_the_cores_and_dismisses() {
    let mut rt = asking();
    let n = rt.state.notice().cloned().expect("the asking box's notice");
    assert_eq!((n.kind, n.subject.as_str(), n.command.as_str(), n.urgent), (NoticeKind::Request, "credential request", "requests", true));
    assert!(rt.find("notice").is_some(), "the notice is not drawn");
    press(&mut rt, "notice-dismiss");
    assert!(rt.state.notice().is_none() && rt.find("notice").is_none(), "the notice stayed up: {:?}", rt.state.message);
    run_command(&mut rt.state, "dismiss", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("no banner to dismiss"));

    let mut rt = runtime();
    run_command(&mut rt.state, "attach", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.notice().is_none() && rt.find("notice").is_none(), "a box nothing waits on has a notice");
}

// Pressing the notice answers the request through the core's card: the sheet says the card,
// which card of how many, and is accepted or denied — never by a stray Enter on it.
#[test]
fn the_notice_opens_the_requests_card() {
    let mut rt = asking();
    press(&mut rt, "notice-call");
    let f = rt.state.sheets.form().expect("the request's card");
    assert_eq!((f.title.as_str(), f.aside.as_str()), ("Allow credential use?", "1 of 1"));
    assert!(f.card.iter().any(|s| s.lines.iter().any(|l| l.text == "This box may use the credential.")), "{:?}", f.card);
    assert_eq!(f.actions.iter().map(|a| (a.key, a.declines)).collect::<Vec<_>>(), [(CardAction::Accept, false), (CardAction::Deny, true)]);
    assert!(rt.find("sheet-card").is_some() && rt.find("sheet-action-accept").is_some(), "the card or its actions are not drawn");
    // The card opens with the keys on no action, and Enter there answers nothing: only an action
    // the keys were moved to is taken.
    key(&mut rt, Key::Enter);
    key(&mut rt, Key::Enter);
    assert!(rt.state.sheets.form().is_some_and(|f| !f.sent), "a stray Enter answered the card");
    key(&mut rt, Key::Tab);
    assert_eq!(rt.state.sheets.form().and_then(|f| f.action_at()).map(|a| a.key), Some(CardAction::Accept));
    key(&mut rt, Key::Enter);
    settle(&mut rt);
    assert!(rt.state.sheets.form().is_none(), "the card stayed up");
    assert_eq!(rt.state.message.as_deref(), Some("approved github for 1 day"));
    assert!(rt.state.notice().is_none(), "the answered request's notice stayed");
}

// A card arriving ends what its command said it was doing: "reading the project's secrets…" is
// over once the card waits on the person.
#[test]
fn a_card_takes_the_place_of_its_commands_step() {
    let mut rt = asking();
    run_command(&mut rt.state, "requests", &mut Tx::default());
    rt.state.message = Some("reading the project's secrets…".into());
    settle(&mut rt);
    assert!(rt.state.sheets.form().is_some());
    assert_eq!(rt.state.message, None, "the step is still said under the card");
}

// Leaving the card leaves the request waiting.
#[test]
fn leaving_the_card_leaves_the_request_waiting() {
    let mut rt = asking();
    run_command(&mut rt.state, "requests", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.sheets.form().is_some());
    key(&mut rt, Key::Escape);
    assert!(rt.state.sheets.form().is_none() && rt.state.message.is_none());
    assert!(rt.state.notice().is_some(), "a request left waiting lost its notice");
}

// The secrets page lists the requests waiting on its server; Tab moves the keys to them, and
// Enter answers one through its card, the page read again after.
#[test]
fn the_secrets_page_answers_its_requests() {
    let mut rt = runtime();
    run_command(&mut rt.state, "credentials", &mut Tx::default());
    settle(&mut rt);
    let p = rt.state.secrets.as_ref().unwrap();
    assert_eq!((p.requests.len(), p.requests[0].subject.as_str(), p.words.waiting.as_str()), (1, "github", "1 waiting"));
    assert!(rt.find("request-req_gh").is_some(), "the request's row is not drawn");
    key(&mut rt, Key::Tab);
    assert_eq!(rt.state.secrets.as_ref().unwrap().current_request().map(|r| r.id.as_str()), Some("req_gh"));
    key(&mut rt, Key::Enter);
    assert!(rt.state.sheets.form().is_some(), "Enter did not open the request's card");
    press(&mut rt, "sheet-action-deny");
    let p = rt.state.secrets.as_ref().unwrap();
    assert!(p.requests.is_empty() && !p.on_requests, "the page still offers the answered request");
    assert_eq!(rt.state.message.as_deref(), Some("denied github"));
}

// What a notification says of a request is the box's own words, so its body is escaped for the
// notification service's markup: a box can neither style it nor hand it a link.
#[test]
fn a_notifications_body_is_markup_escaped() {
    assert_eq!(crate::notify::markup_escaped(r#"gh</b><a href="https://x">sign in</a> & 'go'"#),
        "gh&lt;/b&gt;&lt;a href=&quot;https://x&quot;&gt;sign in&lt;/a&gt; &amp; &#39;go&#39;");
}

// A request that arrived is raised as a notification; pressing it answers that request, on its
// server, whatever is on screen.
#[test]
fn an_arrival_is_raised_and_pressing_it_answers() {
    let mut rt = runtime();
    RAISED.with_borrow_mut(Vec::clear);
    let arrival = Arrival { request: "req_gh".into(), r#box: ASKING.into(), title: "Credential request".into(), body: "reaper race · github".into(), ..Default::default() };
    let sub = rt.state.requests_sub;
    rt.state.inbox.push(Event::Requests(sub, Ok(Requests { arrived: vec![arrival.clone()], ..Default::default() })));
    settle(&mut rt);
    assert_eq!(RAISED.with_borrow(|r| r.clone()), [arrival.clone()]);
    rt.state.inbox.push(Event::Answer(arrival));
    settle(&mut rt);
    assert_eq!(rt.state.sheets.form().map(|f| f.title.as_str()), Some("Allow credential use?"));
}
