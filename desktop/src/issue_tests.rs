//! A box's issue and pull request as tabs: opened from the command and the header, drawn from the
//! core's issue, a comment posted, a close and a merge asked as the core asks them, the files
//! changed as a session given the window, and what each follows let go when parked or closed.

use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::wire::{IssueAct, IssueState, Status, TabKind};
use crate::{Focus, Side, State, issue, on_key, poll, run_command, theme, view};

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
    let at = rt.find(element).unwrap_or_else(|| panic!("no {element} on screen"));
    rt.pointer_down(at.0, at.1);
    rt.pointer_up(at.0, at.1);
    settle(rt);
}

/// The box tagged with an issue and a pull request, its workspace open, and `id`'s tab on it.
fn opened(id: &str) -> (Runtime<State>, u32) {
    let mut rt = runtime();
    let at = rt.state.rows().iter().position(|r| r.r#box == "sbx_p8c3n0vd6rgs2hkq").expect("the tagged box");
    rt.state.move_cursor(at);
    rt.state.open_row(at);
    settle(&mut rt);
    run_command(&mut rt.state, id, &mut Tx::default());
    settle(&mut rt);
    let stream = rt.state.focused_pane().filter(|p| p.issue.is_some()).map(|p| p.stream).expect("the issue's tab, focused");
    (rt, stream)
}

fn shown(rt: &Runtime<State>, stream: u32) -> &crate::wire::Issue {
    issue::tab(&rt.state, stream).and_then(|t| t.issue.as_ref()).expect("the issue, read")
}

// The issue command opens a tab among the shells, numbered, following the issue: the core's
// badge, labels, sub-issues and timeline, drawn natively in place of a terminal.
#[test]
fn the_issue_is_a_tab_drawn_from_the_cores_issue() {
    let (rt, stream) = opened("issue");
    let ws = rt.state.live_ws().unwrap();
    let i = ws.panes.iter().position(|p| p.stream == stream).unwrap();
    assert_eq!(ws.panes[i].side(), Side::Shells);
    assert_eq!(ws.panes[i].exec.title, "issue #817");
    assert!(ws.numbered().contains(&i), "the issue's tab wears no number");
    let t = issue::tab(&rt.state, stream).unwrap();
    assert!(t.sub != 0 && !t.pull);
    let read = shown(&rt, stream);
    assert_eq!((read.badge.as_str(), read.labels.len(), read.sub_total), ("Open", 2, 2));
    assert!(rt.find(&format!("issue-pane-{stream}")).is_some(), "the issue is not drawn");
    assert!(rt.find(&format!("i{stream}-body")).is_some(), "its description is not rendered");
    assert!(rt.find(&format!("comment-{stream}")).is_some() && rt.find(&format!("state-{stream}")).is_some());
    // An issue has no merge and no files changed.
    assert!(rt.find(&format!("merge-bar-{stream}")).is_none() && rt.find(&format!("files-{stream}")).is_none());
    // The command again shows it, never a second.
    let mut rt = rt;
    run_command(&mut rt.state, "issue", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.live_ws().unwrap().panes.iter().filter(|p| p.issue.is_some()).count(), 1);
}

// A box with no tag for it is refused, in the core's words, and nothing opens.
#[test]
fn a_box_without_the_tag_is_refused() {
    let mut rt = runtime();
    rt.state.move_cursor(0);
    run_command(&mut rt.state, "pr", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("no matching repository tag"));
    assert!(rt.state.workspaces.values().all(|w| w.panes.iter().all(|p| p.issue.is_none())));
}

// A pull request ready to merge says so on its chip and in its band; merging asks which way, of
// the ways the repository allows — one it does not is shown and never picked — and merges the
// head the tab showed.
#[test]
fn a_ready_pull_request_is_merged_as_the_core_asks() {
    let (mut rt, stream) = opened("pr");
    assert_eq!(rt.state.focused_pane().unwrap().exec.title, "PR #912");
    assert!(shown(&rt, stream).pull_request.as_ref().unwrap().merge.as_ref().is_some_and(|m| m.ready), "the core's issue is not ready");
    assert!(rt.find(&format!("ready-{stream}")).is_some(), "the ready band is not drawn");
    key(&mut rt, Key::Text("m".into()));
    let q = rt.state.sheets.question_mut().expect("the merge question");
    assert_eq!(q.question.title, "Merge #912 into main");
    assert!(q.question.body.contains("at 8be21c0f00d1"), "the head shown is not what is merged: {}", q.question.body);
    assert_eq!(q.picked, 0);
    // Down steps over the way the repository refuses.
    key(&mut rt, Key::Down);
    assert_eq!(rt.state.sheets.question_mut().unwrap().picked, 2);
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.message.as_deref(), Some("merged #912 (rebase)"));
    assert_eq!(shown(&rt, stream).badge, "Merged");
    assert!(rt.find(&format!("ready-{stream}")).is_none() && rt.find(&format!("merge-bar-{stream}")).is_none());
    // A merged one has nothing to close or reopen.
    assert!(rt.find(&format!("state-{stream}")).is_none());
}

// A comment is written in the tab's field and posted as written; once it is GitHub's the field
// empties and the timeline has it. An empty one is not posted.
#[test]
fn a_comment_is_posted_from_the_tab() {
    let (mut rt, stream) = opened("issue");
    key(&mut rt, Key::Text("c".into()));
    assert!(issue::tab(&rt.state, stream).unwrap().composing, "c does not open the comment field");
    assert!(rt.find(&format!("comment-field-{stream}")).is_some());
    // An empty one is the core's to refuse, in its words; the field stays open.
    issue::post(&mut rt.state, stream);
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("nothing to post — the comment is empty"));
    assert!(issue::tab(&rt.state, stream).unwrap().composing && !issue::tab(&rt.state, stream).unwrap().posting);
    let before = shown(&rt, stream).timeline.len();
    rt.update(|s, _| s.pane_mut(stream).unwrap().issue.as_mut().unwrap().composer = rune::TextEdit::new("  indented, as written"));
    press(&mut rt, &format!("comment-post-{stream}"));
    assert_eq!(rt.state.message.as_deref(), Some("commented on #817"));
    let t = issue::tab(&rt.state, stream).unwrap();
    assert!(t.composer.text.is_empty() && !t.composing && !t.posting);
    let after = &shown(&rt, stream).timeline;
    assert_eq!(after.len(), before + 1);
    assert_eq!(after.last().unwrap().body, "  indented, as written");
}

// Closing asks how, as the core does for an issue, and the badge says what was done.
#[test]
fn closing_an_issue_asks_how() {
    let (mut rt, stream) = opened("issue");
    press(&mut rt, &format!("state-{stream}"));
    let q = rt.state.sheets.question_mut().expect("the close question");
    assert_eq!(q.question.title, "Close #817");
    assert_eq!(q.question.choices.len(), 2);
    key(&mut rt, Key::Down);
    key(&mut rt, Key::Enter);
    assert_eq!(rt.state.message.as_deref(), Some("closed #817 as not planned"));
    assert_eq!(shown(&rt, stream).badge, "Closed as not planned");
    assert!(!issue::tab(&rt.state, stream).unwrap().changing);
    // Dismissed, nothing is done and nothing said.
    key(&mut rt, Key::Text("x".into()));
    key(&mut rt, Key::Escape);
    assert_eq!(shown(&rt, stream).state, IssueState::Closed);
    assert!(!issue::tab(&rt.state, stream).unwrap().changing);
}

// The files changed are a session of their own on a tab given the window, as a tool's is; the
// command again shows it, and its close ends it.
#[test]
fn the_files_changed_are_a_session_given_the_window() {
    let (mut rt, _) = opened("pr");
    key(&mut rt, Key::Text("f".into()));
    let ws = rt.state.live_ws().unwrap();
    let files: Vec<usize> = ws.panes.iter().enumerate().filter(|(_, p)| p.exec.kind == TabKind::Files).map(|(i, _)| i).collect();
    assert_eq!(files.len(), 1, "no files changed tab");
    let p = &ws.panes[files[0]];
    assert_eq!((p.exec.files, p.exec.title.as_str(), p.side()), (912, "files changed #912", Side::Shells));
    assert!(ws.maximized && ws.focused() == Some(files[0]), "not given the window");
    let stream = p.stream;
    run_command(&mut rt.state, "files", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.live_ws().unwrap().panes.iter().filter(|p| p.exec.kind == TabKind::Files).count(), 1, "a second files changed");
    run_command(&mut rt.state, "end", &mut Tx::default());
    settle(&mut rt);
    assert!(rt.state.live_ws().unwrap().panes.iter().all(|p| p.stream != stream), "its close did not end it");
}

// The files changed, given the window, give it back when they go: the pull request's tab shown
// beside the box's terminals again, not maximized.
#[test]
fn the_files_changed_going_give_the_window_back() {
    let (mut rt, pull) = opened("pr");
    let ws = rt.state.live_ws().unwrap();
    assert!(!ws.maximized && ws.focus == Side::Shells);
    let terminals = ws.active[Side::Terminals as usize].map(|i| ws.panes[i].stream);
    key(&mut rt, Key::Text("f".into()));
    assert!(rt.state.live_ws().unwrap().maximized, "not given the window");
    run_command(&mut rt.state, "end", &mut Tx::default());
    settle(&mut rt);
    let ws = rt.state.live_ws().unwrap();
    assert!(ws.panes.iter().all(|p| p.exec.kind != TabKind::Files), "the files changed are still there");
    assert!(!ws.maximized, "the workspace was left maximized");
    assert_eq!(ws.focused().map(|i| ws.panes[i].stream), Some(pull), "the pull request's tab is not the one in front");
    assert_eq!(ws.active[Side::Terminals as usize].map(|i| ws.panes[i].stream), terminals, "the terminals column moved");
}

// Parked, a tab reads nothing; back on screen it follows its issue again; closed, it lets go.
#[test]
fn a_tab_follows_its_issue_only_on_screen() {
    let (mut rt, stream) = opened("issue");
    let key_ = rt.state.live.clone().unwrap();
    rt.state.open_row(0);
    settle(&mut rt);
    assert_eq!(rt.state.workspaces[&key_].panes.iter().find(|p| p.stream == stream).unwrap().issue.as_ref().unwrap().sub, 0, "parked, it still follows");
    rt.state.open_box(&key_);
    settle(&mut rt);
    assert!(issue::tab(&rt.state, stream).unwrap().sub != 0, "back on screen, it follows nothing");
    rt.state.focus = Focus::Panes;
    run_command(&mut rt.state, "end", &mut Tx::default());
    settle(&mut rt);
    assert!(issue::tab(&rt.state, stream).is_none(), "its close did not close it");
}

// The header's issue opens its tab on a press; the page is the modifier press's.
#[test]
fn the_headers_work_opens_its_tab() {
    let mut rt = runtime();
    let at = rt.state.rows().iter().position(|r| r.r#box == "sbx_p8c3n0vd6rgs2hkq").unwrap();
    rt.state.open_row(at);
    settle(&mut rt);
    press(&mut rt, "work-1");
    assert_eq!(rt.state.focused_pane().map(|p| p.exec.title.clone()).as_deref(), Some("PR #912"));
    assert!(rt.take_url_requests().is_empty(), "a plain press opened the browser");
}

// Each act settles itself alone: a close landing while a comment is still on its way leaves the
// comment, and its field, as they were.
#[test]
fn an_act_settles_only_itself() {
    let (mut rt, stream) = opened("issue");
    rt.update(|s, _| {
        let t = s.pane_mut(stream).unwrap().issue.as_mut().unwrap();
        t.composer = rune::TextEdit::new("not yet posted");
        t.composing = true;
        t.posting = true;
        t.changing = true;
    });
    let done = crate::wire::Result { status: Status::Done, note: "closed #817".into(), ..Default::default() };
    issue::acted(&mut rt.state, stream, IssueAct::State, &done);
    let t = issue::tab(&rt.state, stream).unwrap();
    assert!(!t.changing && t.posting && t.composing && t.composer.text == "not yet posted", "a close settled the comment too");
    let refused = crate::wire::Result { status: Status::Failed, why: "the comment was not posted: no".into(), ..Default::default() };
    issue::acted(&mut rt.state, stream, IssueAct::Comment, &refused);
    let t = issue::tab(&rt.state, stream).unwrap();
    assert!(!t.posting && t.composer.text == "not yet posted", "a refused comment lost its words");
}

// A tab opened on the box's issue whose number is a pull request draws it, but merges and
// reviews nothing: those reach the box's pull request, through its own tab.
#[test]
fn an_issue_tab_on_a_pull_request_does_not_merge() {
    let (mut rt, stream) = opened("issue");
    rt.update(|s, _| {
        let t = s.pane_mut(stream).unwrap().issue.as_mut().unwrap();
        let mut read = crate::backend::mock_issue(912, true);
        read.label = "issue #912".into();
        t.issue = Some(read);
    });
    settle(&mut rt);
    assert!(rt.find(&format!("ready-{stream}")).is_some(), "the read pull request is not drawn");
    assert!(rt.find(&format!("merge-{stream}")).is_none() && rt.find(&format!("merge-bar-{stream}")).is_none() && rt.find(&format!("files-{stream}")).is_none());
    key(&mut rt, Key::Text("m".into()));
    assert!(rt.state.sheets.question_mut().is_none(), "m merged from an issue's tab");
}

// A files changed still starting is waited for, never started a second time.
#[test]
fn a_files_changed_starting_is_not_started_again() {
    let (mut rt, _) = opened("pr");
    let sandbox = rt.state.live.clone().unwrap();
    rt.state.files_starting.insert(999, sandbox);
    run_command(&mut rt.state, "files", &mut Tx::default());
    settle(&mut rt);
    assert_eq!(rt.state.message.as_deref(), Some("discobox-review is still starting in the box…"));
    assert!(rt.state.live_ws().unwrap().panes.iter().all(|p| p.exec.kind != TabKind::Files));
}
