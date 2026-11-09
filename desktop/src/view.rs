//! Drawing: the list of every box, and a box's workspace — the sidebar of boxes down the left
//! and the box's terminals in two tabbed columns right of it. The shapes are the TUI's:
//! rounded frames whose tabs ride the top border, `[+][x]` buttons, a header row above. Where
//! the TUI has a hint line of leader keys, this has the desktop's own shortcuts.

use rune::{Color, Cx, El, PointerPress, Span, Tx, col, el, rich, row, spacer, text, text_input};

use crate::wire::{Box as Sandbox, TabKind};
use crate::theme::{self, CELL_H};
use crate::{Focus, Side, State, Workspace, keys, logo, overlay, run_command};

pub const SIDEBAR_W: f32 = 290.0;
const ROW_H: f32 = 26.0;
const TITLE_H: f32 = 20.0;

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
        Some(g) => (g.glyph.clone(), theme::token(&g.color, 0xb0a0bf)),
        None => ((if agent { " " } else { "○" }).into(), theme::dim()),
    }
}

fn t(s: impl Into<String>, c: Color) -> El { text(s).font(theme::UI).color(c) }

fn source_label(b: &Sandbox) -> String { if b.source.is_empty() { "no source".into() } else { b.source.clone() } }

/// `label shortcut · label shortcut`: the commands that matter here, with their keys.
fn shortcut_hints(s: &State, ids: &[&str]) -> El {
    let mut out = String::new();
    let mut spans = vec![];
    for id in ids {
        let (Some(label), Some(k)) = (keys::label_of(&s.spec, id), keys::shortcut_of(s, id)) else { continue };
        if !out.is_empty() {
            let a = out.len();
            out.push_str("   ·   ");
            spans.push(Span::new(a, out.len()).color(theme::inactive()));
        }
        let a = out.len();
        out.push_str(&label.to_lowercase());
        spans.push(Span::new(a, out.len()).color(theme::dim()));
        let a = out.len();
        out.push(' ');
        out.push_str(&k);
        spans.push(Span::new(a, out.len()).color(theme::grey()));
    }
    rich(out, spans).font(theme::UI_SMALL).truncate()
}

fn status_line(s: &State, ids: &[&str]) -> El {
    match &s.message {
        Some(m) => t(m.clone(), theme::warn()).font(theme::UI_SMALL).truncate(),
        None => shortcut_hints(s, ids),
    }
}

/// A rounded frame whose title and buttons ride its top border, as the TUI draws a box.
fn frame(key: impl Into<String>, focused: bool, title: Option<El>, buttons: Option<El>, body: El) -> El {
    let c = if focused { theme::mark() } else { theme::inactive() };
    let mut outer = el().key(key.into()).w_full().h_full().pt(TITLE_H / 2.0).min_w(0.0).min_h(0.0)
        .child(col().w_full().h_full().radius(10.0).border(1.0, c).pt(TITLE_H / 2.0 + 4.0).pb(8.0).px(10.0).min_h(0.0)
            .motion(rune::tok::motion::SNAPPY)
            .child(body));
    if let Some(title) = title {
        outer = outer.child(row().absolute(18.0, 0.0).h(TITLE_H).px(4.0).gap(2.0).center_y().fill(theme::bg()).child(title));
    }
    if let Some(b) = buttons {
        outer = outer.child(row().pin(None, Some(0.0), Some(18.0), None).h(TITLE_H).px(4.0).center_y().fill(theme::bg()).child(b));
    }
    outer
}

/// `[ label ]` in the frame's colour, the label bold when it is the one shown.
fn tab(label: &str, active: bool, border: Color) -> El {
    let s = format!("[ {label} ]");
    let n = s.len();
    rich(s, vec![
        Span::new(0, 2).color(border),
        Span::new(2, n - 2).color(if active { theme::text() } else { theme::dim() }).weight(if active { 600.0 } else { 400.0 }),
        Span::new(n - 2, n).color(border),
    ]).font(theme::UI)
}

fn button(glyph: &str, border: Color, tip: Option<String>) -> El {
    let b = rich(format!("[{glyph}]"), vec![
        Span::new(0, 1).color(border),
        Span::new(1, 1 + glyph.len()).color(theme::grey()),
        Span::new(1 + glyph.len(), 2 + glyph.len()).color(border),
    ]).font(theme::UI).cursor(rune::Cursor::Pointer);
    match tip {
        Some(t) => b.help(t),
        None => b,
    }
}

// ---------------------------------------------------------------------------------------------
// The list

fn list_row(s: &State, i: usize, wide: bool) -> El {
    let b = &s.boxes[i];
    let on = i == s.cursor;
    let live = s.live.as_deref() == Some(&b.key);
    let (dot, dot_c) = glyph(s, false, b.state.as_str());
    let (ag, ag_c) = glyph(s, true, &b.agent);
    let nested = if b.parent.is_empty() || s.box_by_id(&b.parent).is_none() { "" } else { "└ " };
    let name = match (b.note(), on) {
        (Some(m), true) => m.to_string(),
        _ => b.name.clone(),
    };
    let lit = s.focus == Focus::List || s.live.is_none();
    let fill = if on && lit { theme::row_cursor() } else if live { theme::band() } else { Color::TRANSPARENT };
    let mut r = row().key(format!("box-{}", b.key)).w_full().h(ROW_H).px(6.0).gap(8.0).center_y().radius(5.0).fill(fill)
        .cursor(rune::Cursor::Pointer)
        .child(t(if on { "❯" } else { " " }, theme::gold()).w(10.0))
        .child(t(dot, dot_c).w(12.0))
        .child(t(ag, ag_c).w(12.0))
        .child(t(nested, theme::dim()))
        .child(text(name).font(if on || live { theme::UI_BOLD } else { theme::UI }).color(theme::text()).truncate().shrink());
    if wide {
        for tag in &b.tags {
            r = r.child(t(tag.clone(), theme::dim()));
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

fn diffstat(b: &Sandbox) -> El {
    if (b.added, b.deleted) == (0, 0) {
        return t("", theme::dim());
    }
    let (a, d) = (format!("+{}", b.added), format!(" −{}", b.deleted));
    let n = a.len();
    rich(format!("{a}{d}"), vec![Span::new(0, n).color(theme::done()), Span::new(n, n + d.len()).color(theme::salmon())]).font(theme::UI_SMALL)
}

fn band(label: &str, right: String, gold: bool) -> El {
    let (bg, fg) = if gold { (theme::gold(), theme::on_gold()) } else { (theme::band(), theme::grey()) };
    let font = if gold { theme::UI_BOLD } else { theme::UI };
    row().w_full().h(ROW_H).px(10.0).center_y().fill(bg).radius(4.0)
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
            c = c.child(row().pl(54.0).h(16.0).gap(6.0).children(b.tags.iter().map(|x| t(x.clone(), theme::dim()).font(theme::UI_SMALL))));
        }
    }
    c
}

// ---------------------------------------------------------------------------------------------
// The sidebar

fn sidebar(s: &State) -> El {
    let focused = s.focus == Focus::List;
    let border = if focused { theme::mark() } else { theme::inactive() };
    let entry = |label: &str, id: &'static str| {
        row().w_full().h(24.0).px(6.0).center_y().radius(5.0).cursor(rune::Cursor::Pointer)
            .child(t(label.to_string(), theme::grey())).child(spacer())
            .child(t(keys::shortcut_of(s, id).unwrap_or_default(), theme::dim()).font(theme::UI_SMALL))
            .on_press_tx(move |s: &mut State, tx: &mut Tx| run_command(s, id, tx))
    };
    let body = col().w_full().h_full().gap(6.0).min_h(0.0)
        .child(list_block(s, false))
        .child(spacer())
        .child(row().w_full().center().child(logo::view(5.0, theme::bg())))
        .child(spacer())
        .child(el().h(1.0).w_full().fill(theme::inactive()))
        .child(entry("+ new discobox", "new"))
        .child(entry("find a box or command", "palette"))
        .child(entry("all discoboxes", "detach"));
    col().key("sidebar").w(SIDEBAR_W).h_full().gap(4.0)
        .child(row().h(20.0).px(4.0).center_y().child(t("discobox", theme::grey())))
        .child(frame("sidebar-frame", focused, Some(tab("discoboxes", true, border)),
            Some(button("‹", border, keys::shortcut_of(s, "sidebar").map(|k| format!("hide the sidebar  {k}")))
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
    format!("{n} {name}{}", if p.exited { " ·done" } else { "" })
}

fn column(s: &State, ws: &Workspace, sbx: &Sandbox, side: Side, two: bool) -> El {
    let idx = ws.column(side);
    let Some(active) = ws.active[side as usize] else { return el() };
    let focused = s.focus == Focus::Panes && ws.focus == side;
    let border = if focused { theme::mark() } else { theme::inactive() };
    let mut tabs = row().gap(2.0);
    for &i in &idx {
        let label = if two || idx.len() > 1 { pane_label(ws, i, sbx) } else { sbx.name.clone() };
        tabs = tabs.child(tab(&label, i == active, border).cursor(rune::Cursor::Pointer)
            .on_press(move |s: &mut State| s.select_pane(i))
            .on_double_press_tx(move |s: &mut State, tx: &mut Tx| run_command(s, "maximize", tx)));
    }
    let mut buttons = row().gap(2.0);
    if two || ws.maximized {
        let tip = keys::shortcut_of(s, "maximize").map(|k| format!("{}  {k}", if ws.maximized { "restore" } else { "maximize" }));
        buttons = buttons.child(button(if ws.maximized { "-" } else { "+" }, border, tip)
            .on_press_tx(move |s: &mut State, tx: &mut Tx| {
                s.select_pane(active);
                run_command(s, "maximize", tx)
            }));
    }
    if ws.panes[active].exec.kind != TabKind::Primary {
        let tip = keys::shortcut_of(s, "end").map(|k| format!("end this session  {k}"));
        buttons = buttons.child(button("x", border, tip).on_press_tx(move |s: &mut State, tx: &mut Tx| {
            s.select_pane(active);
            run_command(s, "end", tx);
        }));
    }
    let pane = &ws.panes[active];
    let id = pane.stream;
    let mut screen = el().key(format!("screen-{id}")).w_full().grow(1.0).min_h(0.0).clip_rounded(2.0)
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
        screen = screen.child(text(st.clone()).font(theme::UI_SMALL).color(theme::on_gold()).px(8.0).py(2.0).radius(4.0)
            .fill(if pane.exited { theme::err() } else { theme::gold() }).pin(None, None, Some(6.0), Some(6.0)));
    }
    frame(format!("col-{side:?}"), focused, Some(tabs), Some(buttons), screen).grow(1.0).min_w(0.0)
}

fn header(s: &State, sbx: &Sandbox) -> El {
    let mut r = row().w_full().h(20.0).px(12.0).gap(18.0).center_y();
    if !s.sidebar {
        r = r.child(button("›", theme::inactive(), keys::shortcut_of(s, "sidebar").map(|k| format!("show the sidebar  {k}")))
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
        r = r.child(t(tag.clone(), theme::dim()));
    }
    let id = sbx.key.clone();
    r.child(spacer())
        .child(t(format!("cpu {}%  mem {:.1} GiB", sbx.cpu, sbx.memory_bytes as f64 / (1u64 << 30) as f64), theme::dim()))
        .child(button("…", theme::inactive(), Some("box actions".into()))
            .key("box-actions")
            .on_press(move |s: &mut State| s.open_menu(id.clone(), (900.0, 30.0))))
}

fn workspace(s: &State, ws: &Workspace) -> El {
    let Some(sbx) = s.box_by_id(&ws.sandbox) else { return el() };
    let two = !ws.column(Side::Shells).is_empty() && !ws.column(Side::Terminals).is_empty();
    let mut cols = row().w_full().grow(1.0).gap(8.0).min_h(0.0);
    if let Some(e) = &ws.error {
        let enter = keys::shortcut_of(s, "attach").unwrap_or_else(|| "Enter".into());
        cols = cols.child(frame("failed", true, Some(tab(&sbx.name, true, theme::mark())), None,
            col().w_full().h_full().center().gap(8.0)
                .child(t("could not open its sessions", theme::warn()))
                .child(text(e.clone()).font(theme::UI_SMALL).color(theme::grey()).max_w(640.0).wrap())
                .child(t(format!("{enter} tries again"), theme::dim()))));
    } else if ws.opening {
        cols = cols.child(frame("opening", true, Some(tab(&sbx.name, true, theme::mark())), None,
            col().w_full().h_full().center().child(t("attaching…", theme::dim()))));
    } else if ws.maximized {
        cols = cols.child(column(s, ws, sbx, ws.focus, two));
    } else {
        cols = cols.child(column(s, ws, sbx, Side::Terminals, two));
        if two {
            cols = cols.child(column(s, ws, sbx, Side::Shells, two));
        }
    }
    col().key("workspace").grow(1.0).h_full().min_w(0.0).gap(4.0)
        .child(header(s, sbx))
        .child(cols)
        .child(row().h(20.0).px(12.0).center_y()
            .child(status_line(s, &["shell", "terminal", "end", "next pane", "maximize", "next box", "palette"])))
}

// ---------------------------------------------------------------------------------------------
// Every box

fn home(s: &State) -> El {
    let header = row().w_full().h(24.0).gap(16.0).center_y()
        .child(t("discobox", theme::grey()))
        .child(t("all discoboxes", theme::grey()))
        .child(spacer())
        .child(t(if s.bridged { "" } else { "mock data" }, theme::warn()).font(theme::UI_SMALL));
    let rule = || el().h(1.0).w_full().fill(theme::inactive());
    let composer = col().w_full().gap(8.0).px(18.0)
        .child(t("Enter runs the prompt in a new discobox, or just creates one when it is empty", theme::dim()))
        .child(rule())
        .child(row().w_full().gap(10.0).center_y()
            .child(t("❯", theme::grey()))
            .child(text_input(&TEXT_EDIT.with(|e| e.clone()), theme::UI.lh(20.0)).key("composer").single_line().grow(1.0).color(theme::text())
                .placeholder("What should the new discobox do?", theme::dim())
                .on_submit(|s: &mut State, tx: &mut Tx| run_command(s, "new", tx))))
        .child(rule());
    let body = col().w_full().h_full().gap(10.0).px(8.0).min_h(0.0)
        .child(header)
        .child(list_block(s, true))
        .child(spacer())
        .child(composer)
        .child(row().h(20.0).px(18.0).center_y().child(status_line(s, &["attach", "new", "palette", "quit"])));
    frame("home", true, None, None, body)
}

thread_local! {
    static TEXT_EDIT: rune::TextEdit = rune::TextEdit::new("");
}

pub fn view(s: &State, _: &mut Cx<State>) -> El {
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
