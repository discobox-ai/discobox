//! The palette, a box's actions menu and the list's filter: the desktop's ways in to every
//! command.
//!
//! The palette lists every command the spec names and the app's navigation, with their
//! shortcuts, and every box. The menu is a box's actions as the console core gives them (the
//! TUI's own entries, rules and reasons), natively on macOS and drawn in the window elsewhere,
//! where a disabled entry says why it is. The filter is the core's choices for the list, each
//! with what it would list, drawn in the window on every platform: a native menu has no room
//! for the counts.

use rune::{Color, El, Key, KeyEvent, PopupItem, PopupMenu, Tx, col, el, icon, row, spacer, text, text_input};

use crate::wire::{ChoiceGroup, Entry, SpecTarget, State as BoxState};
use crate::{Focus, Menu, State, icons, keys, run_command, theme, view};

pub fn show_menu(s: &mut State, sandbox: String, at: (f32, f32), actions: Vec<Entry>, tx: &mut Tx) {
    if actions.is_empty() {
        return;
    }
    if rune::popup_menu_supported() {
        let mut menu = PopupMenu::new();
        for a in &actions {
            menu = menu.item(PopupItem::new(a.command.clone(), a.label.clone()).enabled(a.enabled));
        }
        tx.popup_menu(menu, at, move |s: &mut State, picked, tx| {
            if let Some(id) = picked {
                run_on(s, &sandbox, id, tx);
            }
        });
    } else {
        let picked = actions.iter().position(|a| a.enabled).unwrap_or(0);
        s.menu = Some(Menu { sandbox, at, actions, picked });
    }
}

/// Runs a box command on the box the menu was opened for — with the boxes marked beside it
/// when it is one of them, as the menu's entries were ruled on for them all.
fn run_on(s: &mut State, sandbox: &str, id: &str, tx: &mut Tx) {
    if let Some(i) = s.row_of(sandbox) {
        if !s.is_selected(i) {
            s.clear_selection();
        }
        s.cursor = i;
    }
    s.focus = Focus::List;
    run_command(s, id, tx);
}

pub fn menu_key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool {
    let Some(m) = s.menu.as_mut() else { return false };
    let n = m.actions.len();
    match ev.key {
        Key::Up => m.picked = (m.picked + n - 1) % n,
        Key::Down => m.picked = (m.picked + 1) % n,
        Key::Escape => s.menu = None,
        Key::Enter => {
            let a = &m.actions[m.picked];
            if a.enabled {
                let (sandbox, id) = (m.sandbox.clone(), a.command.clone());
                s.menu = None;
                run_on(s, &sandbox, &id, tx);
            }
        }
        _ => {}
    }
    true
}

pub fn menu_view(s: &State) -> Option<El> {
    let m = s.menu.as_ref()?;
    let mut list = col().key("box-menu").w(320.0).padding(6.0).gap(2.0).radius(theme::r_card())
        .fill(theme::raised()).border(theme::stroke_quiet(), theme::line_bright()).shadow(12.0, 36.0, Color::BLACK.alpha(0.5));
    for (i, a) in m.actions.iter().enumerate() {
        let on = i == m.picked;
        let ink = if a.enabled { theme::text() } else { theme::dim() };
        let sub = if a.enabled { a.detail.clone() } else { a.why.clone() };
        let (sandbox, id, enabled) = (m.sandbox.clone(), a.command.clone(), a.enabled);
        let mut head = row().w_full().center_y().child(text(a.label.clone()).font(theme::UI).color(ink)).child(spacer());
        if let Some(k) = keys::shortcut_of(s, &a.command) {
            head = head.child(view::keycap(k));
        }
        list = list.child(col().key(format!("menu-{}", a.command)).w_full().px(10.0).py(5.0).gap(2.0).radius(theme::r_row())
            .fill(if on && a.enabled { theme::selection() } else { Color::TRANSPARENT }).motion(theme::SNAPPY)
            .child(head)
            .child(text(sub).font(theme::UI_SMALL).color(if a.enabled { theme::grey() } else { theme::warn().alpha(0.8) }).truncate())
            .on_hover(move |s: &mut State| if let Some(m) = s.menu.as_mut() { m.picked = i })
            .on_press_tx(move |s: &mut State, tx: &mut Tx| {
                if enabled {
                    s.menu = None;
                    run_on(s, &sandbox, &id, tx);
                }
            }));
    }
    Some(el().w_full().h_full().absolute(0.0, 0.0)
        .on_press(|s: &mut State| s.menu = None)
        .child(list.absolute(m.at.0, m.at.1)))
}

// ---------------------------------------------------------------------------------------------
// The palette

struct Item {
    title: String,
    group: String,
    shortcut: Option<String>,
    command: Option<String>,
    sandbox: Option<String>,
}

fn items(s: &State) -> Vec<Item> {
    let query = s.palette.as_ref().map(|p| p.query.text.to_lowercase()).unwrap_or_default();
    let fits = |t: &str| {
        let t = t.to_lowercase();
        let mut it = t.chars();
        query.chars().all(|q| it.any(|c| c == q))
    };
    // Every box listed, whatever the list's filter (the core's search ignores it, ADR
    // 26-11-06-671 §5); the archived only while they are shown.
    let archived = s.listing.filter.archived;
    let mut out: Vec<Item> = s.boxes.iter().filter(|b| fits(&b.name) && (archived || b.state != BoxState::Archived)).map(|b| Item {
        title: b.name.clone(), group: "discobox".into(), shortcut: None, command: None, sandbox: Some(b.key.clone()),
    }).collect();
    for c in &s.spec.commands {
        if c.aliases.iter().chain([&c.label]).any(|a| fits(a)) {
            out.push(Item {
                title: c.label.clone(), group: if c.target == SpecTarget::Box { "box" } else { "console" }.into(),
                shortcut: keys::shortcut_of(s, &c.id),
                command: Some(c.id.clone()), sandbox: None,
            });
        }
    }
    out
}

fn pick(s: &mut State, i: usize, tx: &mut Tx) {
    let all = items(s);
    let Some(it) = all.get(i) else { return };
    let (cmd, sbx) = (it.command.clone(), it.sandbox.clone());
    s.palette = None;
    if let Some(id) = sbx
        && let Some(b) = s.box_by_id(&id).cloned()
    {
        s.open_box(&b.key);
        if !b.running() {
            s.focus = Focus::List;
        }
    } else if let Some(c) = cmd {
        run_command(s, &c, tx);
    }
}

pub fn palette_view(s: &State) -> Option<El> {
    let p = s.palette.as_ref()?;
    let all = items(s);
    let field = text_input(&p.query, theme::UI.lh(22.0)).key("palette-query").single_line().w_full().color(theme::text())
        .placeholder("Find a discobox or a command", theme::dim())
        .on_edit(|s: &mut State| &mut s.palette.as_mut().unwrap().query)
        .on_input_change(|s: &mut State, _| if let Some(p) = s.palette.as_mut() { p.picked = 0 })
        .on_input_key(|s: &mut State, ev: &KeyEvent, tx: &mut Tx| {
            let n = items(s).len().max(1);
            let Some(p) = s.palette.as_mut() else { return false };
            match ev.key {
                Key::Down => p.picked = (p.picked + 1) % n,
                Key::Up => p.picked = (p.picked + n - 1) % n,
                Key::Escape => s.palette = None,
                Key::Enter => {
                    let i = p.picked;
                    pick(s, i, tx);
                }
                _ => return false,
            }
            true
        });
    let mut list = col().w_full().gap(1.0);
    for (i, it) in all.iter().enumerate().take(14) {
        let on = i == p.picked;
        let glyph = if it.sandbox.is_some() { &icons::BOX } else { &icons::COMMAND };
        let mut line = row().key(format!("pal-{i}")).w_full().h(30.0).px(12.0).gap(10.0).center_y().radius(theme::r_row())
            .fill(if on { theme::selection() } else { Color::TRANSPARENT }).motion(theme::SNAPPY)
            .child(icon(glyph, 14.0).color(if on { theme::mark() } else { theme::grey() }))
            .child(text(it.title.clone()).font(theme::UI).color(theme::text()))
            .child(text(it.group.clone()).font(theme::UI_SMALL).color(theme::dim()))
            .child(spacer());
        if let Some(k) = &it.shortcut {
            line = line.child(view::keycap(k.clone()));
        }
        list = list.child(line
            .on_hover(move |s: &mut State| if let Some(p) = s.palette.as_mut() { p.picked = i })
            .on_press_tx(move |s: &mut State, tx: &mut Tx| pick(s, i, tx)));
    }
    if all.is_empty() {
        list = list.child(text("nothing matches").font(theme::UI).color(theme::dim()).padding(12.0));
    }
    let panel = col().key("palette").w(620.0).padding(8.0).gap(6.0).radius(theme::r_card())
        .fill(theme::raised()).border(theme::stroke_focus(), theme::mark()).shadow(16.0, 48.0, Color::BLACK.alpha(0.55))
        .child(row().w_full().h(36.0).px(10.0).gap(10.0).center_y().radius(theme::r_row()).fill(theme::sunken())
            .child(icon(&icons::SEARCH, 16.0).color(theme::grey())).child(field))
        .child(list);
    Some(el().w_full().h_full().absolute(0.0, 0.0).fill(theme::bg().alpha(0.6))
        .on_press(|s: &mut State| s.palette = None)
        .child(row().w_full().pt(90.0).items_start().child(spacer()).child(panel.on_press(|_: &mut State| {})).child(spacer())))
}

// ---------------------------------------------------------------------------------------------
// The list's filter

/// Marks a choice, as the core says marking it does: in place of its group's choice, or a tag
/// alongside the others — and a tag marked already is let go.
fn choose(s: &mut State, i: usize) {
    let Some(c) = s.listing.choices.get(i) else { return };
    let f = if c.marked && c.several { c.unmark.clone() } else { c.mark.clone() };
    // A choice marked already, and not one to let go, changes nothing.
    if f == s.listing.filter {
        return;
    }
    s.set_filter(f);
}

pub fn filter_key(s: &mut State, ev: &KeyEvent, _tx: &mut Tx) -> bool {
    let n = s.listing.choices.len().max(1);
    let Some(m) = s.filter_menu.as_mut() else { return false };
    match ev.key {
        Key::Up => m.picked = (m.picked + n - 1) % n,
        Key::Down => m.picked = (m.picked + 1) % n,
        Key::Escape => s.filter_menu = None,
        // Space marks and stays, for several tags in one trip; Enter marks and closes.
        Key::Text(ref t) if t == " " => {
            let i = m.picked;
            choose(s, i);
        }
        Key::Enter => {
            let i = m.picked;
            s.filter_menu = None;
            choose(s, i);
        }
        _ => {}
    }
    true
}

fn group_title(g: ChoiceGroup) -> &'static str {
    match g {
        ChoiceGroup::Server => "Server",
        ChoiceGroup::Folder => "Folder",
        _ => "Tag",
    }
}

pub fn filter_view(s: &State) -> Option<El> {
    let m = s.filter_menu.as_ref()?;
    let mut list = col().key("filter-menu").w(360.0).padding(6.0).gap(2.0).radius(theme::r_card())
        .fill(theme::raised()).border(theme::stroke_quiet(), theme::line_bright()).shadow(12.0, 36.0, Color::BLACK.alpha(0.5));
    let mut group = None;
    for (i, c) in s.listing.choices.iter().enumerate() {
        if group != Some(c.group) {
            group = Some(c.group);
            list = list.child(text(group_title(c.group)).font(theme::UI_SMALL).color(theme::dim()).px(10.0).pt(if i == 0 { 2.0 } else { 6.0 }));
        }
        let on = i == m.picked;
        let mark = if c.marked { icon(&icons::CHECK, 12.0).color(theme::mark()) } else { el() };
        list = list.child(row().key(format!("filter-{i}")).w_full().px(10.0).py(4.0).gap(8.0).center_y().radius(theme::r_row())
            .fill(if on { theme::selection() } else { Color::TRANSPARENT }).motion(theme::SNAPPY)
            .child(el().size(12.0, 12.0).child(mark))
            .child(text(c.label.clone()).font(if c.marked { theme::UI_BOLD } else { theme::UI }).color(theme::text()).truncate().shrink())
            .child(spacer())
            .child(text(c.detail.clone()).font(theme::UI_SMALL).color(theme::grey()))
            .on_hover(move |s: &mut State| if let Some(m) = s.filter_menu.as_mut() { m.picked = i })
            .on_press(move |s: &mut State| {
                // A tag stays open for the next; anything else is one choice, and closes.
                if !s.listing.choices.get(i).is_some_and(|c| c.several) {
                    s.filter_menu = None;
                }
                choose(s, i);
            }));
    }
    let archived = s.listing.filter.archived;
    let (what, id) = (if archived { "hide archived" } else { "show archived" }, "archived");
    let mut toggle = row().key("filter-archived").w_full().px(10.0).py(4.0).gap(8.0).center_y().radius(theme::r_row())
        .hover(theme::text().alpha(0.06)).cursor(rune::Cursor::Pointer)
        .child(el().size(12.0, 12.0).child(if archived { icon(&icons::CHECK, 12.0).color(theme::mark()) } else { el() }))
        .child(text(what).font(theme::UI).color(theme::grey()))
        .child(spacer());
    if let Some(k) = keys::shortcut_of(s, id) {
        toggle = toggle.child(view::keycap(k));
    }
    list = list.child(el().h(1.0).w_full().fill(theme::line())).child(toggle.on_press_tx(|s: &mut State, tx: &mut Tx| run_command(s, "archived", tx)));
    Some(el().w_full().h_full().absolute(0.0, 0.0)
        .on_press(|s: &mut State| s.filter_menu = None)
        .child(list.absolute(s.filter_at.0, s.filter_at.1).on_press(|_: &mut State| {})))
}
