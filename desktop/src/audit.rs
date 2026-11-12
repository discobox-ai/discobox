//! A box's audit view: its audit trails as one timeline, followed, with a record read in full and
//! its recordings read over it.
//!
//! What the timeline holds, which records fold into one row and what that row says, when each was
//! recorded as the timeline says it, what it says with none, and what a record offers to read are
//! the core's (the audit topic: `console.AuditRuns`, `AuditRunSummary`, `AuditTime`; the audit
//! command: a record as `discobox admin audit get` prints it, `AuditRecordDetail.AuditRecordings`,
//! and a recording as `audit http --body` prints it). The view is the TUI's audit screen drawn
//! natively: beside the list, in place of the workspace, which goes on underneath; oldest at the
//! top and newest at the bottom, the cursor riding the newest while it follows. Moving off the
//! newest stops it there — a record arriving never moves the row being read — and moving back
//! down to it, or End, follows again. A folded run opens to its records (Enter or Right) and
//! folds again (Left); Enter on a record reads it in full, over the window, and a recording it
//! offers is read over that, Esc going back a step at a time.
//!
//! `/` (or the find gesture) searches the timeline as it is typed, as the TUI's does: which
//! records the search lets through, the runs folded over them, what it lights on each, and how
//! many it let through of how many are the core's (the audit topic's query: `console.AuditSearch`,
//! `AuditLit`, `AuditTally`), the app subscribing again for each query. While it is typed the
//! arrows still move; Enter keeps it and gives the keys back; Esc clears it before it leaves.

use std::collections::HashSet;

use rune::{Color, El, Key, KeyEvent, ScrollSpec, Span, TextEdit, Tx, col, el, icon, rich, row, spacer, text, text_input};

use crate::wire::{Args, Audit, AuditQuery, AuditText, Result as CommandResult, Status, Subscribe, Topic};
use crate::{Focus, Pending, State, icons, reader, theme, view};

/// How many rows PageUp and PageDown move.
const PAGE: usize = 20;

/// One row of the timeline: a run (by its index in the core's runs) at its first record, folded
/// or as the head of its opened records, or one record of an opened run.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct Row {
    pub run: usize,
    pub record: usize,
    pub member: bool,
}

/// The audit view while it is up: the box it follows, the subscription following it, the last
/// timeline the core sent, and what the person has done with it.
pub struct AuditView {
    pub sandbox: String,
    pub sub: u32,
    pub timeline: Audit,
    /// Why the timeline could not be read, once the whole read failed: nothing more arrives.
    pub error: Option<String>,
    /// The opened runs, by the ID of their first record, which does not change as a run grows.
    pub expanded: HashSet<String>,
    pub rows: Vec<Row>,
    pub cursor: usize,
    /// Whether the cursor rides the newest row.
    pub following: bool,
    /// The row to scroll into view on the next frame, once (`reveal`).
    reveal: bool,
    /// What was read of a record, over the view: the record in full, then a recording of it on
    /// top. Esc takes the top one away.
    pub reading: Vec<Reading>,
    /// The read in flight, by its command: only its answer is taken, and only while what it was
    /// asked from is still up (`landed`).
    asked: Option<u64>,
    /// What a record's read said on the status line, taken back once it lands or is dropped only
    /// while the line still says it.
    said: Option<String>,
    /// The search, as typed: focused while it is being typed. The timeline is what the core sent
    /// for it once `sub` answers.
    pub query: TextEdit,
    /// Whether `sub` has yet to answer a new search: its first timeline lays out every row
    /// afresh, and the row held is scrolled back into view.
    pub requeried: bool,
}

/// A record or one of its recordings, read; the recording the keys are on; and what the panel
/// says of a recording being read from it, or why it could not be.
pub struct Reading {
    pub text: AuditText,
    pub picked: usize,
    pub status: Option<(String, bool)>,
}

impl AuditView {
    /// The record a row is drawn from: its own, or for a folded run's head, its newest.
    fn shown(&self, r: Row) -> usize {
        match self.timeline.runs.get(r.run) {
            Some(run) if !r.member && self.folded(r) => run.last as usize,
            _ => r.record,
        }
    }

    /// Whether a row is a run's head with its records folded behind it.
    fn folded(&self, r: Row) -> bool { !r.member && self.is_run(r) && !self.opened(r) }

    fn is_run(&self, r: Row) -> bool { self.timeline.runs.get(r.run).is_some_and(|run| run.last > run.first) }

    fn opened(&self, r: Row) -> bool {
        self.timeline.records.get(r.record).is_some_and(|rec| self.expanded.contains(&rec.id))
    }

    /// A row's identity across timelines: the record it starts with, and whether it is a record
    /// of an opened run. A record alone and the head of the run it starts are the same row, so a
    /// record the next one arrives to join keeps the cursor.
    fn identity(&self, r: Row) -> Option<(String, bool)> { self.timeline.records.get(r.record).map(|rec| (rec.id.clone(), r.member)) }

    /// Whether the timeline in hand is narrowed by a search: the core says how many it let
    /// through only under one.
    fn searched(&self) -> bool { !self.timeline.tally.is_empty() }

    /// Lays the rows out again after a change to what they are drawn from — a timeline arriving,
    /// under a search or not, a run opening or folding — keeping the cursor on the row it was on,
    /// or on the newest while following. The row is named before the change: the history letting
    /// go of its oldest records moves every index.
    fn relayout(&mut self, change: impl FnOnce(&mut AuditView)) {
        let keep = self.rows.get(self.cursor).and_then(|&r| self.identity(r));
        change(self);
        self.rows.clear();
        let mut live = HashSet::new();
        for (i, run) in self.timeline.runs.iter().enumerate() {
            let (first, last) = (run.first as usize, run.last as usize);
            self.rows.push(Row { run: i, record: first, member: false });
            let id = self.timeline.records.get(first).map(|r| r.id.clone()).unwrap_or_default();
            if last > first && self.expanded.contains(&id) {
                live.insert(id);
                self.rows.extend((first..=last).map(|k| Row { run: i, record: k, member: true }));
            }
        }
        // A run whose head the history let go of is not coming back. Under a search a run can
        // start at a different record, so what was opened is kept for when it is put away.
        if !self.searched() {
            self.expanded = live;
        }
        let newest = self.rows.len().saturating_sub(1);
        if self.following || keep.is_none() {
            self.cursor = newest;
            self.following = true;
            return;
        }
        match self.rows.iter().rposition(|&r| self.identity(r) == keep) {
            Some(i) => self.cursor = i,
            // The row is gone — the search hid it, or the history let it go — so the cursor goes
            // to the newest, the best match in a timeline, and follows.
            None => {
                self.cursor = newest;
                self.following = true;
            }
        }
    }

    /// Moves the cursor, following again once it is back on the newest.
    fn step(&mut self, by: isize) {
        let newest = self.rows.len().saturating_sub(1);
        self.cursor = self.cursor.saturating_add_signed(by).min(newest);
        self.following = self.cursor == newest;
        self.reveal = true;
    }

    /// Opens or folds the run under the cursor, or the run the record under it belongs to.
    /// Folding from inside a run puts the cursor on the run.
    fn toggle(&mut self, open: bool) {
        let Some(&at) = self.rows.get(self.cursor) else { return };
        if at.member && open {
            return;
        }
        let head = Row { member: false, ..at };
        let head = if at.member { Row { record: self.timeline.runs[at.run].first as usize, ..head } } else { head };
        if !self.is_run(head) || self.opened(head) == open {
            return;
        }
        let id = self.timeline.records[head.record].id.clone();
        let following = self.following;
        self.following = false;
        if at.member {
            self.cursor = self.rows.iter().position(|&r| r == head).unwrap_or(self.cursor);
        }
        self.relayout(|v| {
            if open {
                v.expanded.insert(id);
            } else {
                v.expanded.remove(&id);
            }
        });
        self.following = following && self.cursor == self.rows.len().saturating_sub(1);
        self.reveal = true;
    }

    /// What the view says of the records newer than the row under the cursor: how many, in the
    /// core's words (the timeline's `below`) — matches, under a search. A folded run holds its
    /// own newer records, so they are not below it.
    fn below(&self) -> String {
        let n = match self.rows.get(self.cursor) {
            Some(&r) => {
                let through = if self.folded(r) { self.timeline.runs[r.run].last as usize } else { r.record };
                self.timeline.records.len().saturating_sub(through + 1)
            }
            None => 0,
        };
        let words = &self.timeline.below;
        format!("{n} {}", if n == 1 { &words.one } else { &words.many })
    }
}

/// The audit command on a box: the core has ruled it can run, and the view opens on it,
/// following its timeline (the audit topic) from the start. Opening it again starts over: the
/// records are the server's, and the view holds only what it has been sent.
pub fn open(s: &mut State, sandbox: &str) {
    s.close_pages();
    s.close_overlays();
    s.focus = Focus::List;
    let sub = s.subscribe(Subscribe { topic: Topic::Audit, r#box: sandbox.to_string(), ..Default::default() });
    s.audit = Some(AuditView {
        sandbox: sandbox.to_string(), sub, timeline: Audit::default(), error: None, expanded: HashSet::new(), rows: vec![],
        cursor: 0, following: true, reveal: false, reading: vec![], asked: None, said: None, query: TextEdit::new(""),
        requeried: false,
    });
}

/// `/` on the view: the search takes the keys, on what it holds.
pub fn search(s: &mut State) {
    if let Some(v) = s.audit.as_mut() {
        v.query.focused = true;
    }
}

/// The search changed: the core is asked for the timeline under it, in place of what it sent for
/// the last. The new subscription is made before the old one ends, so the bridge narrows the
/// trails it is reading rather than reading them again; what the old one still sends is dropped.
pub fn searched(s: &mut State) {
    let Some(v) = s.audit.as_ref() else { return };
    let (sandbox, text, old) = (v.sandbox.clone(), v.query.text.clone(), v.sub);
    let audit = (!text.trim().is_empty()).then_some(AuditQuery { text });
    let sub = s.subscribe(Subscribe { topic: Topic::Audit, r#box: sandbox, audit, ..Default::default() });
    s.unsubscribe(old);
    if let Some(v) = s.audit.as_mut() {
        v.sub = sub;
        v.requeried = true;
    }
}

/// Puts the search away: the whole timeline again, and the keys back to the rows.
fn clear(s: &mut State) {
    let Some(v) = s.audit.as_mut() else { return };
    let had = !v.query.text.is_empty();
    v.query = TextEdit::new("");
    if had {
        searched(s);
    }
}

/// Takes the view down and stops following, leaving the workspace it covered as it was.
pub fn close(s: &mut State) {
    if let Some(v) = s.audit.take() {
        s.unsubscribe(v.sub);
        // A read still in flight is for nobody now: what it said goes with it.
        if v.said.is_some() && s.message == v.said {
            s.message = None;
        }
    }
}

/// A timeline the core sent, whole; or why the trails could not be read, after which the last
/// one stands. A timeline read since — a search after a failure reads the trails afresh — puts
/// the failure away. One from a subscription the view no longer follows is dropped.
pub fn take(s: &mut State, sub: u32, r: Result<Audit, String>) {
    let Some(v) = s.audit.as_mut().filter(|v| v.sub == sub) else { return };
    match r {
        Ok(timeline) => {
            v.error = None;
            v.relayout(|v| v.timeline = timeline);
            // A new search moves every row: the one held is brought back into view, once — the
            // flag is spent on its first timeline whether or not the view follows.
            let requeried = std::mem::take(&mut v.requeried);
            v.reveal |= v.following || requeried;
        }
        Err(why) => v.error = Some(why),
    }
}

/// Scrolls the cursor's row into view, once: called each frame.
pub fn reveal(s: &mut State, tx: &mut Tx) {
    let Some(v) = s.audit.as_mut().filter(|v| v.reveal) else { return };
    v.reveal = false;
    if let Some(&r) = v.rows.get(v.cursor) {
        tx.scroll_into_view_in("audit-rows", row_key(r));
    }
}

fn row_key(r: Row) -> String { format!("audit-row-{}-{}", r.record, if r.member { "m" } else { "h" }) }

/// Enter on a row: a folded run opens, an opened run's head folds, and a record is read in full.
fn enter(s: &mut State) {
    let Some(v) = s.audit.as_mut() else { return };
    let Some(&r) = v.rows.get(v.cursor) else { return };
    if !r.member && v.is_run(r) {
        let open = !v.opened(r);
        v.toggle(open);
        return;
    }
    let id = v.timeline.records[r.record].id.clone();
    let said = format!("reading {id}…");
    v.said = Some(said.clone());
    s.message = Some(said);
    read(s, Args { record: id, ..Default::default() });
}

/// Asks the core to read a record, or a recording of it: the read in flight, in place of any
/// other, so a second Enter before the first lands never stacks the record twice.
fn read(s: &mut State, args: Args) {
    let Some(sandbox) = s.audit.as_ref().map(|v| v.sandbox.clone()) else { return };
    let context = s.context();
    let id = s.next_id;
    s.command("audit", vec![sandbox], context, args, Pending::Audit);
    if let Some(v) = s.audit.as_mut() {
        v.asked = Some(id);
    }
}

/// The answer to a read: a record goes over the timeline, a recording over the record it was read
/// from, and a failure is said where it was asked — on the status line for a record, on the
/// record's panel for a recording. Only the read in flight lands, and only while the view, and
/// the record a recording was read from, are what is up: anything else is for nobody, and what
/// it said it was doing is taken back.
pub fn landed(s: &mut State, r: &CommandResult) {
    // Something come in front of the view since the read was asked keeps the keys: what lands
    // then is for nobody.
    let covered = s.overlaid();
    let Some(v) = s.audit.as_mut().filter(|v| v.asked == Some(r.id)) else { return };
    v.asked = None;
    // What this read said on the status line is taken back, and nothing anybody said since.
    if let Some(said) = v.said.take()
        && s.message.as_ref() == Some(&said)
    {
        s.message = None;
    }
    let recording = r.audit.as_ref().is_some_and(|t| t.part.is_some()) || v.reading.last().is_some_and(|top| top.status.as_ref().is_some_and(|(_, failed)| !failed));
    if recording {
        let Some(top) = v.reading.last_mut().filter(|top| top.text.part.is_none()) else { return };
        match (r.status, r.audit.clone()) {
            (Status::Done, Some(text)) if !covered && text.record == top.text.record => {
                top.status = None;
                v.reading.push(Reading { text, picked: 0, status: None });
            }
            (Status::Failed, _) => top.status = Some((r.why.clone(), true)),
            _ => top.status = None,
        }
        return;
    }
    match (r.status, r.audit.clone()) {
        (Status::Done, Some(text)) if !covered => v.reading.push(Reading { text, picked: 0, status: None }),
        (Status::Failed, _) => s.message = Some(r.why.clone()),
        _ => {}
    }
}

/// The view's own keys while it is up, as a screen's are: the arrows, PageUp and PageDown, Home
/// and End move (End follows again), Enter opens a run or reads a record, Right and Left open and
/// fold a run, Esc leaves. Anything else goes on to the window's shortcuts.
pub fn page_key(s: &mut State, ev: &KeyEvent) -> bool {
    let plain = !(ev.mods.ctrl || ev.mods.alt || ev.mods.cmd);
    let Some(v) = s.audit.as_mut().filter(|_| plain) else { return false };
    match ev.key {
        Key::Up => v.step(-1),
        Key::Down => v.step(1),
        Key::PageUp => v.step(-(PAGE as isize)),
        Key::PageDown => v.step(PAGE as isize),
        Key::Home => v.step(isize::MIN / 2),
        Key::End => v.step(isize::MAX / 2),
        Key::Right => v.toggle(true),
        Key::Left => v.toggle(false),
        Key::Enter if !ev.mods.shift => enter(s),
        Key::Text(ref t) if t == "/" => search(s),
        // A search is put away before the view is: Esc undoes the last thing done, and the
        // search is that.
        Key::Escape if !v.query.text.is_empty() => clear(s),
        Key::Escape => close(s),
        _ => return false,
    }
    true
}

/// The search field's keys while it is typed: every character is the query's, the arrows still
/// move through what it lets through, Enter keeps it and gives the keys back to the rows, and Esc
/// clears it.
fn query_key(s: &mut State, ev: &KeyEvent, _tx: &mut Tx) -> bool {
    let Some(v) = s.audit.as_mut() else { return false };
    match ev.key {
        Key::Up => v.step(-1),
        Key::Down => v.step(1),
        Key::PageUp => v.step(-(PAGE as isize)),
        Key::PageDown => v.step(PAGE as isize),
        Key::Enter => v.query.focused = false,
        Key::Escape => clear(s),
        _ => return false,
    }
    true
}

/// The keys of what was read, over the view: Left and Right (and Tab) move along the recordings
/// it offers, Enter reads the one picked, Esc goes back a step.
pub fn reading_key(s: &mut State, ev: &KeyEvent) -> bool {
    let Some(v) = s.audit.as_mut() else { return false };
    let Some(top) = v.reading.last_mut() else { return false };
    let n = top.text.recordings.len();
    match ev.key {
        Key::Escape => {
            v.reading.pop();
            v.asked = None;
        }
        Key::Left if n > 0 => top.picked = (top.picked + n - 1) % n,
        Key::Right | Key::Tab if n > 0 => top.picked = (top.picked + 1) % n,
        Key::Enter if n > 0 => {
            let i = top.picked;
            recording(s, i);
        }
        _ => {}
    }
    true
}

/// Reads one of the recordings the record on top offers.
fn recording(s: &mut State, i: usize) {
    let Some(v) = s.audit.as_ref() else { return };
    let Some(top) = v.reading.last() else { return };
    let Some(rec) = top.text.recordings.get(i) else { return };
    let args = Args { record: top.text.record.clone(), part: Some(rec.part), ..Default::default() };
    // The panel is the whole of what is in front: it says what it is reading, not the status
    // line behind it.
    let what = format!("reading the {}…", rec.label);
    if let Some(top) = s.audit.as_mut().and_then(|v| v.reading.last_mut()) {
        top.status = Some((what, false));
    }
    read(s, args);
}

// ---------------------------------------------------------------------------------------------
// Drawing

const ROW_H: f32 = 24.0;
const TIME_W: f32 = 112.0;
const SOURCE_W: f32 = 72.0;
const FOLD_W: f32 = 16.0;

fn t(s: impl Into<String>, c: Color) -> El { text(s).font(theme::UI).color(c) }

fn hint(key: &str, what: &str) -> El {
    row().gap(4.0).center_y().child(view::keycap(crate::keys::label(key))).child(text(what.to_string()).font(theme::UI_SMALL).color(theme::dim()))
}

/// What a search lit in a line (`console.AuditLit`, by rune), as the spans that draw it: each run
/// of lit runes at once, bold in the accent — what the search has its eye on.
fn lit(line: &str, runes: &[i64]) -> Vec<Span> {
    let at: Vec<usize> = line.char_indices().map(|(b, _)| b).chain(std::iter::once(line.len())).collect();
    let mut spans = vec![];
    let mut i = 0;
    while i < runes.len() {
        let start = runes[i].max(0) as usize;
        let mut end = start + 1;
        i += 1;
        while i < runes.len() && runes[i] as usize == end {
            end += 1;
            i += 1;
        }
        if end < at.len() {
            spans.push(Span::new(at[start], at[end]).bold().color(theme::mark()));
        }
    }
    spans
}

fn audit_row(v: &AuditView, i: usize, r: Row) -> El {
    let on = i == v.cursor;
    let shown = v.shown(r);
    let rec = &v.timeline.records[shown];
    // A run's head says the run's line, and is lit where that line matches.
    let run = (!r.member && v.is_run(r)).then(|| &v.timeline.runs[r.run]);
    let (summary, source_lit, summary_lit) = match run {
        Some(run) => (run.summary.clone(), &run.source_lit, &run.summary_lit),
        None => (rec.summary.clone(), &rec.source_lit, &rec.summary_lit),
    };
    let fold: El = match (r.member, v.is_run(r), v.opened(r)) {
        (false, true, false) => icon(&icons::CHEVRON, 10.0).color(theme::grey()),
        (false, true, true) => icon(&icons::CARET, 10.0).color(theme::grey()),
        _ => el(),
    };
    let pick = move |s: &mut State| {
        if let Some(v) = s.audit.as_mut() {
            v.cursor = i.min(v.rows.len().saturating_sub(1));
            v.following = v.cursor + 1 == v.rows.len();
        }
    };
    row().key(row_key(r)).w_full().h(ROW_H).px(10.0).gap(12.0).center_y().radius(theme::r_row())
        .fill(if on { theme::selection() } else { Color::TRANSPARENT }).hover(theme::text().alpha(0.06)).motion(theme::SNAPPY)
        .cursor(rune::Cursor::Pointer)
        .child(t(rec.clock.clone(), theme::dim()).w(TIME_W))
        .child(rich(rec.source.clone(), lit(&rec.source, source_lit)).font(theme::UI).color(theme::grey()).truncate().w(SOURCE_W))
        .child(row().w(FOLD_W).pl(if r.member { FOLD_W } else { 0.0 }).center_y().child(fold))
        .child(rich(summary.clone(), lit(&summary, summary_lit)).font(if on { theme::UI_BOLD } else { theme::UI })
            .color(if on { theme::text() } else { theme::grey() }).truncate().shrink().grow(1.0))
        .on_press(pick)
        .on_double_press(move |s: &mut State| {
            pick(s);
            enter(s);
        })
}

/// The view: its title band with whether it follows, the column heads, a row per run or record,
/// what it says with none, what the last read could not reach, and its keys.
pub fn page_view(s: &State) -> El {
    let Some(v) = s.audit.as_ref() else { return el() };
    let name = s.box_by_id(&v.sandbox).map(|b| b.name.clone()).unwrap_or_else(|| v.sandbox.clone());
    let state = if v.error.is_some() {
        el()
    } else if v.following || v.rows.is_empty() {
        row().key("audit-following").child(t("following", theme::on_gold()).font(theme::UI_BOLD))
    } else {
        t(v.below(), theme::on_gold()).font(theme::UI_BOLD).key("audit-below")
    };
    let mut body = col().key("audit").w_full().h_full().gap(2.0).min_h(0.0)
        .child(row().w_full().h(26.0).px(10.0).center_y().fill(theme::gold()).radius(theme::r_row())
            .child(text(format!("Audit · {name}")).font(theme::UI_BOLD).color(theme::on_gold()))
            .child(spacer())
            .child(state))
        .child(row().w_full().h(22.0).px(10.0).gap(12.0).center_y()
            .child(t("TIME", theme::dim()).font(theme::UI_SMALL).w(TIME_W))
            .child(t("SOURCE", theme::dim()).font(theme::UI_SMALL).w(SOURCE_W))
            .child(el().w(FOLD_W))
            .child(t("RECORD", theme::dim()).font(theme::UI_SMALL)));
    let mut rows = col().key("audit-rows").w_full().gap(1.0);
    // What the timeline says with no record is the core's, and a read that failed whole outranks
    // it: it is said under the records instead.
    if v.rows.is_empty() && !v.timeline.note.is_empty() && v.error.is_none() {
        rows = rows.child(row().key("audit-note").padding(10.0).child(t(v.timeline.note.clone(), theme::dim())));
    }
    for (i, &r) in v.rows.iter().enumerate() {
        rows = rows.child(audit_row(v, i, r));
    }
    body = body.child(rows.grow(1.0).min_h(0.0).scroll(ScrollSpec { stick_bottom: false }));
    // The search line is under the records it narrows, where the TUI's is, with how many it let
    // through of how many — the core's words.
    if v.query.focused || !v.query.text.is_empty() {
        let field = text_input(&v.query, theme::UI.lh(20.0)).key("audit-query").single_line().grow(1.0).color(theme::text())
            .placeholder("search the timeline", theme::dim())
            .on_edit(|s: &mut State| &mut s.audit.as_mut().unwrap().query)
            .on_input_change(|s: &mut State, _| searched(s))
            .on_input_key(query_key);
        body = body.child(row().key("audit-search").w_full().h(30.0).px(10.0).gap(8.0).center_y().radius(theme::r_row()).fill(theme::sunken())
            .border(theme::stroke_quiet(), if v.query.focused { theme::mark() } else { theme::line_bright() })
            .child(icon(&icons::SEARCH, 14.0).color(theme::grey()))
            .child(field)
            .child(t(v.timeline.tally.clone(), theme::dim()).font(theme::UI_SMALL)));
    }
    // What is missing is said under the records, and outranks them: a quiet timeline and a trail
    // that stopped answering look the same without it.
    for (i, gap) in v.timeline.missing.iter().chain(v.error.iter()).enumerate() {
        body = body.child(row().key(format!("audit-gap-{i}")).w_full().px(10.0).child(t(gap.clone(), theme::err()).font(theme::UI_SMALL).truncate().shrink()));
    }
    let mut hints = row().w_full().h(24.0).px(10.0).gap(14.0).center_y();
    if let Some(&r) = v.rows.get(v.cursor) {
        hints = match (r.member, v.is_run(r), v.opened(r)) {
            (true, _, _) => hints.child(hint("enter", "open")).child(hint("left", "fold")),
            (false, true, true) => hints.child(hint("enter", "fold")),
            (false, true, false) => hints.child(hint("enter", "expand")),
            _ => hints.child(hint("enter", "open")),
        };
    }
    if !v.following {
        hints = hints.child(hint("end", "follow"));
    }
    hints = if v.query.text.is_empty() {
        hints.child(hint("/", "search")).child(hint("escape", "back"))
    } else {
        hints.child(hint("/", "edit search")).child(hint("escape", "clear search"))
    };
    hints = hints
        .child(spacer())
        .child(t(s.message.clone().unwrap_or_default(), theme::grey()).font(theme::UI_SMALL).truncate().shrink());
    body = body.child(hints);
    col().key("audit-page").grow(1.0).h_full().min_w(0.0).padding(10.0).radius(theme::r_frame())
        .border(theme::stroke_focus(), theme::mark()).child(body)
}

/// A recording button: the picked one outlined.
fn recording_button(label: &str, on: bool, i: usize) -> El {
    row().key(format!("audit-recording-{i}")).h(28.0).px(14.0).center_y().radius(theme::r_row())
        .fill(if on { theme::mark().alpha(0.18) } else { Color::TRANSPARENT })
        .border(if on { theme::stroke_focus() } else { theme::stroke_quiet() }, if on { theme::mark() } else { theme::line_bright() })
        .hover(theme::text().alpha(0.06)).cursor(rune::Cursor::Pointer).motion(theme::SNAPPY)
        .child(text(label.to_string()).font(if on { theme::UI_BOLD } else { theme::UI }).color(theme::text()))
        .on_press(move |s: &mut State| {
            if let Some(top) = s.audit.as_mut().and_then(|v| v.reading.last_mut()) {
                top.picked = i;
            }
            recording(s, i);
        })
}

/// What was read as the panel draws it: the core's text without its last newline, its tabs set
/// as spaces. Its lines are what its find reads (`reader`).
pub fn reading_body(text: &str) -> String { text.trim_end_matches('\n').replace('\t', "    ") }

/// What was read, over the window: its title and record, its text as the core printed it, and
/// the recordings it offers to read. It is found through, copied and scrolled as any read-only
/// card is (`reader`).
pub fn reading_view(s: &State) -> Option<El> {
    let top = s.audit.as_ref()?.reading.last()?;
    // One element for the whole text, its lines broken where it breaks them: a body is up to the
    // core's cap, and can be nearly all line breaks. Its lines are lit and anchored inside it.
    let read = reading_body(&top.text.text);
    let pieces: Vec<String> = read.split('\n').map(str::to_string).collect();
    let mut offsets = Vec::with_capacity(pieces.len());
    let mut at = 0;
    for p in &pieces {
        offsets.push(at);
        at += p.len() + 1;
    }
    let lit = reader::highlights(s, &pieces, 0..pieces.len(), &offsets);
    let body = t(read, theme::text()).font(theme::UI_SMALL).wrap().w_full().text_highlights(lit);
    let lines = col().key("audit-text").w_full().padding(10.0).radius(theme::r_row()).fill(theme::sunken()).child(body);
    let mut foot = row().w_full().gap(8.0).center_y()
        .child(hint("escape", if s.audit.as_ref().is_some_and(|v| v.reading.len() > 1) { "back to the record" } else { "close" }));
    if let Some((what, failed)) = &top.status {
        foot = foot.child(row().key("audit-reading-status").shrink().min_w(0.0).child(t(what.clone(), if *failed { theme::err() } else { theme::grey() }).font(theme::UI_SMALL).truncate().shrink()));
    }
    foot = foot.child(spacer());
    for (i, rec) in top.text.recordings.iter().enumerate() {
        foot = foot.child(recording_button(&rec.label, top.picked == i, i));
    }
    let card = col().key("audit-reading").w(860.0).max_h(760.0).padding(18.0).gap(12.0).radius(theme::r_card())
        .fill(theme::raised()).border(theme::stroke_focus(), theme::mark()).shadow(16.0, 48.0, Color::BLACK.alpha(0.55))
        .child(row().w_full().center_y()
            .child(text(top.text.title.clone()).font(theme::UI_BOLD).color(theme::text()))
            .child(spacer())
            .child(t(top.text.record.clone(), theme::dim())))
        .child(reader::follow_scroll(s, lines.scroll(ScrollSpec { stick_bottom: false }).max_h(600.0).min_h(0.0)))
        .child(reader::foot(s, &pieces))
        .child(foot);
    Some(el().w_full().h_full().absolute(0.0, 0.0).fill(theme::bg().alpha(0.6))
        .on_press(|s: &mut State| {
            if let Some(v) = s.audit.as_mut() {
                v.reading.pop();
            }
        })
        .child(row().w_full().pt(60.0).items_start().child(spacer()).child(card.on_press(|_: &mut State| {})).child(spacer())))
}
