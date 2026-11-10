//! Drawing: the list of every box, and a box's workspace — the sidebar of boxes down the left
//! and the box's terminals in two tabbed columns right of it. The structure is the TUI's: framed
//! columns with their sessions on the frame, a gold title band, a pink focus. The materials are
//! a desktop app's (ADR 26-11-07-363 §1): rounded vector frames, tab chips set into the frame's
//! top edge, icon buttons with tooltips, keycaps, rounded selections, the mark as vector art,
//! and spring motion. Nothing here types out the TUI's brackets, box drawing or blocks.

use rune::{Color, Cx, El, Icon, PointerPress, Spring, Transition, Tx, col, el, icon, row, spacer, text, text_input};

use crate::wire::{Box as Sandbox, TabKind};
use crate::theme::{self, CELL_H};
use crate::{Focus, Side, State, Workspace, icons, keys, mark, overlay, run_command};

pub const SIDEBAR_W: f32 = 290.0;
const ROW_H: f32 = 26.0;
/// A tab chip's height; half of it rides above the frame's top edge.
const CHIP_H: f32 = 22.0;

/// Sections in first-seen order, and a box nested under the box that created it.
pub fn list_order(boxes: Vec<Sandbox>) -> Vec<Sandbox> {
    let mut sources: Vec<String> = vec![];
    for b in &boxes {
        if !sources.contains(&b.source) {
            sources.push(b.source.clone());
        }
    }
    let ids: Vec<&str> = boxes.iter().map(|b| b.key.as_str()).collect();
    let nested = |b: &Sandbox| !b.parent.is_empty() && ids.contains(&b.parent.as_str());
    let mut out = vec![];
    for src in sources {
        for b in boxes.iter().filter(|b| b.source == src && !nested(b)) {
            out.push(b.clone());
            out.extend(boxes.iter().filter(|c| c.parent == b.key).cloned());
        }
    }
    out
}

/// A state's glyph and colour, as the spec gives them.
fn glyph(s: &State, agent: bool, state: &str) -> (String, Color) {
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

/// What a control does, and its shortcut where it has one.
fn tip(s: &State, what: &str, id: &str) -> Option<String> {
    Some(match keys::shortcut_of(s, id) {
        Some(k) => format!("{what}  {k}"),
        None => what.to_string(),
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
/// each lit on hover. `close` ends the session it names.
fn chip(key: impl Into<String>, label: &str, active: bool, focused: bool, close: Option<El>) -> El {
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

/// What the cursor row says in place of a box's name, as the TUI's row does.
fn note(b: &Sandbox) -> Option<&str> {
    [&b.provisioning, &b.message].into_iter().find(|s| !s.is_empty()).map(|s| s.as_str())
}

fn list_row(s: &State, i: usize, wide: bool) -> El {
    let b = &s.boxes[i];
    let on = i == s.cursor;
    let live = s.live.as_deref() == Some(&b.key);
    let (dot, dot_c) = glyph(s, false, b.state.as_str());
    let (ag, ag_c) = glyph(s, true, &b.agent);
    let nested = !b.parent.is_empty() && s.box_by_id(&b.parent).is_some();
    let name = match (note(b), on) {
        (Some(m), true) => m.to_string(),
        _ => b.name.clone(),
    };
    let lit = s.focus == Focus::List || s.live.is_none();
    // The cursor row is the selection; the box on screen keeps a quieter one when the keys are
    // elsewhere.
    let fill = if on && lit { theme::selection() } else if live { theme::line() } else { Color::TRANSPARENT };
    let mut r = row().key(format!("box-{}", b.key)).w_full().h(ROW_H).px(6.0).gap(8.0).center_y().radius(theme::r_row()).fill(fill)
        .hover(theme::text().alpha(0.05)).press(theme::text().alpha(0.1)).motion(theme::SNAPPY)
        .cursor(rune::Cursor::Pointer)
        .child(el().size(12.0, 12.0).child(if on { icon(&icons::CHEVRON, 12.0).color(theme::gold()) } else { el() }))
        .child(t(dot, dot_c).w(12.0))
        .child(t(ag, ag_c).w(12.0));
    if nested {
        r = r.child(icon(&icons::NESTED, 12.0).color(theme::dim()));
    }
    r = r.child(text(name).font(if on || live { theme::UI_BOLD } else { theme::UI }).color(theme::text()).truncate().shrink());
    if wide {
        for tag in &b.tags {
            r = r.child(tag_chip(tag));
        }
        let branch = if b.branch.is_empty() { "·".into() } else { format!("{}@{}", b.branch, b.commit) };
        r = r.child(spacer())
            .child(t(b.harness.clone(), theme::dim()).w(80.0))
            .child(t(branch, theme::grey()).w(190.0).truncate())
            .child(t(if b.dirty { "dirty" } else { "" }, theme::warn()).w(50.0))
            .child(t(if b.cpu > 0 { format!("{}%", b.cpu) } else { "·".into() }, theme::dim()).w(44.0))
            .child(t(if b.memory_bytes > 0 { format!("{:.1} GiB", b.memory_bytes as f64 / (1u64 << 30) as f64) } else { "·".into() }, theme::dim()).w(70.0))
            .child(diffstat(b).w(90.0));
    }
    let id = b.key.clone();
    r.on_press(move |s: &mut State| {
        s.cursor = i;
        s.focus = Focus::List;
        // A press on a running box beside a workspace switches to it, as the TUI's tree does.
        if s.live.is_some() && s.boxes[i].running() {
            s.open_box(i);
        }
    })
    .on_double_press_tx(move |s: &mut State, tx: &mut Tx| {
        s.cursor = i;
        run_command(s, "attach", tx);
    })
    .on_secondary_press(move |s: &mut State, p: PointerPress, _tx: &mut Tx| {
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

fn list_block(s: &State, wide: bool) -> El {
    let mut c = col().w_full().gap(2.0).child(band("Discoboxes", plural(s.boxes.len()), true));
    if s.boxes.is_empty() {
        return c.child(t(if s.bridged { "listing…" } else { "no discoboxes here yet" }, theme::dim()).padding(10.0));
    }
    let mut src: Option<String> = None;
    for i in 0..s.boxes.len() {
        let b = &s.boxes[i];
        if src.as_deref() != Some(&b.source) {
            src = Some(b.source.clone());
            let n = s.boxes.iter().filter(|x| x.source == b.source).count();
            c = c.child(el().h(4.0)).child(band(&source_label(b), plural(n), false));
        }
        c = c.child(list_row(s, i, wide));
        if !wide && !b.tags.is_empty() {
            c = c.child(row().pl(54.0).h(18.0).gap(6.0).center_y().children(b.tags.iter().map(|x| tag_chip(x))));
        }
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
        .child(frame("sidebar-frame", focused, Some(chip("sidebar-title", "discoboxes", true, focused, None)),
            Some(icon_button("sidebar-hide", &icons::HIDE_SIDEBAR, tip(s, "hide the sidebar", "sidebar"))
                .on_press_tx(|s: &mut State, tx: &mut Tx| run_command(s, "sidebar", tx))), body).grow(1.0))
}

// ---------------------------------------------------------------------------------------------
// The workspace

fn pane_label(ws: &Workspace, i: usize, sbx: &Sandbox) -> String {
    let p = &ws.panes[i];
    let n = ws.strip().iter().position(|&j| j == i).unwrap_or(0) + 1;
    let name = match p.exec.kind {
        TabKind::Primary => sbx.name.clone(),
        _ => p.screen.title.clone().filter(|t| !t.is_empty() && t.len() < 24).unwrap_or_else(|| p.exec.title.clone()),
    };
    format!("{n} {name}{}", if p.exited { " · done" } else { "" })
}

fn column(s: &State, ws: &Workspace, sbx: &Sandbox, side: Side, two: bool) -> El {
    let idx = ws.column(side);
    let Some(active) = ws.active[side as usize] else { return el() };
    let focused = s.focus == Focus::Panes && ws.focus == side;
    let mut tabs = row().gap(4.0);
    for &i in &idx {
        let label = if two || idx.len() > 1 { pane_label(ws, i, sbx) } else { sbx.name.clone() };
        let stream = ws.panes[i].stream;
        let close = (ws.panes[i].exec.kind != TabKind::Primary).then(|| {
            chip_close(format!("tab-close-{stream}"), tip(s, "end this session", "end")).on_press_tx(move |s: &mut State, tx: &mut Tx| {
                s.select_pane(i);
                run_command(s, "end", tx);
            })
        });
        tabs = tabs.child(chip(format!("tab-{stream}"), &label, i == active, focused, close).cursor(rune::Cursor::Pointer)
            .on_press(move |s: &mut State| s.select_pane(i))
            .on_double_press_tx(move |s: &mut State, tx: &mut Tx| run_command(s, "maximize", tx)));
    }
    let mut buttons = row().gap(4.0);
    if two || ws.maximized {
        let (glyph, what) = if ws.maximized { (&icons::RESTORE, "restore") } else { (&icons::MAXIMIZE, "maximize") };
        buttons = buttons.child(icon_button(format!("maximize-{side:?}"), glyph, tip(s, what, "maximize"))
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
        .child(pane.screen.view(&format!("p{id}"), focused))
        .on_layout(move |s: &mut State, r: [f32; 4]| {
            s.resize_pane(id, (r[2] / theme::cell_w()).floor() as usize, (r[3] / CELL_H).floor() as usize)
        })
        .on_wheel(move |s: &mut State, w, _| {
            s.scroll_pane(id, (w.dy / CELL_H).round() as i32);
            true
        })
        .on_press(move |s: &mut State| s.select_pane(active))
        .cursor(rune::Cursor::Text);
    if let Some(st) = &pane.status {
        screen = screen.child(text(st.clone()).font(theme::UI_SMALL).color(theme::on_gold()).px(8.0).py(2.0).radius(theme::r_key())
            .fill(if pane.exited { theme::err() } else { theme::gold() }).pin(None, None, Some(6.0), Some(6.0)));
    }
    frame(format!("col-{side:?}"), focused, Some(tabs), Some(buttons), screen).grow(1.0).min_w(0.0)
}

fn header(s: &State, sbx: &Sandbox) -> El {
    let mut r = row().w_full().h(24.0).px(12.0).gap(18.0).center_y();
    if !s.sidebar {
        r = r.child(icon_button("sidebar-show", &icons::SHOW_SIDEBAR, tip(s, "show the sidebar", "sidebar"))
            .on_press_tx(|s: &mut State, tx: &mut Tx| run_command(s, "sidebar", tx)));
    }
    r = r.child(t(source_label(sbx), theme::grey()))
        .child(t(sbx.id.clone(), theme::dim()));
    if !sbx.branch.is_empty() {
        r = r.child(t(format!("{}@{}", sbx.branch, sbx.commit), theme::grey()));
    }
    if sbx.dirty {
        r = r.child(t("dirty", theme::warn()));
    }
    if (sbx.added, sbx.deleted) != (0, 0) {
        r = r.child(diffstat(sbx));
    }
    for tag in &sbx.tags {
        r = r.child(tag_chip(tag));
    }
    let id = sbx.key.clone();
    r.child(spacer())
        .child(t(format!("cpu {}%  mem {:.1} GiB", sbx.cpu, sbx.memory_bytes as f64 / (1u64 << 30) as f64), theme::dim()))
        .child(icon_button("box-actions", &icons::MORE, Some("box actions".into()))
            .on_press(move |s: &mut State| s.open_menu(id.clone(), (900.0, 30.0))))
}

fn workspace(s: &State, ws: &Workspace) -> El {
    let Some(sbx) = s.box_by_id(&ws.sandbox) else { return el() };
    let two = !ws.column(Side::Shells).is_empty() && !ws.column(Side::Terminals).is_empty();
    let mut cols = row().w_full().grow(1.0).gap(10.0).min_h(0.0);
    if let Some(e) = &ws.error {
        let enter = keys::shortcut_of(s, "attach").unwrap_or_else(|| "Enter".into());
        cols = cols.child(frame("failed", true, Some(chip("failed-title", &sbx.name, true, true, None)), None,
            col().w_full().h_full().center().gap(8.0)
                .child(t("could not open its sessions", theme::warn()))
                .child(text(e.clone()).font(theme::UI_SMALL).color(theme::grey()).max_w(640.0).wrap())
                .child(row().gap(6.0).center_y().child(keycap(enter)).child(t("tries again", theme::dim())))));
    } else if ws.opening {
        cols = cols.child(frame("opening", true, Some(chip("opening-title", &sbx.name, true, true, None)), None,
            col().w_full().h_full().center().child(t("attaching…", theme::dim()))));
    } else if ws.maximized {
        cols = cols.child(column(s, ws, sbx, ws.focus, two));
    } else {
        cols = cols.child(column(s, ws, sbx, Side::Terminals, two));
        if two {
            cols = cols.child(column(s, ws, sbx, Side::Shells, two));
        }
    }
    col().key("workspace").grow(1.0).h_full().min_w(0.0).gap(4.0).motion(theme::SNAPPY)
        .child(header(s, sbx))
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
    let composer = col().w_full().gap(8.0).px(18.0)
        .child(text("Enter runs the prompt in a new discobox, or just creates one when it is empty").font(theme::UI_SMALL).color(theme::dim()))
        .child(row().w_full().h(40.0).px(12.0).gap(10.0).center_y().radius(theme::r_row()).fill(theme::sunken()).border(theme::stroke_quiet(), theme::line())
            .child(icon(&icons::CHEVRON, 14.0).color(theme::mark()))
            .child(text_input(&TEXT_EDIT.with(|e| e.clone()), theme::UI.lh(20.0)).key("composer").single_line().grow(1.0).color(theme::text())
                .placeholder("What should the new discobox do?", theme::dim())
                .on_submit(|s: &mut State, tx: &mut Tx| run_command(s, "new", tx))));
    let body = col().w_full().h_full().gap(10.0).px(8.0).min_h(0.0)
        .child(header)
        .child(list_block(s, true))
        .child(spacer())
        .child(composer)
        .child(row().w_full().h(24.0).px(18.0).center_y().min_w(0.0).child(status_line(s)));
    frame("home", true, None, None, body)
}

thread_local! {
    static TEXT_EDIT: rune::TextEdit = rune::TextEdit::new("");
}

pub fn view(s: &State, cx: &mut Cx<State>) -> El {
    // The system's appearance, or the one `--theme` picks.
    theme::set_dark(cx.system_dark());
    let mut root = row().w_full().h_full().padding(10.0).gap(10.0).fill(theme::bg());
    match s.live_ws() {
        Some(ws) => {
            if s.sidebar && !ws.maximized {
                root = root.child(sidebar(s));
            }
            root = root.child(workspace(s, ws));
        }
        None => root = root.child(home(s)),
    }
    let mut stack = el().w_full().h_full().child(root);
    if let Some(m) = overlay::menu_view(s) {
        stack = stack.child(m);
    }
    if let Some(p) = overlay::palette_view(s) {
        stack = stack.child(p);
    }
    stack
}
