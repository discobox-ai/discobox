//! The palette and a box's actions menu: the desktop's ways in to every command.
//!
//! The palette lists every command the spec names and the app's navigation, with their
//! shortcuts, and every box. The menu is a box's actions as the console core gives them (the
//! TUI's own entries, rules and reasons), natively on macOS and drawn in the window elsewhere,
//! where a disabled entry says why it is.

use rune::{Color, El, Key, KeyEvent, PopupItem, PopupMenu, Span, Tx, col, el, rich, row, spacer, text, text_input};

use crate::wire::Entry;
use crate::{Focus, Menu, State, keys, run_command, theme};

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

/// Runs a box command on the box the menu was opened for.
fn run_on(s: &mut State, sandbox: &str, id: &str, tx: &mut Tx) {
    if let Some(i) = s.boxes.iter().position(|b| b.key == sandbox) {
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
    let mut list = col().key("box-menu").w(320.0).padding(6.0).gap(2.0).radius(10.0)
        .fill(theme::hex(0x18181b)).border(1.0, theme::inactive()).shadow(12.0, 36.0, Color::BLACK.alpha(0.5));
    for (i, a) in m.actions.iter().enumerate() {
        let on = i == m.picked;
        let ink = if a.enabled { theme::text() } else { theme::dim() };
        let sub = if a.enabled { a.detail.clone() } else { a.why.clone() };
        let (sandbox, id, enabled) = (m.sandbox.clone(), a.command.clone(), a.enabled);
        list = list.child(col().key(format!("menu-{}", a.command)).w_full().px(10.0).py(5.0).radius(6.0)
            .fill(if on && a.enabled { theme::mark().alpha(0.22) } else { Color::TRANSPARENT })
            .child(row().w_full().child(text(a.label.clone()).font(theme::UI).color(ink)).child(spacer())
                .child(text(keys::shortcut_of(s, &a.command).unwrap_or_default()).font(theme::UI_SMALL).color(theme::dim())))
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
    let mut out: Vec<Item> = s.boxes.iter().filter(|b| fits(&b.name)).map(|b| Item {
        title: b.name.clone(), group: "discobox".into(), shortcut: None, command: None, sandbox: Some(b.key.clone()),
    }).collect();
    for c in &s.spec.commands {
        if c.aliases.iter().chain([&c.label]).any(|a| fits(a)) {
            out.push(Item {
                title: c.label.clone(), group: if c.target == "box" { "box" } else { "console" }.into(),
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
        && let Some(i) = s.boxes.iter().position(|b| b.key == id)
    {
        s.open_box(i);
        if !s.boxes[i].running() {
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
        let glyph = if it.sandbox.is_some() { "▸ " } else { "" };
        let label = format!("{glyph}{}", it.title);
        let n = label.len();
        let g = it.group.clone();
        let line = rich(format!("{label}   {g}"), vec![
            Span::new(0, n).color(theme::text()),
            Span::new(n, n + 3 + g.len()).color(theme::dim()),
        ]).font(theme::UI);
        list = list.child(row().key(format!("pal-{i}")).w_full().h(30.0).px(12.0).center_y().radius(6.0)
            .fill(if on { theme::mark().alpha(0.22) } else { Color::TRANSPARENT })
            .child(line).child(spacer())
            .child(text(it.shortcut.clone().unwrap_or_default()).font(theme::UI_SMALL).color(theme::grey()))
            .on_hover(move |s: &mut State| if let Some(p) = s.palette.as_mut() { p.picked = i })
            .on_press_tx(move |s: &mut State, tx: &mut Tx| pick(s, i, tx)));
    }
    if all.is_empty() {
        list = list.child(text("nothing matches").font(theme::UI).color(theme::dim()).padding(12.0));
    }
    let panel = col().key("palette").w(620.0).padding(8.0).gap(6.0).radius(12.0)
        .fill(theme::hex(0x18181b)).border(1.0, theme::mark().alpha(0.6)).shadow(16.0, 48.0, Color::BLACK.alpha(0.55))
        .child(row().w_full().h(36.0).px(10.0).gap(10.0).center_y().radius(8.0).fill(theme::hex(0x0f0f12))
            .child(text("⌕").font(theme::UI).color(theme::grey())).child(field))
        .child(list);
    Some(el().w_full().h_full().absolute(0.0, 0.0).fill(Color::BLACK.alpha(0.35))
        .on_press(|s: &mut State| s.palette = None)
        .child(row().w_full().pt(90.0).items_start().child(spacer()).child(panel.on_press(|_: &mut State| {})).child(spacer())))
}
