//! Drawing: the list of every box, and a box's workspace — the sidebar of boxes down the left
//! and the box's terminals in two tabbed columns right of it. The structure is the TUI's: framed
//! columns with their sessions on the frame, a gold title band, a pink focus. The materials are
//! a desktop app's (ADR 26-11-07-363 §1): rounded vector frames, tab chips set into the frame's
//! top edge, icon buttons with tooltips, keycaps, rounded selections, the mark as vector art,
//! and spring motion. Nothing here types out the TUI's brackets, box drawing or blocks.

use rune::{Color, Cx, Drag, El, Icon, PointerPress, Spring, Transition, Tx, col, el, icon, row, spacer, text, text_input};

use crate::wire::{Box as Sandbox, GitTone, Link, Row, TabKind};
use crate::theme::{self, CELL_H};
use crate::paste::{self, DropOver};
use crate::{Focus, MenuKind, Pane, Side, State, Workspace, harnesses, icons, keys, mark, overlay, run_command, sheet};

pub const SIDEBAR_W: f32 = 290.0;
const ROW_H: f32 = 26.0;
/// A tab chip's height; half of it rides above the frame's top edge.
const CHIP_H: f32 = 22.0;

/// A state's glyph and colour, as the spec gives them.
pub fn glyph(s: &State, agent: bool, state: &str) -> (String, Color) {
    let table = if agent { &s.spec.agents } else { &s.spec.states };
    match table.get(state) {
        Some(g) => (g.glyph.clone(), if g.color.is_empty() { theme::text() } else { theme::token(&g.color) }),
        None => ((if agent { " " } else { "○" }).into(), theme::dim()),
    }
}

fn t(s: impl Into<String>, c: Color) -> El { text(s).font(theme::UI).color(c) }

fn source_label(b: &Sandbox) -> String { if b.source.is_empty() { "no source".into() } else { b.source.clone() } }

/// A shortcut as a keycap: mono, a 1 pt edge with a 2 pt bottom, 4 pt corners.
pub fn keycap(k: impl Into<String>) -> El {
    el().radius(theme::r_key()).fill(theme::line_bright()).pt(1.0).px(1.0).pb(2.0)
        .child(text(k).font(theme::UI_SMALL).color(theme::grey()).px(5.0).radius(theme::r_key() - 1.0).fill(theme::raised()))
}

/// The status line: a message, when there is one. Shortcuts are on the controls' tooltips, the
/// sidebar's and menus' keycaps and the palette's shortcut column (ADR 26-11-07-363 §1).
fn status_line(s: &State) -> El {
    match &s.message {
        Some(m) => t(m.clone(), theme::warn()).font(theme::UI_SMALL).truncate(),
        None => el(),
    }
}

/// A control drawn as its icon, with a tooltip naming what it does and its shortcut.
fn icon_button(key: impl Into<String>, glyph: &'static Icon, tip: Option<String>) -> El {
    let b = el().key(key.into()).size(24.0, 22.0).center().radius(theme::r_row()).fill(theme::raised())
        .hover(theme::text().alpha(0.08)).press(theme::text().alpha(0.16)).motion(theme::SNAPPY)
        .cursor(rune::Cursor::Pointer)
        .child(icon(glyph, 14.0).color(theme::grey()));
    match tip {
        Some(t) => b.help(t),
        None => b,
    }
}

/// A control's tooltip, from the registry: what its command does, and its shortcut where it has
/// one.
pub fn tip(s: &State, id: &str) -> Option<String> {
    let what = s.spec.command(id)?.detail.clone();
    Some(match keys::shortcut_of(s, id) {
        Some(k) => format!("{what}  {k}"),
        None => what,
    })
}

/// A rounded vector frame: the accent at 1.5 pt when focused, the line colour at 1 pt when not.
/// Its tab chips sit in its top edge on the left, its buttons on the right.
fn frame(key: impl Into<String>, focused: bool, title: Option<El>, buttons: Option<El>, body: El) -> El {
    let (w, c) = if focused { (theme::stroke_focus(), theme::mark()) } else { (theme::stroke_quiet(), theme::line()) };
    let mut outer = el().key(key.into()).w_full().h_full().pt(CHIP_H / 2.0).min_w(0.0).min_h(0.0)
        .child(col().w_full().h_full().radius(theme::r_frame()).fill(theme::raised()).border(w, c)
            .pt(CHIP_H / 2.0 + 6.0).pb(8.0).px(10.0).min_h(0.0)
            .motion(theme::SNAPPY)
            .child(body));
    if let Some(title) = title {
        outer = outer.child(row().absolute(14.0, 0.0).h(CHIP_H).gap(4.0).center_y().child(title));
    }
    if let Some(b) = buttons {
        outer = outer.child(row().pin(None, Some(0.0), Some(14.0), None).h(CHIP_H).gap(4.0).center_y().child(b));
    }
    outer
}

/// A tab chip in a frame's edge: the one shown in the frame's colour and bold, the others quiet,
/// each lit on hover. `state` says how its stream stands; `close` ends the session it names.
fn chip(key: impl Into<String>, label: &str, active: bool, focused: bool, state: Option<El>, close: Option<El>) -> El {
    let edge = match (active, focused) {
        (true, true) => theme::mark(),
        (true, false) => theme::line_bright(),
        _ => theme::line(),
    };
    let ink = if active { theme::text() } else { theme::grey() };
    let mut c = row().key(key.into()).h(CHIP_H).pl(10.0).pr(if close.is_some() { 4.0 } else { 10.0 }).gap(4.0).center_y()
        .radius(theme::r_key()).fill(theme::raised()).border(if active && focused { theme::stroke_focus() } else { theme::stroke_quiet() }, edge)
        .hover(theme::text().alpha(0.06)).press(theme::text().alpha(0.12)).motion(theme::SNAPPY)
        .child(text(label.to_string()).font(if active { theme::UI_BOLD } else { theme::UI }).color(ink).truncate());
    if let Some(st) = state {
        c = c.child(st);
    }
    if let Some(x) = close {
        c = c.child(x);
    }
    c
}

/// A chip's close control.
fn chip_close(key: impl Into<String>, tip: Option<String>) -> El {
    let b = el().key(key.into()).size(16.0, 16.0).center().radius(theme::r_key())
        .hover(theme::text().alpha(0.12)).press(theme::text().alpha(0.2)).cursor(rune::Cursor::Pointer)
        .child(icon(&icons::CLOSE, 10.0).color(theme::grey()));
    match tip {
        Some(t) => b.help(t),
        None => b,
    }
}

// ---------------------------------------------------------------------------------------------
// The list

/// A nesting level's width: the branch to a row's creator.
const BRANCH_W: f32 = 12.0;

/// The branches a nested row hangs from, drawn as strokes where the TUI types its tree: for each
/// level above its creator's, a rule where a branch carries on past it there; then its own
/// elbow, running on below when it is not its creator's last.
fn branches(r: &Row) -> El {
    let rule = |on: bool, full: bool| {
        let mut c = el().w(BRANCH_W).h(ROW_H);
        if on {
            c = c.child(el().w(theme::stroke_quiet()).h(if full { ROW_H } else { ROW_H / 2.0 }).fill(theme::dim()).absolute(BRANCH_W / 2.0 - 1.0, 0.0));
        }
        c
    };
    let mut out = row().h(ROW_H);
    for &on in &r.rules {
        out = out.child(rule(on, true));
    }
    out.child(rule(true, !r.last).child(el().w(BRANCH_W / 2.0).h(theme::stroke_quiet()).fill(theme::dim()).absolute(BRANCH_W / 2.0 - 1.0, ROW_H / 2.0)))
}

fn list_row(s: &State, i: usize, wide: bool) -> El {
    let r = &s.rows()[i];
    let Some(b) = s.box_by_id(&r.r#box) else { return el() };
    let on = i == s.cursor;
    let marked = s.is_selected(i);
    let live = s.live.as_deref() == Some(&b.key);
    let (dot, dot_c) = glyph(s, false, b.state.as_str());
    let (ag, ag_c) = glyph(s, true, &b.agent);
    // The cursor row says what the core has to say about the box in place of its name.
    let remark = r.remark.as_ref().filter(|_| on);
    let name = remark.map(|m| m.text.clone()).unwrap_or_else(|| b.name.clone());
    let lit = s.focus == Focus::List || s.live.is_none();
    // The cursor row is the selection; marked rows wear the accent; the box on screen keeps a
    // quieter fill when the keys are elsewhere.
    let fill = if on && lit {
        theme::selection()
    } else if marked {
        theme::mark().alpha(0.16)
    } else if live {
        theme::line()
    } else {
        Color::TRANSPARENT
    };
    let lead = if marked {
        icon(&icons::CHECK, 12.0).color(theme::mark())
    } else if on {
        icon(&icons::CHEVRON, 12.0).color(theme::gold())
    } else {
        el()
    };
    let mut row_el = row().key(format!("box-{}", b.key)).w_full().h(ROW_H).px(6.0).gap(8.0).center_y().radius(theme::r_row()).fill(fill)
        .hover(theme::text().alpha(0.05)).press(theme::text().alpha(0.1)).motion(theme::SNAPPY)
        .cursor(rune::Cursor::Pointer)
        .child(el().size(12.0, 12.0).child(lead))
        .child(t(dot, dot_c).w(12.0))
        .child(t(ag, ag_c).w(12.0));
    if r.depth > 0 {
        row_el = row_el.child(branches(r));
    }
    row_el = row_el.child(text(name).font(if on || live { theme::UI_BOLD } else { theme::UI }).color(if remark.is_some() { theme::warn() } else { theme::text() }).truncate().shrink());
    // A remark a command answers offers that command, by its own name and this app's key.
    if let Some(m) = remark.filter(|m| !m.command.is_empty()) {
        let (cmd, verb) = (m.command.clone(), if m.verb.is_empty() { m.command.clone() } else { m.verb.clone() });
        let mut offer = row().key(format!("remark-{}", b.key)).gap(4.0).center_y().px(4.0).radius(theme::r_key())
            .hover(theme::text().alpha(0.08)).cursor(rune::Cursor::Pointer)
            .child(text(verb).font(theme::UI_SMALL).color(theme::mark()));
        if let Some(k) = keys::shortcut_of(s, &cmd) {
            offer = offer.child(keycap(k));
        }
        row_el = row_el.child(offer.on_press_tx(move |s: &mut State, tx: &mut Tx| {
            s.cursor = i;
            run_command(s, &cmd, tx);
        }));
    }
    if !r.origin.is_empty() {
        row_el = row_el.child(t(r.origin.clone(), theme::dim()).font(theme::UI_SMALL));
    }
    if wide {
        for tag in &b.tags {
            row_el = row_el.child(tag_chip(tag));
        }
        let branch = if b.branch.is_empty() { "·".into() } else { format!("{}@{}", b.branch, b.commit) };
        row_el = row_el.child(spacer())
            .child(t(b.harness.clone(), theme::dim()).w(80.0))
            .child(t(branch, theme::grey()).w(190.0).truncate())
            .child(t(if b.dirty { "dirty" } else { "" }, theme::warn()).w(50.0))
            .child(t(if b.cpu > 0 { format!("{}%", b.cpu) } else { "·".into() }, theme::dim()).w(44.0))
            .child(t(if b.memory_bytes > 0 { format!("{:.1} GiB", b.memory_bytes as f64 / (1u64 << 30) as f64) } else { "·".into() }, theme::dim()).w(70.0))
            .child(diffstat(b).w(90.0));
    }
    let id = b.key.clone();
    row_el.on_press_tx(move |s: &mut State, tx: &mut Tx| s.press_row(i, tx.mods()))
    .on_double_press_tx(move |s: &mut State, tx: &mut Tx| {
        s.cursor = i;
        s.clear_selection();
        run_command(s, "attach", tx);
    })
    .on_secondary_press(move |s: &mut State, p: PointerPress, _tx: &mut Tx| {
        if !s.is_selected(i) {
            s.clear_selection();
        }
        s.cursor = i;
        s.open_menu(id.clone(), (p.x, p.y));
    })
}

/// A tag as a chip: outlined in its text colour, never filled.
fn tag_chip(tag: &str) -> El {
    text(tag.to_string()).font(theme::UI_SMALL).color(theme::dim()).px(5.0).radius(theme::r_key()).border(theme::stroke_quiet(), theme::dim())
}

fn diffstat(b: &Sandbox) -> El {
    if (b.added, b.deleted) == (0, 0) {
        return el();
    }
    row().gap(6.0)
        .child(text(format!("+{}", b.added)).font(theme::UI_SMALL).color(theme::done()))
        .child(text(format!("−{}", b.deleted)).font(theme::UI_SMALL).color(theme::salmon()))
}

/// The gold title band heading the list, or a section band grouping the rows beneath it.
fn band(label: &str, right: String, gold: bool) -> El {
    let (bg, fg) = if gold { (theme::gold(), theme::on_gold()) } else { (theme::line(), theme::grey()) };
    let font = if gold { theme::UI_BOLD } else { theme::UI };
    row().w_full().h(ROW_H).px(10.0).center_y().fill(bg).radius(theme::r_row())
        .child(text(label.to_string()).font(font).color(fg))
        .child(spacer())
        .child(text(right).font(font).color(fg))
}

fn plural(n: usize) -> String { format!("{n} box{}", if n == 1 { "" } else { "es" }) }

/// The filter: what the list is narrowed to, as the core says it, with a caret that says it
/// opens — the servers command's control. Its place is where the filter menu opens.
fn filter_control(s: &State) -> El {
    let mut c = row().key("filter").h(24.0).px(8.0).gap(6.0).center_y().radius(theme::r_row())
        .hover(theme::text().alpha(0.06)).press(theme::text().alpha(0.12)).motion(theme::SNAPPY).cursor(rune::Cursor::Pointer)
        .child(icon(&icons::FILTER, 12.0).color(theme::grey()))
        .child(t(s.listing.label.clone(), theme::text()).truncate().shrink())
        .child(icon(&icons::CARET, 12.0).color(theme::grey()))
        .on_layout(|s: &mut State, r: [f32; 4]| s.filter_at = (r[0], r[1] + r[3] + 4.0))
        .on_press_tx(|s: &mut State, tx: &mut Tx| run_command(s, "servers", tx));
    if let Some(tip) = tip(s, "servers") {
        c = c.help(tip);
    }
    c
}

/// A line under the rows that runs a command: the archived offer.
fn offer(s: &State, key: &str, label: String, id: &'static str) -> El {
    let mut r = row().key(key.to_string()).h(22.0).px(10.0).gap(6.0).center_y().radius(theme::r_row());
    if let Some(t) = tip(s, id) {
        r = r.help(t);
    }
    r
        .hover(theme::text().alpha(0.06)).cursor(rune::Cursor::Pointer)
        .child(t(label, theme::dim()).font(theme::UI_SMALL))
        .on_press_tx(move |s: &mut State, tx: &mut Tx| run_command(s, id, tx))
}

fn list_block(s: &State, wide: bool) -> El {
    let rows = s.rows();
    let marked = s.list_targets().len();
    let right = if s.range.is_some() || !s.selected.is_empty() { format!("{marked} selected · {}", plural(rows.len())) } else { plural(rows.len()) };
    let mut c = col().w_full().gap(2.0).child(band("Discoboxes", right, true))
        .child(row().w_full().gap(6.0).center_y().child(filter_control(s)).child(spacer()));
    if !s.loaded {
        let what = match &s.list_error {
            Some(e) => format!("could not list the discoboxes: {e}"),
            None if s.bridged => "listing…".into(),
            None => "no discoboxes here yet".into(),
        };
        return c.child(t(what, theme::dim()).padding(10.0).wrap());
    }
    if let Some(e) = &s.list_error {
        c = c.child(t(format!("could not list again: {e}"), theme::warn()).font(theme::UI_SMALL).px(10.0).truncate());
    }
    if rows.is_empty() {
        let none = if s.listing.narrowed { "no discoboxes match the filter" } else { "no discoboxes here yet" };
        c = c.child(t(none, theme::dim()).padding(10.0));
    }
    let mut section: Option<&str> = None;
    for i in 0..rows.len() {
        let r = &rows[i];
        if s.listing.grouped && section != Some(r.section.as_str()) {
            section = Some(r.section.as_str());
            let n = rows.iter().filter(|x| x.section == r.section).count();
            c = c.child(el().h(4.0)).child(band(&r.section_title, plural(n), false));
        }
        c = c.child(list_row(s, i, wide));
        if !wide && let Some(b) = s.box_by_id(&r.r#box) && !b.tags.is_empty() {
            c = c.child(row().pl(54.0).h(18.0).gap(6.0).center_y().children(b.tags.iter().map(|x| tag_chip(x))));
        }
    }
    for n in &s.listing.notes {
        c = c.child(row().h(22.0).px(10.0).gap(8.0).center_y()
            .child(t(n.title.clone(), theme::grey()).font(theme::UI_SMALL))
            .child(t(n.detail.clone(), theme::warn()).font(theme::UI_SMALL)));
    }
    if s.listing.filter.archived {
        c = c.child(offer(s, "archived-offer", "hide archived".into(), "archived"));
    } else if s.listing.archived > 0 {
        c = c.child(offer(s, "archived-offer", format!("{} archived · show them", s.listing.archived), "archived"));
    }
    c
}

// ---------------------------------------------------------------------------------------------
// The sidebar

/// A row that runs a command: its icon, what it does, and its keycap.
fn entry(s: &State, glyph: &'static Icon, label: &str, id: &'static str) -> El {
    let mut r = row().key(format!("entry-{id}")).w_full().h(28.0).px(8.0).gap(8.0).center_y().radius(theme::r_row())
        .hover(theme::text().alpha(0.06)).press(theme::text().alpha(0.12)).motion(theme::SNAPPY)
        .cursor(rune::Cursor::Pointer)
        .child(icon(glyph, 14.0).color(theme::grey()))
        .child(t(label.to_string(), theme::grey()).truncate().shrink())
        .child(spacer());
    if let Some(k) = keys::shortcut_of(s, id) {
        r = r.child(keycap(k));
    }
    if let Some(t) = tip(s, id) {
        r = r.help(t);
    }
    r.on_press_tx(move |s: &mut State, tx: &mut Tx| run_command(s, id, tx))
}

/// The mark and the product's name, where the TUI prints its own.
fn brand_line() -> El {
    row().h(22.0).px(4.0).gap(8.0).center_y().child(mark::view(16.0)).child(text("discobox").font(theme::UI_BOLD).color(theme::grey()))
}

fn sidebar(s: &State) -> El {
    let focused = s.focus == Focus::List;
    let body = col().w_full().h_full().gap(6.0).min_h(0.0)
        .child(list_block(s, false))
        .child(spacer())
        .child(row().w_full().center().child(mark::view(72.0)))
        .child(spacer())
        .child(el().h(1.0).w_full().fill(theme::line()))
        .child(entry(s, &icons::PLUS, "new discobox", "new"))
        .child(entry(s, &icons::SEARCH, "search", "palette"))
        .child(entry(s, &icons::BOX, "all discoboxes", "detach"));
    col().key("sidebar").w(SIDEBAR_W).h_full().gap(4.0)
        .transition(Transition::offset(-24.0, 0.0).combined(Transition::opacity()))
        .child(brand_line())
        .child(frame("sidebar-frame", focused, Some(chip("sidebar-title", "discoboxes", true, focused, None, None)),
            Some(icon_button("sidebar-hide", &icons::HIDE_SIDEBAR, tip(s, "sidebar"))
                .on_press_tx(|s: &mut State, tx: &mut Tx| run_command(s, "sidebar", tx))), body).grow(1.0))
}

// ---------------------------------------------------------------------------------------------
// The workspace

/// A tab's label: its number and its name. A service's name is what the core calls it, and its
/// number is its own counting's, the service gesture's (`S1` for the first): the services are
/// counted apart from the other tabs (the core's `Column.Number`).
fn pane_label(ws: &Workspace, i: usize, sbx: &Sandbox) -> String {
    let p = &ws.panes[i];
    if p.service() {
        let n = ws.services().iter().position(|&j| j == i).unwrap_or(0) + 1;
        return format!("S{n} {}", p.exec.title);
    }
    let name = match p.exec.kind {
        TabKind::Primary => sbx.name.clone(),
        _ => p.screen.title.clone().filter(|t| !t.is_empty() && t.len() < 24).unwrap_or_else(|| p.exec.title.clone()),
    };
    let n = ws.numbered().iter().position(|&j| j == i).unwrap_or(0) + 1;
    format!("{n} {name}")
}

/// What a tab's chip says of its stream beside its label: a gold dot while it reconnects, a
/// faint one once its program has finished and its last screen is held, or while its service is
/// not running.
fn chip_state(p: &Pane) -> Option<El> {
    let c = if p.settled() {
        theme::dim()
    } else if p.status.is_some() {
        theme::gold()
    } else {
        return None;
    };
    Some(el().size(6.0, 6.0).radius(3.0).fill(c))
}

fn column(s: &State, ws: &Workspace, sbx: &Sandbox, side: Side, two: bool) -> El {
    let idx = ws.column(side);
    let Some(active) = ws.active[side as usize] else { return el() };
    let focused = s.focus == Focus::Panes && ws.focus == side;
    let mut tabs = row().gap(4.0);
    for &i in &idx {
        let label = if two || idx.len() > 1 { pane_label(ws, i, sbx) } else { sbx.name.clone() };
        let p = &ws.panes[i];
        let stream = p.stream;
        // A held tab's close dismisses its last screen (end_focused); any other's ends its session.
        // The primary ends with its box, and a service is stopped from its menu: neither closes.
        let close = (!matches!(p.exec.kind, TabKind::Primary | TabKind::Service)).then(|| {
            chip_close(format!("tab-close-{stream}"), tip(s, "end")).on_press_tx(move |s: &mut State, tx: &mut Tx| {
                if let Some(i) = s.live_ws().and_then(|w| w.panes.iter().position(|p| p.stream == stream)) {
                    s.select_pane(i);
                    run_command(s, "end", tx);
                }
            })
        });
        let mut c = chip(format!("tab-{stream}"), &label, i == active, focused, chip_state(p), close)
            .cursor(if s.dragging == Some(stream) { rune::Cursor::Grabbing } else { rune::Cursor::Pointer })
            .on_layout(move |s: &mut State, r: [f32; 4]| {
                s.chips.insert(stream, r);
            })
            .on_drag(move |s: &mut State, d: &mut Drag, _tx: &mut Tx| s.drag_chip(stream, d))
            .on_press(move |s: &mut State| {
                if let Some(i) = s.live_ws().and_then(|w| w.panes.iter().position(|p| p.stream == stream)) {
                    s.select_pane(i);
                }
            })
            .on_double_press_tx(move |s: &mut State, tx: &mut Tx| run_command(s, "maximize", tx))
            .on_secondary_press(move |s: &mut State, p: PointerPress, _tx: &mut Tx| {
                if let Some(i) = s.live_ws().and_then(|w| w.panes.iter().position(|p| p.stream == stream)) {
                    s.select_pane(i);
                }
                s.open_commands_menu(MenuKind::Pane, (p.x, p.y));
            });
        if s.dragging == Some(stream) {
            // Lifted while it is dragged.
            c = c.fill(theme::line_bright());
        }
        tabs = tabs.child(c);
    }
    let mut buttons = row().gap(4.0);
    if two || ws.maximized {
        let glyph = if ws.maximized { &icons::RESTORE } else { &icons::MAXIMIZE };
        buttons = buttons.child(icon_button(format!("maximize-{side:?}"), glyph, tip(s, "maximize"))
            .on_press_tx(move |s: &mut State, tx: &mut Tx| {
                s.select_pane(active);
                run_command(s, "maximize", tx)
            }));
    }
    let pane = &ws.panes[active];
    let id = pane.stream;
    // Terminal content never moves on a spring: what a program draws lands as it is drawn, its
    // layout (`instant_changes`) and its colours (`Spring::NONE`; `instant_changes` leaves colours
    // on their springs) alike.
    let mut screen = el().key(format!("screen-{id}")).w_full().grow(1.0).min_h(0.0).clip_rounded(theme::r_key())
        .fill(theme::term_bg()).instant_changes().motion(Spring::NONE)
        .child(pane.screen.view(&format!("p{id}"), focused, &s.spec.link_schemes))
        .on_layout(move |s: &mut State, r: [f32; 4]| {
            s.resize_pane(id, (r[2] / theme::cell_w()).floor() as usize, (r[3] / CELL_H).floor() as usize)
        })
        .on_wheel(move |s: &mut State, w, _| {
            s.pane_wheel(id, &w);
            true
        })
        .pointer_buttons([rune::PointerButton::Middle])
        .on_drag(move |s: &mut State, d: &mut rune::Drag, tx: &mut Tx| s.pane_drag(id, active, d, tx))
        .on_pointer_move_local(move |s: &mut State, p, _| s.pane_hover(id, p.x, p.y))
        .on_hover_change(move |s: &mut State, inside, _| {
            if !inside && let Some(p) = s.pane_mut(id) {
                p.screen.hover = None;
            }
        })
        .on_secondary_press_local(move |s: &mut State, p, tx| s.pane_secondary(id, active, p, tx))
        // A program with the mouse is pointed at, not selected in.
        .cursor(if pane.screen.reports_mouse() { rune::Cursor::Default } else { rune::Cursor::Text });
    // Files dropped on one of the box's sessions: images carried in, anything else pasted as its
    // path (`paste.rs`); lit while a drag is over it. A service's tab takes nothing typed.
    // The border is there only under a drag: one drawn transparent would still cut a ring out of
    // the screen's ground.
    if s.takes_images(id) {
        if s.drop_over == Some(DropOver::Pane(id)) {
            screen = screen.border(theme::stroke_focus(), theme::mark());
        }
        screen = screen
            .on_drop_files(move |s: &mut State, files, _| s.drop_files(id, files))
            .on_drop_hover(move |s: &mut State, on, _| s.drop_hover(DropOver::Pane(id), on));
    }
    let mut chips = row().gap(6.0).pin(None, None, Some(6.0), Some(6.0));
    if pane.screen.seized && pane.screen.wants_mouse() {
        chips = chips.child(text("mouse taken").font(theme::UI_SMALL).color(theme::grey()).px(8.0).py(2.0).radius(theme::r_key())
            .fill(theme::raised()).border(theme::stroke_quiet(), theme::line()));
    }
    // What the tab says over its program: reconnecting in gold, what a held tab's ending left it
    // saying quietly.
    if let Some(st) = &pane.status {
        let (fill, ink) = if pane.settled() { (theme::line_bright(), theme::grey()) } else { (theme::gold(), theme::on_gold()) };
        chips = chips.child(el().key(format!("status-{id}")).px(8.0).py(2.0).radius(theme::r_key()).fill(fill)
            .child(text(st.clone()).font(theme::UI_SMALL).color(ink)));
    }
    screen = screen.child(chips);
    if let Some(bar) = find_bar(s, id) {
        screen = screen.child(bar);
    }
    frame(format!("col-{side:?}"), focused, Some(tabs), Some(buttons), screen).min_w(0.0)
}

/// How far down the screen the find bar sits: below the scrollback's "lines back" badge, which
/// the find moves and which says how far back a match is.
const FIND_TOP: f32 = 26.0;

/// The find through a pane's scrollback, over its top right while it is open: what is typed is
/// looked for as it is typed; Enter goes back to the match before, Shift-Enter on to the next,
/// Escape closes it.
fn find_bar(s: &State, stream: u32) -> Option<El> {
    let f = s.find.as_ref().filter(|_| s.find_stream() == Some(stream))?;
    let screen = &s.pane(stream)?.screen;
    let missing = screen.find.as_ref().is_some_and(|f| f.missing && !f.query.is_empty());
    let field = text_input(&f.query, theme::UI.lh(20.0)).key("find-query").single_line().w(220.0).color(theme::text())
        .placeholder("find in the scrollback", theme::dim())
        .on_edit(|s: &mut State| &mut s.find.as_mut().unwrap().query)
        .on_input_change(|s: &mut State, _| s.find_changed())
        .on_input_key(|s: &mut State, ev: &rune::KeyEvent, _tx: &mut Tx| {
            match ev.key {
                rune::Key::Enter => s.find_next(!ev.mods.shift),
                rune::Key::Escape => s.close_find(),
                _ => return false,
            }
            true
        });
    let step = |key: &str, glyph: &'static Icon, what: &str, back: bool| {
        icon_button(format!("find-{key}"), glyph, Some(what.into())).on_press(move |s: &mut State| s.find_next(back))
    };
    Some(row().key("find-bar").pin(None, Some(FIND_TOP), Some(8.0), None).h(32.0).px(8.0).gap(6.0).center_y()
        .radius(theme::r_row()).fill(theme::raised()).border(theme::stroke_focus(), if missing { theme::err() } else { theme::line_bright() })
        .shadow(8.0, 24.0, Color::BLACK.alpha(0.4))
        .child(icon(&icons::SEARCH, 14.0).color(theme::grey()))
        .child(field)
        .child(t(if missing { "no match" } else { "" }, theme::err()).font(theme::UI_SMALL))
        .child(step("back", &icons::UP, "the match before", true))
        .child(step("on", &icons::CARET, "the next match", false))
        .child(icon_button("find-close", &icons::CLOSE, Some("close".into())).on_press(|s: &mut State| s.close_find())))
}

/// The divider between the two columns: dragged, it moves where they split; a double press puts
/// them back half and half.
fn divider() -> El {
    el().key("split").w(10.0).h_full().radius(theme::r_key()).cursor(rune::Cursor::ResizeColumn)
        .hover(theme::text().alpha(0.06)).press(theme::mark().alpha(0.16))
        .on_drag(|s: &mut State, d: &mut Drag, _tx: &mut Tx| s.drag_split(d))
        .on_double_press(|s: &mut State| {
            if let Some(ws) = s.live_ws_mut() {
                ws.split = 0.5;
            }
        })
}

/// A link in the header: opened in the browser on a press; one without a URL is text.
fn link(key: String, l: &Link) -> El {
    if l.url.is_empty() {
        return t(l.label.clone(), theme::grey());
    }
    let url = l.url.clone();
    text(l.label.clone()).key(key).font(theme::UI).color(theme::mark()).px(2.0).radius(theme::r_key())
        .hover(theme::mark().alpha(0.12)).press(theme::mark().alpha(0.2)).cursor(rune::Cursor::Pointer).help(url.clone())
        .on_press_tx(move |s: &mut State, tx: &mut Tx| {
            if let Err(e) = tx.open_url(&url) {
                s.message = Some(format!("could not open {url}: {e}"));
            }
        })
}

/// What the workspace's header says of its box: where it came from, then the core's header —
/// its id, the issue and pull request it is for, where its work sits in git, what it changed,
/// what it serves (a forwarded web port is a link) and the rest of its tags — and what it uses.
fn header(s: &State, ws: &Workspace, sbx: &Sandbox) -> El {
    let mut r = row().w_full().h(24.0).px(12.0).gap(14.0).center_y();
    if !s.sidebar {
        r = r.child(icon_button("sidebar-show", &icons::SHOW_SIDEBAR, tip(s, "sidebar"))
            .on_press_tx(|s: &mut State, tx: &mut Tx| run_command(s, "sidebar", tx)));
    }
    // The git position and word in their tone: uncommitted work warns, committed work ahead
    // reads as the name does, anything else is quiet.
    let tone = |g: GitTone| match g {
        GitTone::Dirty => theme::warn(),
        GitTone::Ahead => theme::text(),
        GitTone::Quiet => theme::dim(),
    };
    // What does not fit is cut at the right, where the readouts start: the ports and tags,
    // which have no bound on their width, come last.
    let mut mid = row().h(24.0).gap(14.0).center_y().grow(1.0).min_w(0.0).clip_rounded(0.0);
    // What its agent is doing, as the core says it last said: in its own words while it says any.
    if let Some(h) = ws.header.as_ref().filter(|h| !h.agent.is_empty()) {
        let (g, c) = glyph(s, true, &h.agent);
        let said = if h.agent_msg.is_empty() { h.agent.clone() } else { h.agent_msg.clone() };
        mid = mid.child(row().key("agent").gap(6.0).center_y().min_w(0.0).child(t(g, c)).child(t(said, c).max_w(420.0).truncate()));
    }
    mid = mid.child(t(source_label(sbx), theme::grey()));
    match &ws.header {
        None => mid = mid.child(t(sbx.id.clone(), theme::dim())),
        Some(h) => {
            mid = mid.child(t(h.id.clone(), theme::dim()));
            for (i, w) in h.work.iter().enumerate() {
                mid = mid.child(link(format!("work-{i}"), w));
            }
            if !h.blocked.is_empty() {
                mid = mid.child(t(h.blocked.clone(), theme::err()));
            }
            if !h.base.is_empty() {
                mid = mid.child(t(h.base.clone(), tone(h.tone)));
            }
            if !h.changes.is_empty() {
                mid = mid.child(t(h.changes.clone(), if h.ready { theme::done() } else { tone(h.tone) }));
            }
            if h.diff {
                mid = mid.child(row().gap(6.0)
                    .child(text(format!("+{}", h.added)).font(theme::UI_SMALL).color(theme::done()))
                    .child(text(format!("−{}", h.deleted)).font(theme::UI_SMALL).color(theme::salmon())));
            }
            if let Some(d) = &h.desktop {
                mid = mid.child(link("desktop".into(), d));
            }
            if !h.ports.is_empty() {
                let mut ports = row().gap(4.0).center_y();
                if !h.unforwarded.is_empty() {
                    ports = ports.help(format!("not forwarded: {}", h.unforwarded));
                }
                for (g, group) in h.ports.iter().enumerate() {
                    ports = ports.child(t(format!("{}:", group.protocol), theme::dim()));
                    for (i, p) in group.ports.iter().enumerate() {
                        ports = ports.child(link(format!("port-{g}-{i}"), p));
                    }
                }
                mid = mid.child(ports);
            }
            for tag in &h.tags {
                mid = mid.child(tag_chip(tag));
            }
        }
    }
    let id = sbx.key.clone();
    r.child(mid)
        .child(t(format!("cpu {}%  mem {:.1} GiB", sbx.cpu, sbx.memory_bytes as f64 / (1u64 << 30) as f64), theme::dim()))
        .child(icon_button("box-actions", &icons::MORE, tip(s, "actions"))
            .on_press(move |s: &mut State| s.open_menu(id.clone(), (900.0, 30.0))))
}

fn workspace(s: &State, ws: &Workspace) -> El {
    let Some(sbx) = s.box_by_id(&ws.sandbox) else { return el() };
    let two = !ws.column(Side::Shells).is_empty() && !ws.column(Side::Terminals).is_empty();
    let mut cols = row().key("columns").w_full().grow(1.0).min_h(0.0)
        .on_layout(|s: &mut State, r: [f32; 4]| s.columns_w = r[2]);
    if let Some(e) = &ws.error {
        let enter = keys::shortcut_of(s, "attach").unwrap_or_else(|| "Enter".into());
        cols = cols.child(frame("failed", true, Some(chip("failed-title", &sbx.name, true, true, None, None)), None,
            col().w_full().h_full().center().gap(8.0)
                .child(t("could not open its sessions", theme::warn()))
                .child(text(e.clone()).font(theme::UI_SMALL).color(theme::grey()).max_w(640.0).wrap())
                .child(row().gap(6.0).center_y().child(keycap(enter)).child(t("tries again", theme::dim())))));
    } else if ws.opening {
        let doing = if ws.step.is_empty() { "attaching…".to_string() } else { format!("{}…", ws.step) };
        cols = cols.child(frame("opening", true, Some(chip("opening-title", &sbx.name, true, true, None, None)), None,
            col().w_full().h_full().center().child(t(doing, theme::dim()))));
    } else if ws.maximized {
        cols = cols.child(column(s, ws, sbx, ws.focus, two).grow(1.0));
    } else if two {
        cols = cols.child(column(s, ws, sbx, Side::Terminals, two).grow(ws.split))
            .child(divider())
            .child(column(s, ws, sbx, Side::Shells, two).grow(1.0 - ws.split));
    } else {
        cols = cols.child(column(s, ws, sbx, Side::Terminals, two).grow(1.0));
    }
    // What a box on its way up is being made to do, while it says so.
    let step = (!ws.opening && !ws.step.is_empty()).then(|| row().key("provisioning").w_full().h(20.0).px(12.0).gap(8.0).center_y()
        .child(t("provisioning", theme::gold()).font(theme::UI_SMALL))
        .child(t(ws.step.clone(), theme::grey()).font(theme::UI_SMALL).truncate().shrink()));
    col().key("workspace").grow(1.0).h_full().min_w(0.0).gap(4.0).motion(theme::SNAPPY)
        .child(header(s, ws, sbx))
        .children(step)
        .child(cols)
        .child(row().w_full().h(24.0).px(12.0).center_y().min_w(0.0)
            .child(status_line(s)))
}

// ---------------------------------------------------------------------------------------------
// Every box

fn home(s: &State) -> El {
    let header = row().w_full().h(26.0).gap(16.0).center_y()
        .child(brand_line())
        .child(t("all discoboxes", theme::grey()))
        .child(spacer())
        .child(t(if s.bridged { "" } else { "mock data" }, theme::warn()).font(theme::UI_SMALL));
    let body = col().w_full().h_full().gap(10.0).px(8.0).min_h(0.0)
        .child(header)
        .child(list_block(s, true))
        .child(spacer())
        .child(composer(s))
        .child(row().w_full().h(24.0).px(18.0).center_y().min_w(0.0).child(status_line(s)));
    frame("home", true, None, None, body)
}

/// The composer: what the new discobox should do. Return creates it — through the create card,
/// as the new command does — and Shift+Return is a new line; an empty prompt creates a box with
/// nothing to do. Under it, what the create would be made with (the core's summary of the run
/// options), lit where somebody chose it, and the run options to change it.
fn composer(s: &State) -> El {
    let field = text_input(&s.composer, theme::UI.lh(20.0)).key("composer").grow(1.0).color(theme::text())
        .placeholder("What should the new discobox do?", theme::dim())
        .on_edit(|s: &mut State| &mut s.composer)
        .on_input_change(|s: &mut State, _| s.composer_changed())
        .on_submit(|s: &mut State, tx: &mut Tx| run_command(s, "new", tx));
    let mut summary = row().key("composer-summary").w_full().gap(6.0).center_y().min_w(0.0).flex_wrap();
    for (i, c) in s.summary.iter().enumerate() {
        if i > 0 {
            summary = summary.child(t("·", theme::dim()).font(theme::UI_SMALL));
        }
        summary = summary.child(t(c.text.clone(), if c.chosen { theme::gold() } else { theme::dim() }).font(theme::UI_SMALL));
    }
    summary = summary.child(spacer()).child(offer(s, "options-offer", "run options".into(), "options"));
    // A folder dropped on it is where the next discobox is cut from (`paste.rs`); lit while a
    // drag carrying one is over it.
    let over = s.drop_over == Some(DropOver::Composer);
    col().w_full().gap(6.0).px(18.0)
        .child(text("Return runs the prompt in a new discobox, or just creates one when it is empty · Shift+Return is a new line · drop a folder to cut it from there")
            .font(theme::UI_SMALL).color(theme::dim()))
        .child(row().key("composer-box").w_full().px(12.0).py(10.0).gap(10.0).items_start().radius(theme::r_row()).fill(theme::sunken())
            .border(if over { theme::stroke_focus() } else { theme::stroke_quiet() }, if s.composer.focused || over { theme::mark() } else { theme::line() })
            .on_drop_files(|s: &mut State, folders, _| s.drop_source(folders))
            .accepts_files(paste::is_folder)
            .on_drop_hover(|s: &mut State, on, _| s.drop_hover(DropOver::Composer, on))
            .child(icon(&icons::CHEVRON, 14.0).color(theme::mark()).mt(3.0))
            .child(rune::auto_grow(field, 1, 8).grow(1.0)))
        .child(summary)
}

pub fn view(s: &State, cx: &mut Cx<State>) -> El {
    // The system's appearance, or the one `--theme` picks.
    theme::set_dark(cx.system_dark());
    let mut root = row().w_full().padding(10.0).gap(10.0).fill(theme::bg());
    match s.live_ws() {
        // A page opens beside the list, in place of whatever was on screen.
        _ if s.harnesses.is_some() => {
            if s.sidebar {
                root = root.child(sidebar(s));
            }
            root = root.child(harnesses::page_view(s));
        }
        Some(ws) => {
            if s.sidebar && !ws.maximized {
                root = root.child(sidebar(s));
            }
            root = root.child(workspace(s, ws));
        }
        None => root = root.child(home(s)),
    }
    // Off macOS the menu bar is the window's own, above everything else; on macOS it is the
    // system's.
    let root = if keys::MAC {
        root.h_full()
    } else {
        col().w_full().h_full().fill(theme::bg()).child(overlay::menu_bar(s).px(10.0).pt(4.0)).child(root.pt(4.0).grow(1.0).min_h(0.0))
    };
    let mut stack = el().w_full().h_full().child(root);
    if let Some(m) = overlay::menu_view(s) {
        stack = stack.child(m);
    }
    if let Some(f) = overlay::filter_view(s) {
        stack = stack.child(f);
    }
    if let Some(p) = overlay::palette_view(s) {
        stack = stack.child(p);
    }
    if let Some(h) = overlay::help_view(s) {
        stack = stack.child(h);
    }
    if let Some(c) = harnesses::card_view(s) {
        stack = stack.child(c);
    }
    if let Some(r) = harnesses::review_view(s) {
        stack = stack.child(r);
    }
    if let Some(sh) = sheet::view(s) {
        stack = stack.child(sh);
    }
    stack
}
