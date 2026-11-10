//! The harnesses page, a harness's config card, and a box's harness changes reviewed.
//!
//! What is listed, what each harness's actions are and whether each applies, what a reset asks,
//! what the card says and what the review offers are the core's (`console.Harness.Actions`,
//! `ResetQuestion`, `HarnessCard`, `HarnessChanges.Offers`), carried by the harnesses and
//! review config commands: the page is opened on the harnesses the command read, each action is
//! the same command naming the harness and the action, and its result carries the harnesses
//! read again. A file is edited through the core's `edit`, which the sheets answer
//! (`sheet::take_edit`). The page is drawn beside the list, in place of the workspace, and has
//! its own keys while it is up, as a screen does; the card and the review are panels over the
//! window.

use rune::{Color, El, Key, KeyEvent, PointerPress, ScrollSpec, Tx, col, el, row, spacer, text};

use crate::wire::{Args, CardRow, Entry, Harness, HarnessAction, HarnessChange, HarnessChanges, Result as CommandResult};
use crate::{Menu, MenuKind, Pending, State, theme, view};

/// The harnesses page: the harnesses of the server it was opened for, and the one the keys are on.
pub struct HarnessPage {
    pub server: String,
    pub rows: Vec<Harness>,
    pub cursor: usize,
}

impl HarnessPage {
    pub fn current(&self) -> Option<&Harness> { self.rows.get(self.cursor) }

    /// Takes the harnesses read again, with the keys on `about` when it is one of them (the one
    /// acted on, a variant made), else on the harness they were on.
    fn take(&mut self, rows: Vec<Harness>, about: &str) {
        let on = if about.is_empty() { self.current().map(|h| h.id.clone()).unwrap_or_default() } else { about.to_string() };
        self.rows = rows;
        self.cursor = self.rows.iter().position(|h| h.id == on).unwrap_or(self.cursor.min(self.rows.len().saturating_sub(1)));
    }
}

/// A harness's config card, as the core says it.
pub struct Card {
    pub title: String,
    pub rows: Vec<CardRow>,
}

/// A box's harness changes under review: what the core read, and the offer the keys are on.
pub struct Review {
    pub sandbox: String,
    pub changes: HarnessChanges,
    pub picked: usize,
}

/// The harnesses command, opening the page on the server the list is narrowed to (the primary
/// for none). What it read opens the page (`open_page`).
pub fn open(s: &mut State) {
    let server = s.listing.filter.server.clone();
    let context = s.context();
    s.command("harnesses", vec![], context, Args { server: server.clone(), ..Default::default() }, Pending::Page(server));
}

/// The page, on the harnesses the core read for it from `server`: the server it was asked of,
/// whatever the list's filter has moved to since, which every action on the page goes to.
///
/// The page takes the keys: whatever it does not answer goes to the window's shortcuts and to the
/// list, never to the terminals of a workspace it covers, and a box command acts on the list's
/// choice rather than on a box no longer on screen.
pub fn open_page(s: &mut State, server: String, r: &CommandResult) {
    s.close_overlays();
    s.close_pages();
    s.focus = crate::Focus::List;
    s.harnesses = Some(HarnessPage { server, rows: r.harnesses.clone(), cursor: 0 });
    if let Some(p) = s.harnesses.as_mut()
        && let Some(i) = p.rows.iter().position(|h| h.default)
    {
        p.cursor = i;
    }
}

pub fn close_page(s: &mut State) { s.harnesses = None; }

/// Does one of the harness's actions, as the core offers it: one that does not apply says why,
/// and the files action first asks which file.
pub fn act(s: &mut State, action: HarnessAction, path: String, at: (f32, f32)) {
    let Some(h) = s.harnesses.as_ref().and_then(HarnessPage::current).cloned() else {
        s.message = Some("no harnesses to act on".into());
        return;
    };
    let Some(offer) = h.actions.iter().find(|a| a.action == action) else { return };
    if !offer.enabled {
        s.message = Some(format!("cannot {} {}: {}", offer.label, h.name, offer.why));
        return;
    }
    if action == HarnessAction::Files && path.is_empty() {
        open_files(s, at);
        return;
    }
    s.close_overlays();
    s.message = Some(match action {
        HarnessAction::Config => format!("reading {}…", h.name),
        HarnessAction::Files => format!("opening {path}…"),
        _ => String::new(),
    }).filter(|m| !m.is_empty());
    let server = s.harnesses.as_ref().map(|p| p.server.clone()).unwrap_or_default();
    let context = s.context();
    let args = Args { server, harness: h.id.clone(), action: Some(action), path, ..Default::default() };
    s.command("harnesses", vec![], context, args, Pending::Harness(h.name.clone()));
}

/// What an action came to: the harnesses read again, the keys on the one it was about, and a
/// card for the config action.
pub fn take(s: &mut State, name: &str, r: &CommandResult) {
    if !r.card.is_empty() {
        s.message = None;
        s.card = Some(Card { title: name.to_string(), rows: r.card.clone() });
        return;
    }
    if let Some(p) = s.harnesses.as_mut()
        && !r.harnesses.is_empty()
    {
        p.take(r.harnesses.clone(), &r.harness);
    }
    s.message = (!r.note.is_empty()).then(|| r.note.clone());
}

/// The harness under the keys' actions, as a menu: each as the core offers it, or why not.
pub fn open_menu(s: &mut State, at: (f32, f32)) {
    let Some(h) = s.harnesses.as_ref().and_then(HarnessPage::current).cloned() else { return };
    s.close_overlays();
    let entries: Vec<Entry> = h.actions.iter().map(|a| Entry {
        command: a.action.as_str().into(), label: a.label.clone(), detail: a.detail.clone(), enabled: a.enabled, why: a.why.clone(), uses: 0,
    }).collect();
    let picked = entries.iter().position(|e| e.enabled).unwrap_or(0);
    s.menu = Some(Menu { kind: MenuKind::Harness(h.id.clone()), at, entries, picked, sub: 0 });
}

/// The harness's files, to pick the one to edit: each by its path, its size and what the core
/// says of it.
fn open_files(s: &mut State, at: (f32, f32)) {
    let Some(h) = s.harnesses.as_ref().and_then(HarnessPage::current).cloned() else { return };
    s.close_overlays();
    let entries = h.files.iter().map(|f| {
        let detail = std::iter::once(size(f.size)).chain(f.notes.iter().cloned()).collect::<Vec<_>>().join(" · ");
        Entry { command: f.path.clone(), label: f.path.clone(), detail, enabled: true, why: String::new(), uses: 0 }
    }).collect();
    s.menu = Some(Menu { kind: MenuKind::HarnessFiles(h.id.clone()), at, entries, picked: 0, sub: 0 });
}

/// A size in bytes, in binary units.
fn size(n: i64) -> String {
    let units = ["B", "KiB", "MiB", "GiB"];
    let (mut v, mut i) = (n as f64, 0);
    while v >= 1024.0 && i + 1 < units.len() {
        v /= 1024.0;
        i += 1;
    }
    if i == 0 { format!("{n} B") } else if v < 10.0 { format!("{v:.1} {}", units[i]) } else { format!("{v:.0} {}", units[i]) }
}

/// Where a harness's menu opens from the keys: beside its row.
fn row_at(s: &State) -> (f32, f32) {
    let left = if s.sidebar { 340.0 } else { 40.0 };
    (left + 260.0, 130.0 + s.harnesses.as_ref().map_or(0, |p| p.cursor) as f32 * ROW_H)
}

/// The page's own keys while it is up, as a screen's are: the arrows move, Enter reads the
/// harness's config, the context-menu key opens its actions, Esc leaves the page. Anything else
/// goes on to the window's shortcuts.
pub fn page_key(s: &mut State, ev: &KeyEvent, _tx: &mut Tx) -> bool {
    let plain = !(ev.mods.ctrl || ev.mods.alt || ev.mods.cmd);
    let Some(p) = s.harnesses.as_mut().filter(|_| plain) else { return false };
    let last = p.rows.len().saturating_sub(1);
    match ev.key {
        Key::Up => p.cursor = p.cursor.saturating_sub(1),
        Key::Down => p.cursor = (p.cursor + 1).min(last),
        Key::Home => p.cursor = 0,
        Key::End => p.cursor = last,
        Key::Escape => close_page(s),
        Key::Enter if !ev.mods.shift => act(s, HarnessAction::Config, String::new(), (0.0, 0.0)),
        Key::ContextMenu => {
            let at = row_at(s);
            open_menu(s, at);
        }
        Key::Function(10) if ev.mods.shift => {
            let at = row_at(s);
            open_menu(s, at);
        }
        _ => return false,
    }
    true
}

// ---------------------------------------------------------------------------------------------
// A box's harness changes

/// The review config command on a box: what the core read opens the review (`open_review`).
pub fn open_review(s: &mut State, r: &CommandResult) {
    let Some(changes) = r.changes.clone() else { return };
    s.close_overlays();
    // It opens on reading it again, never on saving: a save makes the change every box's on the
    // harness, hooks and commands included, and is chosen, not fallen into with Enter.
    let picked = changes.offers.len();
    s.review = Some(Review { sandbox: r.r#box.clone(), changes, picked });
}

/// Saves or discards the candidate the review shows, by the ID it shows: never a newer one the
/// box captured since.
pub fn change(s: &mut State, change: HarnessChange) {
    let Some(rv) = s.review.as_ref() else { return };
    let Some(offer) = rv.changes.offers.iter().find(|o| o.change == change) else { return };
    if !offer.enabled {
        s.message = Some(offer.why.clone());
        return;
    }
    let (sandbox, candidate) = (rv.sandbox.clone(), rv.changes.candidate.clone());
    s.message = Some(match change {
        HarnessChange::Save => "saving the harness changes…",
        HarnessChange::Discard => "discarding the harness changes…",
    }.into());
    let context = s.context();
    let args = Args { change: Some(change), candidate, ..Default::default() };
    s.command("review config", vec![sandbox], context, args, Pending::Change);
}

/// Reads the box's harness changes again, in place of what the review shows.
fn refresh(s: &mut State) {
    let Some(sandbox) = s.review.as_ref().map(|r| r.sandbox.clone()) else { return };
    let context = s.context();
    s.command("review config", vec![sandbox], context, Args::default(), Pending::Enter("review config".into()));
}

/// The choices along the review's foot: the core's offers, then reading it again.
fn review_choices(rv: &Review) -> usize { rv.changes.offers.len() + 1 }

fn review_choose(s: &mut State, i: usize) {
    let Some(rv) = s.review.as_ref() else { return };
    match rv.changes.offers.get(i) {
        Some(o) => {
            let c = o.change;
            change(s, c);
        }
        None => refresh(s),
    }
}

/// The card's keys and the review's: Esc closes; in the review, Left and Right (and Tab) move
/// along its choices and Enter takes one.
pub fn panel_key(s: &mut State, ev: &KeyEvent) -> bool {
    if s.card.is_some() {
        if matches!(ev.key, Key::Escape | Key::Enter) {
            s.card = None;
        }
        return true;
    }
    let Some(rv) = s.review.as_mut() else { return false };
    let n = review_choices(rv);
    match ev.key {
        Key::Escape => s.review = None,
        Key::Left => rv.picked = (rv.picked + n - 1) % n,
        Key::Right | Key::Tab => rv.picked = (rv.picked + 1) % n,
        Key::Enter => {
            let i = rv.picked;
            review_choose(s, i);
        }
        _ => {}
    }
    true
}

// ---------------------------------------------------------------------------------------------
// Drawing

const ROW_H: f32 = 28.0;

pub(crate) fn t(s: impl Into<String>, c: Color) -> El { text(s).font(theme::UI).color(c) }

fn harness_row(p: &HarnessPage, i: usize, h: &Harness) -> El {
    let on = i == p.cursor;
    let id = h.id.clone();
    let mut r = row().key(format!("harness-{}", h.id)).w_full().h(ROW_H).px(10.0).gap(12.0).center_y().radius(theme::r_row())
        .fill(if on { theme::selection() } else { Color::TRANSPARENT }).hover(theme::text().alpha(0.06)).motion(theme::SNAPPY)
        .cursor(rune::Cursor::Pointer)
        .child(t(h.name.clone(), if on { theme::text() } else { theme::grey() }).font(if on { theme::UI_BOLD } else { theme::UI }).truncate().shrink().grow(1.0))
        .child(t(if h.default { "★ default" } else { "" }, theme::gold()).w(96.0))
        .child(t(if h.built_in { "built-in" } else { "" }, theme::dim()).w(80.0));
    let count = |n: i64, one: &str, many: &str| if n == 0 { String::new() } else { format!("{n} {}", if n == 1 { one } else { many }) };
    r = r.child(t(count(h.secrets, "secret", "secrets"), theme::dim()).w(90.0))
        .child(t(count(h.files.len() as i64, "file", "files"), theme::dim()).w(70.0))
        .child(t(h.age.clone(), theme::dim()).w(80.0))
        .child(t(h.bundle.clone(), theme::dim()).w(260.0).truncate());
    let pick = move |s: &mut State| {
        if let Some(p) = s.harnesses.as_mut() {
            p.cursor = i;
        }
    };
    r.on_press(pick)
        .on_double_press_tx(move |s: &mut State, _tx: &mut Tx| {
            pick(s);
            act(s, HarnessAction::Config, String::new(), (0.0, 0.0));
        })
        .on_secondary_press(move |s: &mut State, at: PointerPress, _tx: &mut Tx| {
            if let Some(p) = s.harnesses.as_mut() {
                p.cursor = p.rows.iter().position(|h| h.id == id).unwrap_or(p.cursor);
            }
            open_menu(s, (at.x, at.y));
        })
}

/// The page: its title band, a row per harness, and what it says when there are none.
pub fn page_view(s: &State) -> El {
    let Some(p) = s.harnesses.as_ref() else { return el() };
    let n = p.rows.len();
    let count = format!("{n} harness{}", if n == 1 { "" } else { "es" });
    let mut body = col().key("harnesses").w_full().h_full().gap(2.0).min_h(0.0)
        .child(row().w_full().h(26.0).px(10.0).center_y().fill(theme::gold()).radius(theme::r_row())
            .child(text("Harnesses").font(theme::UI_BOLD).color(theme::on_gold()))
            .child(spacer())
            .child(text(count).font(theme::UI_BOLD).color(theme::on_gold())));
    if p.rows.is_empty() {
        body = body.child(t("no harnesses in this project — register one with `discobox admin harness create`", theme::dim()).padding(10.0));
    }
    let mut rows = col().w_full().gap(2.0);
    for (i, h) in p.rows.iter().enumerate() {
        rows = rows.child(harness_row(p, i, h));
    }
    let hints = row().w_full().h(24.0).px(10.0).gap(14.0).center_y()
        .child(hint("enter", "config"))
        .child(hint("shift+f10", "actions"))
        .child(hint("escape", "back"))
        .child(spacer())
        .child(t(s.message.clone().unwrap_or_default(), theme::grey()).font(theme::UI_SMALL).truncate().shrink());
    body = body.child(rows.grow(1.0).min_h(0.0).scroll(ScrollSpec { stick_bottom: false })).child(hints);
    col().key("harnesses-page").grow(1.0).h_full().min_w(0.0).padding(10.0).radius(theme::r_frame())
        .border(theme::stroke_focus(), theme::mark()).child(body)
}

pub(crate) fn hint(key: &str, what: &str) -> El {
    row().gap(4.0).center_y().child(view::keycap(crate::keys::label(key))).child(text(what.to_string()).font(theme::UI_SMALL).color(theme::dim()))
}

/// A panel over the window, near its top: the card or the review.
pub(crate) fn panel(name: &str, width: f32, body: El, dismiss: fn(&mut State)) -> El {
    let card = col().key(name.to_string()).w(width).max_h(720.0).padding(18.0).gap(12.0).radius(theme::r_card())
        .fill(theme::raised()).border(theme::stroke_focus(), theme::mark()).shadow(16.0, 48.0, Color::BLACK.alpha(0.55))
        .child(body);
    el().w_full().h_full().absolute(0.0, 0.0).fill(theme::bg().alpha(0.6))
        .on_press(dismiss)
        .child(row().w_full().pt(70.0).items_start().child(spacer()).child(card.on_press(|_: &mut State| {})).child(spacer()))
}

const LABEL_W: f32 = 110.0;

/// The card: each row's label lined up, its value in its tones, and a file's first lines one
/// step in under it.
pub fn card_view(s: &State) -> Option<El> {
    let c = s.card.as_ref()?;
    let mut rows = col().w_full().gap(3.0);
    for r in &c.rows {
        let mut value = row().grow(1.0).shrink().flex_wrap();
        for span in &r.spans {
            value = value.child(t(span.text.clone(), crate::secrets::ink(span.tone)));
        }
        rows = rows.child(row().w_full().gap(8.0).items_start().child(t(r.label.clone(), theme::grey()).w(LABEL_W)).child(value));
        let mut lines = col().w_full().pl(LABEL_W + 20.0);
        for l in &r.lines {
            lines = lines.child(t(l.clone(), theme::dim()).font(theme::UI_SMALL));
        }
        if r.more > 0 {
            lines = lines.child(t(format!("… {} more lines — its files action opens it in your editor", r.more), theme::dim()).font(theme::UI_SMALL));
        }
        rows = rows.child(lines);
    }
    let body = col().w_full().gap(12.0).min_h(0.0)
        .child(row().w_full().center_y().child(text(c.title.clone()).font(theme::UI_BOLD).color(theme::text())).child(spacer()).child(hint("escape", "close")))
        .child(rows.scroll(ScrollSpec { stick_bottom: false }).max_h(600.0));
    Some(panel("harness-card", 760.0, body, |s: &mut State| s.card = None))
}

/// A line of a diff, in the colour of what it does.
fn diff_line(l: &str) -> El {
    let ink = if l.starts_with("+++") || l.starts_with("---") {
        theme::grey()
    } else if l.starts_with('+') {
        theme::done()
    } else if l.starts_with('-') {
        theme::salmon()
    } else if l.starts_with("@@") {
        theme::mark()
    } else {
        theme::text()
    };
    t(l.replace('\t', "    "), ink).font(theme::UI_SMALL)
}

/// A review button: the picked one outlined, one that cannot act dimmed with why on the pointer.
fn choice(key: String, label: &str, on: bool, enabled: bool, why: &str, i: usize) -> El {
    let ink = if enabled { theme::text() } else { theme::dim() };
    let mut b = row().key(key).h(28.0).px(14.0).center_y().radius(theme::r_row())
        .fill(if on && enabled { theme::mark().alpha(0.18) } else { Color::TRANSPARENT })
        .border(if on { theme::stroke_focus() } else { theme::stroke_quiet() }, if on { theme::mark() } else { theme::line_bright() })
        .motion(theme::SNAPPY)
        .child(text(label.to_string()).font(if on { theme::UI_BOLD } else { theme::UI }).color(ink));
    if !why.is_empty() {
        b = b.help(why.to_string());
    }
    if enabled {
        b = b.hover(theme::text().alpha(0.06)).cursor(rune::Cursor::Pointer);
    }
    b.on_press(move |s: &mut State| {
        if let Some(rv) = s.review.as_mut() {
            rv.picked = i;
        }
        review_choose(s, i);
    })
}

/// The review: where the candidate stands, what saving does, each file's diff, the
/// credentials and the diagnostics, and the core's offers.
pub fn review_view(s: &State) -> Option<El> {
    let rv = s.review.as_ref()?;
    let c = &rv.changes;
    let name = s.box_by_id(&rv.sandbox).map(|b| b.name.clone()).unwrap_or_else(|| rv.sandbox.clone());
    let mut sections = col().w_full().gap(10.0);
    if !c.nothing.is_empty() {
        sections = sections.child(t(c.nothing.clone(), theme::dim()));
    }
    for f in &c.files {
        let label = f.label.clone();
        let mut diff = col().w_full().padding(8.0).radius(theme::r_row()).fill(theme::sunken());
        for l in f.diff.lines().filter(|l| !l.is_empty()) {
            diff = diff.child(diff_line(l));
        }
        sections = sections.child(col().w_full().gap(4.0).child(t(label, theme::gold()).font(theme::UI_BOLD)).child(diff));
    }
    for (title, items, ink) in [("credentials", &c.credentials, theme::text()), ("diagnostics", &c.diagnostics, theme::warn())] {
        if items.is_empty() {
            continue;
        }
        let mut list = col().w_full().gap(2.0).child(t(title, theme::gold()).font(theme::UI_BOLD));
        for item in items {
            list = list.child(t(format!("• {item}"), ink));
        }
        sections = sections.child(list);
    }
    let mut buttons = row().w_full().gap(8.0).center_y().child(hint("escape", "close")).child(spacer());
    for (i, o) in c.offers.iter().enumerate() {
        buttons = buttons.child(choice(format!("review-{}", o.change.as_str()), &o.label, rv.picked == i, o.enabled, &o.why, i));
    }
    let refresh = c.offers.len();
    buttons = buttons.child(choice("review-refresh".into(), "Refresh preview", rv.picked == refresh, true, "", refresh));
    let body = col().w_full().gap(12.0).min_h(0.0)
        .child(text(format!("Harness changes — {name}")).font(theme::UI_BOLD).color(theme::text()))
        .child(t(c.summary.clone(), theme::grey()))
        .child(t(c.saving.clone(), theme::grey()).wrap().w_full())
        .child(sections.scroll(ScrollSpec { stick_bottom: false }).max_h(480.0))
        .child(buttons);
    Some(panel("harness-review", 820.0, body, |s: &mut State| s.review = None))
}
