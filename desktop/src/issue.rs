//! A box's issue and pull request, as tabs among its shells: the GitHub issue or pull request its
//! `issue=` or `pr=` tag numbers, read in the window rather than in a browser, with a field to
//! answer it, its close or reopen, a pull request's merge, and its files changed.
//!
//! What it says is the core's (the issue topic, `console.Issue`): the badge, whether it can merge
//! and why not, each event's words, the checks summed up, what each reviewer said. Its bodies are
//! markdown the bridge made safe to render (`console.MarkdownSource`), drawn natively (Rune's
//! Markdown) in the window's own tokens. Every act is the core's: a comment is posted as written,
//! a close or reopen and a merge are the core's questions (sheets), and a merge merges the head
//! this tab showed, so a branch pushed to since is refused, not merged unseen. A pull request's
//! files changed are discobox-review in the box over the base and head this tab showed: a session
//! of its own, on a tab given the window as a tool's is, ended when its tab is closed.
//!
//! The tab follows its issue while its workspace is on screen and lets go of it when parked, as
//! the workspace's own polls do. Its keys are its own, as the TUI's tab's are: `c` writes a
//! comment, `x` closes or reopens, `m` merges, `f` shows the files changed, `o` opens the page,
//! `r` reads it again; the page keys scroll. In the comment field Return is a new line, ⌘↩
//! (Ctrl+Enter elsewhere) posts and Esc steps out keeping what was written.

use rune::{Color, El, Key, KeyEvent, ScrollSpec, TextEdit, Tx, col, el, row, spacer, text, text_input};
use rune_kit::markdown::Markdown;

use crate::wire::{
    Args, Issue, IssueAct, IssueItem, IssueItemKind, IssueMark, IssueQuery, IssueState, PullRequest, Result as CommandResult, Status, Subscribe, Tab, TabKind,
    Topic,
};
use crate::{DEFAULT_SIZE, Focus, Pane, Pending, State, run_on, theme, view};

/// A tab on a box's issue or pull request.
pub struct IssueTab {
    /// Which of the box's work it is: its pull request, or its issue.
    pub pull: bool,
    /// The issue topic's subscription while its workspace is on screen; 0 while it is parked.
    pub sub: u32,
    /// The last read, none until the first lands; why the last read failed, the one before it
    /// staying on screen under it.
    pub issue: Option<Issue>,
    pub error: Option<String>,
    /// The comment being written: kept while the tab is open, written or not, so stepping out
    /// to look at a terminal does not lose it.
    pub composer: TextEdit,
    /// Whether the comment field is shown.
    pub composing: bool,
    /// A comment, a close or reopen, or a merge on its way to GitHub.
    pub posting: bool,
    pub changing: bool,
    /// The head of the pull request the person was shown: what a subscription made again —
    /// the workspace back on screen — still says a push since against (`IssueQuery.shown`).
    pub shown: String,
}

/// How a timeline's event rows lay out: the time, then who.
const WHEN_W: f32 = 90.0;
const WHO_W: f32 = 140.0;
/// The facts' label column.
const FACT_W: f32 = 100.0;

/// The tab's pane: a pane like any other in the strip — numbered, moved between, its chip
/// dragged — whose stream is never opened.
pub fn pane(stream: u32, pull: bool, label: &str) -> Pane {
    let exec = Tab { exec: format!("{}:{label}", if pull { "pr" } else { "issue" }), kind: TabKind::Shell, title: label.into(), live: true, ..Default::default() };
    let mut composer = TextEdit::new("");
    composer.focused = false;
    let mut p = Pane::new(stream, exec);
    p.issue = Some(IssueTab { pull, sub: 0, issue: None, error: None, composer, composing: false, posting: false, changing: false, shown: String::new() });
    p
}

/// The issue and pr commands' flow: the box's workspace, and the tab on its work — focused if
/// it is open, else opened and read in full. A box with no workspace that is not running is
/// only pointed at, as opening one is.
pub fn open(s: &mut State, r: &CommandResult, pull: bool) {
    let Some(work) = r.work.clone() else { return };
    let key = r.r#box.clone();
    let Some(b) = s.box_by_id(&key).cloned() else { return };
    if !b.running() && !s.workspaces.contains_key(&key) {
        s.message = Some(format!("{} is {} — start it from its menu", b.name, b.state.as_str()));
        return;
    }
    s.show_workspace(&key);
    let Some(ws) = s.workspaces.get_mut(&key) else { return };
    if let Some(i) = ws.panes.iter().position(|p| p.issue.as_ref().is_some_and(|t| t.pull == pull)) {
        ws.show(i);
        s.focus = Focus::Panes;
        return;
    }
    let stream = s.next_stream;
    s.next_stream += 1;
    let ws = s.workspaces.get_mut(&key).unwrap();
    ws.panes.push(pane(stream, pull, &work.label));
    let at = ws.panes.len() - 1;
    ws.show(at);
    s.focus = Focus::Panes;
    follow(s, &key, stream, true);
}

/// The tab on a stream, if that pane draws an issue.
pub fn tab(s: &State, stream: u32) -> Option<&IssueTab> { s.pane(stream)?.issue.as_ref() }

fn tab_mut(s: &mut State, stream: u32) -> Option<&mut IssueTab> { s.pane_mut(stream)?.issue.as_mut() }

/// The box whose workspace holds a pane.
fn box_of(s: &State, stream: u32) -> Option<String> {
    s.workspaces.iter().find(|(_, w)| w.panes.iter().any(|p| p.stream == stream)).map(|(k, _)| k.clone())
}

/// Follows a tab's issue (again), in full when someone asked to see it now — which shows them
/// the head it brings — else against the head they were shown: a new subscription in place of
/// the one there was, whose snapshots that one's no longer reach.
fn follow(s: &mut State, sandbox: &str, stream: u32, full: bool) {
    let Some((pull, old, shown)) = tab(s, stream).map(|t| (t.pull, t.sub, t.shown.clone())) else { return };
    s.unsubscribe(old);
    let shown = if full { String::new() } else { shown };
    let sub = s.subscribe(Subscribe { topic: Topic::Issue, r#box: sandbox.into(), issue: Some(IssueQuery { pull, full, shown }), ..Default::default() });
    if let Some(t) = tab_mut(s, stream) {
        t.sub = sub;
    }
}

/// Follows every issue tab of a workspace coming back on screen.
pub fn follow_all(s: &mut State, sandbox: &str) {
    let streams: Vec<u32> = s.workspaces.get(sandbox).map(|w| w.panes.iter().filter(|p| p.issue.as_ref().is_some_and(|t| t.sub == 0)).map(|p| p.stream).collect()).unwrap_or_default();
    for stream in streams {
        follow(s, sandbox, stream, false);
    }
}

/// Lets go of every issue tab's subscription of a workspace leaving the screen: parked, it
/// reads nothing, as its sessions and header do not.
pub fn unfollow_all(s: &mut State, sandbox: &str) {
    let subs: Vec<u32> = s.workspaces.get_mut(sandbox).map(|w| w.panes.iter_mut().filter_map(|p| p.issue.as_mut()).map(|t| std::mem::take(&mut t.sub)).collect()).unwrap_or_default();
    for sub in subs {
        s.unsubscribe(sub);
    }
}

/// An issue snapshot, for the tab following it: its label on its chip (a pull request ready to
/// merge is marked there, `view::chip_state`), and the head the person is shown.
pub fn take(s: &mut State, sub: u32, r: Result<Issue, String>) {
    if sub == 0 {
        return;
    }
    let Some(p) = s.workspaces.values_mut().flat_map(|w| w.panes.iter_mut()).find(|p| p.issue.as_ref().is_some_and(|t| t.sub == sub)) else { return };
    let t = p.issue.as_mut().unwrap();
    match r {
        Ok(issue) => {
            p.exec.title = issue.label.clone();
            t.shown = match &issue.pull_request {
                Some(_) if !issue.pushed.is_empty() => issue.pushed.clone(),
                Some(pr) => pr.head_sha.clone(),
                None => String::new(),
            };
            t.error = None;
            t.issue = Some(issue);
        }
        Err(e) => t.error = Some(e),
    }
}

/// Closes a tab: what it follows is let go, and what was written in its field with it.
pub fn close(s: &mut State, stream: u32) {
    let sub = tab(s, stream).map_or(0, |t| t.sub);
    s.unsubscribe(sub);
    s.drop_pane(stream);
}

/// Reads it again now, in full: what it brings is what the person has now seen, a head newer
/// than the one shown first included.
pub fn read_again(s: &mut State, stream: u32) {
    let Some(sandbox) = box_of(s, stream) else { return };
    let number = tab(s, stream).and_then(|t| t.issue.as_ref()).map(|i| i.number);
    follow(s, &sandbox, stream, true);
    s.message = Some(match number {
        Some(n) => format!("reading #{n} again"),
        None => "reading it again".into(),
    });
}

/// The command a tab's work is reached by.
fn command(t: &IssueTab) -> &'static str { if t.pull { "pr" } else { "issue" } }

/// Opens the comment field, with the keys.
pub fn compose(s: &mut State, stream: u32) {
    let Some(t) = tab_mut(s, stream) else { return };
    t.composing = true;
    t.composer.focused = true;
}

/// Steps out of the comment field, keeping what was written.
pub fn stop_composing(s: &mut State, stream: u32) {
    if let Some(t) = tab_mut(s, stream) {
        t.composing = false;
        t.composer.focused = false;
    }
}

/// Posts what the field holds, as the core posts it (an empty one it refuses, in its words);
/// it is cleared once the comment is GitHub's, and a refusal keeps every word of it.
pub fn post(s: &mut State, stream: u32) {
    let Some(sandbox) = box_of(s, stream) else { return };
    let Some(t) = tab_mut(s, stream).filter(|t| !t.posting) else { return };
    t.posting = true;
    let comment = t.composer.text.clone();
    let id = command(t);
    s.message = Some("posting the comment…".into());
    act(s, stream, sandbox, id, Args { act: Some(IssueAct::Comment), comment, ..Default::default() });
}

/// Closes or reopens it, as the core asks (a sheet).
pub fn change_state(s: &mut State, stream: u32) {
    let Some(sandbox) = box_of(s, stream) else { return };
    let Some(t) = tab_mut(s, stream).filter(|t| !t.changing) else { return };
    t.changing = true;
    let id = command(t);
    act(s, stream, sandbox, id, Args { act: Some(IssueAct::State), ..Default::default() });
}

/// Merges the box's pull request — this tab's, opened as one — at the head this tab showed, as
/// the core asks (a sheet of the ways the repository allows): a branch pushed to since is
/// GitHub's to refuse.
pub fn merge(s: &mut State, stream: u32) {
    let Some(sandbox) = box_of(s, stream) else { return };
    let Some(t) = tab_mut(s, stream).filter(|t| t.pull && !t.changing) else { return };
    let (head, pushed) = t.issue.as_ref().and_then(|i| Some((i.pull_request.as_ref()?.head_sha.clone(), i.pushed.clone()))).unwrap_or_default();
    t.changing = true;
    act(s, stream, sandbox, "pr", Args { act: Some(IssueAct::Merge), head, pushed, ..Default::default() });
}

fn act(s: &mut State, stream: u32, sandbox: String, id: &str, args: Args) {
    let context = s.context();
    let what = args.act.unwrap_or_default();
    s.command(id, vec![sandbox], context, args, Pending::Act(stream, what));
}

/// What an act came to, settling that act alone: a comment posted clears the field, and a
/// refusal keeps every word of it, saying why. A question dismissed did nothing, and says
/// nothing.
pub fn acted(s: &mut State, stream: u32, what: IssueAct, r: &CommandResult) {
    if let Some(t) = tab_mut(s, stream) {
        match what {
            IssueAct::Comment => {
                t.posting = false;
                if r.status == Status::Done {
                    t.composer = TextEdit::new("");
                    t.composing = false;
                }
            }
            IssueAct::State | IssueAct::Merge => t.changing = false,
        }
    }
    s.message = match r.status {
        Status::Done => (!r.note.is_empty()).then(|| r.note.clone()),
        Status::Failed => Some(r.why.clone()),
        _ => None,
    };
}

/// The files command for a box: discobox-review over the base and head its pull request's tab
/// showed, or the head read now for a box with none open; a files changed tab already open for
/// it is shown instead.
pub fn files(s: &mut State, targets: Vec<String>) {
    let Some(sandbox) = targets.first().cloned() else { return };
    // One still starting is waited for, never started twice.
    if s.files_starting.values().any(|b| *b == sandbox) {
        s.message = Some("discobox-review is still starting in the box…".into());
        return;
    }
    if let Some(ws) = s.workspaces.get_mut(&sandbox)
        && let Some(i) = ws.panes.iter().position(|p| p.exec.kind == TabKind::Files && !p.exited)
    {
        if s.live.as_deref() != Some(&sandbox) {
            s.show_workspace(&sandbox);
        }
        let ws = s.workspaces.get_mut(&sandbox).unwrap();
        ws.show(i);
        ws.maximized = true;
        s.focus = Focus::Panes;
        return;
    }
    let shown = s.workspaces.get(&sandbox).and_then(|w| w.panes.iter().filter_map(|p| p.issue.as_ref()).find(|t| t.pull))
        .and_then(|t| t.issue.as_ref()?.pull_request.as_ref().map(|p| (p.head_sha.clone(), p.base_sha.clone())));
    let (head, base) = shown.unwrap_or_default();
    let stream = s.next_stream;
    s.next_stream += 1;
    let (cols, rows) = DEFAULT_SIZE;
    s.message = Some("starting discobox-review in the box…".into());
    let context = s.context();
    let args = Args { stream, cols: cols.into(), rows: rows.into(), head, base, ..Default::default() };
    let live = s.live.clone();
    s.files_starting.insert(stream, sandbox.clone());
    s.command("files", vec![sandbox.clone()], context, args, Pending::Session(stream, sandbox, live));
}

/// Opens a link from an issue: an absolute web address only. A stranger writes these, and
/// anything else — a file, a custom scheme — is not followed.
fn open_link(s: &mut State, url: &str, tx: &mut Tx) {
    if !(url.starts_with("https://") || url.starts_with("http://")) {
        s.message = Some(format!("not opened: {url} is not a web address"));
        return;
    }
    if let Err(e) = tx.open_url(url) {
        s.message = Some(format!("could not open {url}: {e}"));
    }
}

/// The page on GitHub the tab shows.
fn open_page(s: &mut State, stream: u32, tx: &mut Tx) {
    let Some(url) = tab(s, stream).and_then(|t| t.issue.as_ref()).map(|i| i.url.clone()) else { return };
    open_link(s, &url, tx);
}

/// A key on the focused issue tab, while its comment field does not have it: its own keys, as
/// the TUI's are; false for one it leaves to the window.
pub fn key(s: &mut State, stream: u32, ev: &KeyEvent, tx: &mut Tx) -> bool {
    if ev.mods.cmd || ev.mods.ctrl || ev.mods.alt {
        return false;
    }
    let scroll = format!("issue-{stream}");
    match &ev.key {
        Key::PageUp => tx.scroll_page(scroll, -1),
        Key::PageDown => tx.scroll_page(scroll, 1),
        Key::Text(t) if t == " " => tx.scroll_page(scroll, 1),
        Key::Home => tx.scroll_to_y(scroll, 0.0),
        Key::End => tx.scroll_to_bottom(scroll),
        Key::Text(t) => match t.as_str() {
            "c" => compose(s, stream),
            "x" => change_state(s, stream),
            "m" => merge(s, stream),
            "f" if tab(s, stream).is_some_and(|t| t.pull) => run_on(s, "files", box_of(s, stream).into_iter().collect(), tx),
            "o" => open_page(s, stream, tx),
            "r" => read_again(s, stream),
            _ => return false,
        },
        _ => return false,
    }
    true
}

// ---------------------------------------------------------------------------------------------
// Drawing

fn t(s: impl Into<String>, c: Color) -> El { text(s).font(theme::UI).color(c) }

fn small(s: impl Into<String>, c: Color) -> El { text(s).font(theme::UI_SMALL).color(c) }

/// A mark the core gave: filled or hollow, in its tone.
fn mark(m: &IssueMark) -> El {
    let c = theme::tone(m.tone);
    let dot = el().size(8.0, 8.0).radius(4.0);
    if m.filled { dot.fill(c) } else { dot.border(theme::stroke_focus(), c) }
}

/// A body's markdown, rendered natively; its links open only on the web.
fn markdown(key: String, src: &str) -> El {
    Markdown::new(src.to_string())
        .selectable(true)
        .on_link_tx(|s: &mut State, url: &str, tx: &mut Tx| open_link(s, url, tx))
        .render(&theme::markdown())
        .key(key)
        .w_full()
}

/// A button on the tab's bar: what it does, with the key that does it beside it.
fn button(key: impl Into<String>, label: &str, press: &str, enabled: bool) -> El {
    let ink = if enabled { theme::text() } else { theme::dim() };
    let b = row().key(key).h(24.0).px(10.0).gap(6.0).center_y().radius(theme::r_key()).border(theme::stroke_quiet(), theme::line_bright())
        .child(t(label.to_string(), ink))
        .child(view::keycap(press.to_uppercase()));
    if enabled { b.hover(theme::selection()).cursor(rune::Cursor::Pointer) } else { b }
}

/// A group's head in the timeline — a comment, a review — as the list heads a group: who, what,
/// and when.
fn bar(who: &str, what: El, ago: &str) -> El {
    row().w_full().h(24.0).px(10.0).gap(8.0).center_y().radius(theme::r_row()).fill(theme::line())
        .child(text(who.to_string()).font(theme::UI_BOLD).color(theme::text()))
        .child(what)
        .child(spacer())
        .child(small(ago.to_string(), theme::grey()))
}

fn fact(label: &str, value: El) -> El {
    row().w_full().gap(8.0).items_start()
        .child(small(label.to_string(), theme::dim()).w(FACT_W).mt(2.0))
        .child(value.grow(1.0).min_w(0.0))
}

/// The pull request's branches and what is in the way of merging it.
fn merge_band(stream: u32, p: &PullRequest, can_merge: bool, changing: bool) -> Option<El> {
    let m = p.merge.as_ref()?;
    if m.ready {
        let band = row().key(format!("ready-{stream}")).w_full().h(34.0).px(12.0).gap(10.0).center_y().radius(theme::r_row())
            .fill(theme::selection()).border(theme::stroke_focus(), theme::ok())
            .child(mark(&m.mark))
            .child(text(m.said.clone()).font(theme::UI_BOLD).color(theme::ok()))
            .child(spacer());
        if !can_merge {
            return Some(band);
        }
        let go = button(format!("merge-{stream}"), "Merge", "m", !changing).fill(theme::raised())
            .on_press(move |s: &mut State| merge(s, stream));
        return Some(band.child(go));
    }
    let mut r = row().key(format!("merge-said-{stream}")).w_full().gap(8.0).center_y()
        .child(mark(&m.mark))
        .child(text(m.said.clone()).font(theme::UI_BOLD).color(theme::tone(m.mark.tone)).wrap().shrink());
    if m.unknown {
        r = r.child(button(format!("ask-{stream}"), "Ask again", "r", true).on_press(move |s: &mut State| read_again(s, stream)));
    }
    Some(r)
}

/// The facts the page's sidebar has: reviewers, assignees, labels, type, milestone, whether it
/// is locked, sub-issues, and a pull request's size and checks.
fn facts(issue: &Issue) -> El {
    let mut out = col().w_full().gap(6.0);
    if let Some(p) = &issue.pull_request {
        if !p.reviewers.is_empty() {
            let mut rs = col().gap(3.0);
            for r in &p.reviewers {
                rs = rs.child(row().gap(6.0).center_y().child(mark(&r.mark)).child(t(r.name.clone(), theme::text())).child(small(r.said.clone(), theme::grey())));
            }
            out = out.child(fact("Reviewers", rs));
        }
    }
    if !issue.assignees.is_empty() {
        out = out.child(fact("Assignees", t(issue.assignees.join(", "), theme::text()).wrap()));
    }
    if !issue.labels.is_empty() {
        let mut ls = row().gap(10.0).flex_wrap();
        for l in &issue.labels {
            let mut dot = el().size(8.0, 8.0).radius(4.0);
            dot = match theme::label(&l.color) {
                Some(c) => dot.fill(c),
                None => dot.border(theme::stroke_quiet(), theme::dim()),
            };
            ls = ls.child(row().gap(5.0).center_y().child(dot).child(t(l.name.clone(), theme::text())));
        }
        out = out.child(fact("Labels", ls));
    }
    if !issue.r#type.is_empty() {
        out = out.child(fact("Type", t(issue.r#type.clone(), theme::text())));
    }
    if !issue.milestone.is_empty() {
        out = out.child(fact("Milestone", t(issue.milestone.clone(), theme::text())));
    }
    if issue.locked {
        out = out.child(fact("Locked", t("the conversation is limited to collaborators", theme::grey())));
    }
    if issue.sub_total > 0 {
        let mut subs = col().gap(3.0);
        let mut said = format!("{} of {} done", issue.sub_done, issue.sub_total);
        if (issue.sub_issues.len() as i64) < issue.sub_total {
            said.push_str(&format!(" · {} shown", issue.sub_issues.len()));
        }
        subs = subs.child(t(said, theme::text()));
        for r in &issue.sub_issues {
            subs = subs.child(row().gap(6.0).center_y().child(mark(&r.mark)).child(small(r.number.clone(), theme::dim())).child(t(r.title.clone(), theme::text()).truncate().shrink()));
        }
        out = out.child(fact("Sub-issues", subs));
    }
    if let Some(p) = &issue.pull_request {
        let files = if p.changed_files == 1 { "1 file".to_string() } else { format!("{} files", p.changed_files) };
        let commits = if p.commits == 1 { "1 commit".to_string() } else { format!("{} commits", p.commits) };
        out = out.child(fact("Changes", row().gap(8.0).center_y()
            .child(t(format!("{files} · {commits}"), theme::text()))
            .child(text(format!("+{}", p.additions)).font(theme::UI).color(theme::done()))
            .child(text(format!("−{}", p.deletions)).font(theme::UI).color(theme::salmon()))));
        let c = &p.checks;
        if c.failing + c.pending + c.passed > 0 || c.partial {
            let mut cs = col().gap(3.0);
            let mut sum = row().gap(8.0);
            if c.failing > 0 {
                sum = sum.child(t(format!("{} failing", c.failing), theme::err()));
            }
            if c.pending > 0 {
                sum = sum.child(t(format!("{} pending", c.pending), theme::warn()));
            }
            if c.passed > 0 {
                sum = sum.child(t(format!("{} passed", c.passed), theme::ok()));
            }
            if c.partial {
                sum = sum.child(t(if c.failing + c.pending + c.passed == 0 { "could not be read" } else { "not all could be read" }, theme::warn()));
            }
            cs = cs.child(sum);
            for (i, check) in c.unpassed.iter().enumerate() {
                let mut name = t(check.name.clone(), theme::text());
                if !check.url.is_empty() {
                    let url = check.url.clone();
                    name = name.key(format!("check-{i}")).color(theme::info()).cursor(rune::Cursor::Pointer).help(url.clone())
                        .on_press_tx(move |s: &mut State, tx: &mut Tx| open_link(s, &url, tx));
                }
                cs = cs.child(row().gap(6.0).center_y().child(mark(&check.mark)).child(name));
            }
            out = out.child(fact("Checks", cs));
        }
    }
    out
}

/// One thing in the timeline: a comment or a review under its bar, a run of events and commits
/// as rows of when, who and what.
fn item(stream: u32, n: usize, it: &IssueItem) -> El {
    match it.kind {
        IssueItemKind::Comment => {
            let mut c = col().w_full().gap(6.0).mt(6.0).child(bar(&it.actor, small("commented", theme::grey()), &it.ago));
            c = c.child(body(format!("i{stream}-{n}"), &it.body));
            if !it.reactions.is_empty() {
                c = c.child(small(it.reactions.clone(), theme::dim()));
            }
            c
        }
        IssueItemKind::Review => {
            let tone = it.tone.map_or(theme::grey(), theme::tone);
            let mut c = col().w_full().gap(6.0).mt(6.0).child(bar(&it.actor, small(it.text.clone(), tone), &it.ago));
            if !it.body.trim().is_empty() {
                c = c.child(markdown(format!("i{stream}-{n}"), &it.body));
            }
            for (k, th) in it.threads.iter().enumerate() {
                let mut thread = col().w_full().gap(4.0).pl(16.0).mt(4.0).child(small(th.r#where.clone(), theme::info()));
                let mut hunk = col().w_full().px(8.0).py(4.0).radius(theme::r_key()).fill(theme::sunken());
                for line in &th.hunk {
                    let ink = if line.starts_with("@@") { theme::info() } else if line.starts_with('+') { theme::done() } else if line.starts_with('-') { theme::salmon() } else { theme::grey() };
                    hunk = hunk.child(text(line.replace('\t', "    ")).font(theme::TERM).color(ink).truncate());
                }
                if !th.hunk.is_empty() {
                    thread = thread.child(hunk);
                }
                for (m, cm) in th.comments.iter().enumerate() {
                    thread = thread.child(row().gap(8.0).center_y().child(text(cm.author.clone()).font(theme::UI_BOLD).color(theme::text())).child(small(cm.ago.clone(), theme::dim())))
                        .child(markdown(format!("i{stream}-{n}-{k}-{m}"), &cm.body));
                    if !cm.reactions.is_empty() {
                        thread = thread.child(small(cm.reactions.clone(), theme::dim()));
                    }
                }
                c = c.child(thread);
            }
            c
        }
        IssueItemKind::Commit => event_row(&it.ago, &it.actor, row().gap(8.0).min_w(0.0).child(small(it.commit.clone(), theme::dim())).child(t(it.text.clone(), theme::text()).truncate().shrink())),
        IssueItemKind::Event => event_row(&it.ago, &it.actor, t(it.text.clone(), theme::grey()).wrap()),
    }
}

fn event_row(ago: &str, who: &str, what: El) -> El {
    row().w_full().gap(8.0).items_start()
        .child(small(ago.to_string(), theme::dim()).w(WHEN_W).mt(2.0))
        .child(t(who.to_string(), theme::text()).w(WHO_W).truncate())
        .child(what.grow(1.0).min_w(0.0))
}

/// A body, or what the page says of none.
fn body(key: String, src: &str) -> El {
    if src.trim().is_empty() {
        return t("No description provided.", theme::dim());
    }
    markdown(key, src)
}

/// The tab drawn in its pane: the issue, scrolled, with its bar of what can be done under it,
/// and the comment field while one is being written.
pub fn view(s: &State, p: &Pane) -> El {
    let stream = p.stream;
    let Some(tb) = p.issue.as_ref() else { return el() };
    let mut page = col().key(format!("issue-{stream}")).w_full().grow(1.0).min_h(0.0).px(14.0).py(10.0).gap(8.0);
    if let Some(e) = &tb.error {
        let said = if tb.issue.is_some() { format!("could not read it again: {e}") } else { format!("could not read it: {e}") };
        page = page.child(t(said, theme::err()).wrap());
    }
    let Some(issue) = &tb.issue else {
        if tb.error.is_none() {
            page = page.child(t(format!("reading {}…", p.exec.title), theme::dim()));
        }
        return col().w_full().h_full().child(page);
    };
    if !issue.notice.is_empty() {
        page = page.child(row().key(format!("pushed-{stream}")).w_full().gap(8.0).center_y()
            .child(t(issue.notice.clone(), theme::warn()).wrap().shrink())
            .child(button(format!("reread-{stream}"), "Read again", "r", true).on_press(move |s: &mut State| read_again(s, stream))));
    }
    page = page.child(row().w_full().gap(10.0).items_start()
        .child(text(issue.title.clone()).font(theme::UI_BOLD).color(theme::mark()).wrap().shrink())
        .child(t(format!("#{}", issue.number), theme::dim())));
    let comments = if issue.comments == 1 { "1 comment".to_string() } else { format!("{} comments", issue.comments) };
    let mut byline = row().w_full().gap(8.0).center_y().flex_wrap()
        .child(row().gap(6.0).center_y().px(8.0).py(2.0).radius(theme::r_key()).border(theme::stroke_quiet(), theme::tone(issue.mark.tone))
            .child(mark(&issue.mark)).child(t(issue.badge.clone(), theme::tone(issue.mark.tone))));
    match &issue.pull_request {
        Some(pr) => {
            let (who, verb, ago) = if pr.merged { (pr.merged_by.clone(), "merged", pr.merged_ago.clone()) } else { (issue.author.clone(), "wants to merge", issue.opened.clone()) };
            let commits = if pr.commits == 1 { "1 commit".to_string() } else { format!("{} commits", pr.commits) };
            byline = byline.child(text(who).font(theme::UI_BOLD).color(theme::text()))
                .child(t(format!("{verb} {commits} into"), theme::grey()))
                .child(t(pr.base.clone(), theme::info()))
                .child(t("from", theme::grey()))
                .child(t(pr.head.clone(), theme::info()))
                .child(t(format!("· {ago} · {comments}"), theme::grey()));
        }
        None => {
            byline = byline.child(text(issue.author.clone()).font(theme::UI_BOLD).color(theme::text()))
                .child(t(format!("opened this issue {} · {comments}", issue.opened), theme::grey()));
        }
    }
    page = page.child(byline);
    if let Some(band) = issue.pull_request.as_ref().and_then(|pr| merge_band(stream, pr, tb.pull, tb.changing)) {
        page = page.child(band);
    }
    page = page.child(facts(issue));
    page = page.child(col().w_full().gap(6.0).mt(6.0)
        .child(bar(&issue.author, small("opened", theme::grey()), &issue.opened))
        .child(body(format!("i{stream}-body"), &issue.body))
        .children((!issue.reactions.is_empty()).then(|| small(issue.reactions.clone(), theme::dim()))));
    let mut run = col().w_full().gap(4.0);
    let mut in_run = false;
    for (n, it) in issue.timeline.iter().enumerate() {
        let row_like = matches!(it.kind, IssueItemKind::Event | IssueItemKind::Commit);
        if row_like {
            run = run.child(item(stream, n, it));
            in_run = true;
            continue;
        }
        if in_run {
            page = page.child(run.mt(6.0));
            run = col().w_full().gap(4.0);
            in_run = false;
        }
        page = page.child(item(stream, n, it));
    }
    if in_run {
        page = page.child(run.mt(6.0));
    }
    if issue.truncated {
        page = page.child(t("the timeline goes on — open it on GitHub for the rest", theme::warn()).mt(8.0));
    }
    // The page scrolls inside what the field and the bar leave it, clipped there.
    let scroll = page.scroll(ScrollSpec { stick_bottom: false }).grow(1.0).min_h(0.0).clip_rounded(0.0);
    col().w_full().h_full().min_h(0.0).child(scroll).child(actions(s, p, tb, issue))
}

/// What can be done, as buttons with their keys; and the comment field while one is written.
fn actions(_s: &State, p: &Pane, tb: &IssueTab, issue: &Issue) -> El {
    let stream = p.stream;
    // A merge and the files changed reach the box's pull request: on the tab opened as one.
    let pr = issue.pull_request.as_ref().filter(|_| tb.pull);
    let merged = issue.pull_request.as_ref().is_some_and(|p| p.merged);
    let what = if tb.pull { "PR" } else { "issue" };
    let mut bar = row().key(format!("issue-bar-{stream}")).w_full().px(14.0).py(6.0).gap(8.0).center_y().flex_wrap();
    let written = !tb.composer.text.trim().is_empty();
    bar = bar.child(button(format!("comment-{stream}"), if written { "Go on writing" } else { "Comment" }, "c", true).on_press(move |s: &mut State| compose(s, stream)));
    if !merged {
        let label = if issue.state == IssueState::Closed { format!("Reopen {what}") } else { format!("Close {what}") };
        bar = bar.child(button(format!("state-{stream}"), &label, "x", !tb.changing).on_press(move |s: &mut State| change_state(s, stream)));
    }
    if pr.is_some() {
        if issue.state == IssueState::Open && !merged {
            bar = bar.child(button(format!("merge-bar-{stream}"), "Merge", "m", !tb.changing).on_press(move |s: &mut State| merge(s, stream)));
        }
        bar = bar.child(button(format!("files-{stream}"), "Files changed", "f", true).on_press_tx(move |s: &mut State, tx: &mut Tx| {
            let targets = box_of(s, stream).into_iter().collect();
            run_on(s, "files", targets, tx);
        }));
    }
    bar = bar
        .child(button(format!("page-{stream}"), "Open on GitHub", "o", true).on_press_tx(move |s: &mut State, tx: &mut Tx| open_page(s, stream, tx)))
        .child(button(format!("read-{stream}"), "Read again", "r", true).on_press(move |s: &mut State| read_again(s, stream)));
    let mut out = col().w_full().gap(6.0).pb(6.0);
    if tb.composing {
        let field = text_input(&tb.composer, theme::UI.lh(20.0)).key(format!("comment-field-{stream}")).grow(1.0).color(theme::text())
            .placeholder("Leave a comment — Markdown", theme::dim())
            .on_edit(move |s: &mut State| &mut s.pane_mut(stream).and_then(|p| p.issue.as_mut()).expect("the comment's tab").composer)
            .on_input_key(move |s: &mut State, ev: &KeyEvent, _tx: &mut Tx| {
                match ev.key {
                    // Posting is a chord nobody types by accident: what it does is public.
                    Key::Enter if ev.mods.cmd || ev.mods.ctrl => post(s, stream),
                    Key::Escape => stop_composing(s, stream),
                    _ => return false,
                }
                true
            });
        let posting = tb.posting;
        let post_label = if posting { "Posting…" } else { "Post" };
        out = out.child(col().w_full().px(14.0).pt(6.0).gap(6.0)
            .child(small(format!("Comment on #{} · Return is a new line · {} posts", issue.number, crate::keys::label(if crate::keys::MAC { "primary+enter" } else { "ctrl+enter" })), theme::dim()).wrap().w_full())
            .child(row().w_full().px(10.0).py(8.0).radius(theme::r_row()).fill(theme::sunken())
                .border(theme::stroke_quiet(), if tb.composer.focused { theme::mark() } else { theme::line() })
                .child(rune::auto_grow(field, 3, 12).grow(1.0)))
            .child(row().w_full().gap(8.0).child(spacer())
                .child(button(format!("comment-cancel-{stream}"), "Later", "esc", true).on_press(move |s: &mut State| stop_composing(s, stream)))
                .child(button(format!("comment-post-{stream}"), post_label, "↩", !posting).on_press(move |s: &mut State| post(s, stream)))));
    }
    out.child(bar)
}
