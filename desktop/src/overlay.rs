//! The desktop's ways in to every command: the palette, the menu bar (drawn in the window off
//! macOS), a box's actions menu, a pane's menu, the help, and the list's filter.
//!
//! The palette is ADR 26-11-06-671's: a query and at most one chip, a command or a box, in either
//! order; what it finds, and in what order, is the core's (the palette topic). The menus list the
//! registry's commands by their desktop menu, as the core rules on them where they were opened
//! (the commands topic), where a disabled entry says why it is; a box's actions are the core's
//! lifecycle entries for it, natively on macOS. The help is every command by its menu, with its
//! shortcut. The filter is the core's choices for the list, each with what it would list, drawn
//! in the window on every platform: a native menu has no room for the counts.

use rune::{Color, El, Key, KeyEvent, PopupItem, PopupMenu, ScrollSpec, Tx, col, el, icon, row, spacer, text, text_input};

use crate::wire::{ChoiceGroup, Entry, HarnessAction, Menu as DesktopMenu, PaletteRow, SpecTarget};
use crate::{Focus, Menu, MenuKind, PALETTE_ROWS, State, harnesses, icons, keys, lacking, reader, row_id, run_command, run_on, secrets, theme, tools, view};

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
                act_on(s, &sandbox, id, tx);
            }
        });
    } else {
        let picked = actions.iter().position(|a| a.enabled).unwrap_or(0);
        s.menu = Some(Menu { kind: MenuKind::Actions(sandbox), at, entries: actions, picked, sub: 0 });
    }
}

/// Runs a box command on the box the menu was opened for — with the boxes marked beside it
/// when it is one of them, as the menu's entries were ruled on for them all.
fn act_on(s: &mut State, sandbox: &str, id: &str, tx: &mut Tx) {
    if let Some(i) = s.row_of(sandbox) {
        if !s.is_selected(i) {
            s.clear_selection();
        }
        s.cursor = i;
    }
    s.focus = Focus::List;
    run_command(s, id, tx);
}

/// Runs a menu's entry: a box's action on its box, any other on what the window has picked.
fn choose_entry(s: &mut State, i: usize, tx: &mut Tx) {
    let Some(m) = s.menu.as_ref() else { return };
    let Some(e) = m.entries.get(i) else { return };
    if !e.enabled {
        s.message = Some(e.why.clone());
        return;
    }
    let (kind, id, at) = (m.kind.clone(), e.command.clone(), m.at);
    match kind {
        MenuKind::Actions(sandbox) => act_on(s, &sandbox, &id, tx),
        MenuKind::Bar(_) | MenuKind::Pane => run_command(s, &id, tx),
        MenuKind::Services(sandbox, services) => {
            if let Some(v) = services.into_iter().find(|v| v.id == id) {
                s.open_service_menu(sandbox, v);
            }
        }
        MenuKind::Service(sandbox, service) => {
            s.close_overlays();
            match service.actions.iter().find(|a| a.verb.as_str() == id) {
                Some(a) => s.service_verb(sandbox, &service, a.verb),
                None => s.show_service(&sandbox, &service.id),
            }
        }
        MenuKind::Harness(_) => {
            let current = s.harnesses.as_ref().and_then(|p| p.current());
            if let Some(a) = current.and_then(|h| h.actions.iter().find(|a| a.action.as_str() == id)).map(|a| a.action) {
                harnesses::act(s, a, String::new(), at);
            }
        }
        MenuKind::HarnessFiles(_) => harnesses::act(s, HarnessAction::Files, id, (0.0, 0.0)),
        MenuKind::Tools(sandbox, tools, addresses) => tools::choose(s, sandbox, tools, addresses, &id, tx),
        MenuKind::ToolFiles(sandbox, tool) => tools::edit(s, sandbox, &tool, id),
        MenuKind::Secret(_) => secrets::pick(s, &id),
    }
}

/// Opens the menu bar's menu `by` along from the one open, wrapping.
fn step_bar(s: &mut State, by: i64) {
    let Some(MenuKind::Bar(menu)) = s.menu.as_ref().map(|m| m.kind.clone()) else { return };
    let menus = &s.spec.menus;
    let at = menus.iter().position(|m| *m == menu).unwrap_or(0) as i64;
    let next = menus[(at + by).rem_euclid(menus.len() as i64) as usize];
    open_bar(s, next);
}

/// Opens one of the menu bar's menus, under its title.
pub fn open_bar(s: &mut State, menu: DesktopMenu) {
    let at = s.bar_at.get(&menu).copied().unwrap_or((10.0, 34.0));
    s.open_commands_menu(MenuKind::Bar(menu), at);
}

pub fn menu_key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool {
    let Some(m) = s.menu.as_mut() else { return false };
    let n = m.entries.len().max(1);
    match ev.key {
        Key::Up => m.picked = (m.picked + n - 1) % n,
        Key::Down => m.picked = (m.picked + 1) % n,
        Key::Left => step_bar(s, -1),
        Key::Right => step_bar(s, 1),
        Key::Escape => {
            if let Some(m) = s.menu.take() {
                s.unsubscribe(m.sub);
            }
        }
        Key::Enter => {
            let i = m.picked;
            choose_entry(s, i, tx);
        }
        _ => {}
    }
    true
}

pub fn menu_view(s: &State) -> Option<El> {
    let m = s.menu.as_ref()?;
    let compact = matches!(m.kind, MenuKind::Bar(_) | MenuKind::Pane);
    let mut list = col().key("menu").w(340.0).padding(6.0).gap(2.0).radius(theme::r_card())
        .fill(theme::raised()).border(theme::stroke_quiet(), theme::line_bright()).shadow(12.0, 36.0, Color::BLACK.alpha(0.5));
    for (i, a) in m.entries.iter().enumerate() {
        let on = i == m.picked;
        let ink = if a.enabled { theme::text() } else { theme::dim() };
        let sub = if a.enabled { a.detail.clone() } else { a.why.clone() };
        let mut head = row().w_full().center_y().child(text(a.label.clone()).font(theme::UI).color(ink)).child(spacer());
        // A service's entries are services and verbs, and a harness's are actions and files, not
        // commands: they have no shortcut.
        let commands = !matches!(m.kind, MenuKind::Services(..) | MenuKind::Service(..) | MenuKind::Harness(_) | MenuKind::HarnessFiles(_)
            | MenuKind::Tools(..) | MenuKind::ToolFiles(..));
        if let Some(k) = keys::shortcut_of(s, &a.command).filter(|_| commands) {
            head = head.child(view::keycap(k));
        }
        // A box's actions and its services say what each does, or why it cannot, under it; the
        // menu bar's and a pane's menus are longer, one line each, saying it on the pointer.
        let entry = col().key(format!("menu-{}", a.command)).w_full().px(10.0).py(if compact { 3.0 } else { 5.0 }).gap(2.0).radius(theme::r_row())
            .fill(if on && a.enabled { theme::selection() } else { Color::TRANSPARENT }).motion(theme::SNAPPY)
            .child(head);
        let entry = if compact {
            entry.help(sub)
        } else {
            entry.child(text(sub).font(theme::UI_SMALL).color(if a.enabled { theme::grey() } else { theme::warn().alpha(0.8) }).truncate())
        };
        list = list.child(entry
            .on_hover(move |s: &mut State| if let Some(m) = s.menu.as_mut() { m.picked = i })
            .on_press_tx(move |s: &mut State, tx: &mut Tx| choose_entry(s, i, tx)));
    }
    Some(el().w_full().h_full().absolute(0.0, 0.0)
        .on_press(|s: &mut State| if let Some(m) = s.menu.take() { s.unsubscribe(m.sub) })
        .child(list.absolute(m.at.0, m.at.1).on_press(|_: &mut State| {})))
}

// ---------------------------------------------------------------------------------------------
// The menu bar, off macOS (on macOS it is the system's)

/// The menu bar: the desktop's menus by name, each opening its commands under it. While one is
/// open, the pointer over another opens that one instead, as a menu bar does.
pub fn menu_bar(s: &State) -> El {
    let open = match s.menu.as_ref().map(|m| &m.kind) {
        Some(MenuKind::Bar(menu)) => Some(*menu),
        _ => None,
    };
    let mut bar = row().key("menubar").w_full().h(26.0).px(4.0).gap(2.0).center_y();
    for &name in &s.spec.menus {
        let on = open == Some(name);
        bar = bar.child(text(name.as_str()).font(theme::UI).color(if on { theme::text() } else { theme::grey() })
            .key(format!("menubar-{}", name.as_str())).px(10.0).py(3.0).radius(theme::r_row())
            .fill(if on { theme::selection() } else { Color::TRANSPARENT })
            .hover(theme::text().alpha(0.06)).press(theme::text().alpha(0.12)).motion(theme::SNAPPY).cursor(rune::Cursor::Pointer)
            .on_layout(move |s: &mut State, r: [f32; 4]| { s.bar_at.insert(name, (r[0], r[1] + r[3] + 2.0)); })
            .on_hover(move |s: &mut State| {
                if matches!(s.menu.as_ref().map(|m| &m.kind), Some(MenuKind::Bar(n)) if *n != name) {
                    open_bar(s, name);
                }
            })
            .on_press(move |s: &mut State| {
                if matches!(s.menu.as_ref().map(|m| &m.kind), Some(MenuKind::Bar(n)) if *n == name) {
                    if let Some(m) = s.menu.take() {
                        s.unsubscribe(m.sub);
                    }
                } else {
                    open_bar(s, name);
                }
            }));
    }
    bar
}

// ---------------------------------------------------------------------------------------------
// The help

/// What the help lists, in the order it draws it: each command by its menu, with its label and
/// its shortcut, or what reaches it instead. The help's find and copy read it (`reader`).
pub fn help_lines(s: &State) -> Vec<(String, String, String)> {
    let mut out = vec![];
    for &menu in &s.spec.menus {
        for c in s.spec.commands.iter().filter(|c| c.bindings.desktop.menu == Some(menu)) {
            let lacks = lacking(&s.spec, &c.id).is_some();
            let key = keys::shortcut_of(s, &c.id).unwrap_or_else(|| if lacks { "not yet" } else { "menu, palette" }.into());
            out.push((menu.as_str().to_string(), c.label.clone(), key));
        }
    }
    out
}

/// Every command by its menu: what it does, and its shortcut, or that the palette and the menu
/// reach it; one the app does not answer yet says so. Found through, copied and scrolled as any
/// read-only card is (`reader`).
pub fn help_view(s: &State) -> Option<El> {
    if !s.help {
        return None;
    }
    let lines = help_lines(s);
    let pieces: Vec<String> = lines.iter().map(|(_, label, _)| label.clone()).collect();
    let mut columns = row().w_full().gap(24.0).items_start();
    let mut column = col().gap(2.0).w(300.0);
    let mut height = 0;
    let per_column = s.spec.commands.len().div_ceil(3) + s.spec.menus.len();
    let mut i = 0;
    for &menu in &s.spec.menus {
        let commands: Vec<_> = s.spec.commands.iter().filter(|c| c.bindings.desktop.menu == Some(menu)).collect();
        if height > 0 && height + commands.len() > per_column {
            columns = columns.child(column);
            column = col().gap(2.0).w(300.0);
            height = 0;
        }
        height += commands.len() + 1;
        column = column.child(text(menu.as_str()).font(theme::UI_BOLD).color(theme::gold()).pt(if height > commands.len() + 1 { 8.0 } else { 0.0 }));
        for c in commands {
            let lacks = lacking(&s.spec, &c.id).is_some();
            let label = text(c.label.clone()).font(theme::UI).color(if lacks { theme::dim() } else { theme::text() }).truncate().shrink();
            let mut line = row().key(format!("help-{}", c.id)).w_full().h(22.0).gap(8.0).center_y()
                .child(reader::lit(s, &pieces, i, label))
                .child(spacer());
            line = match keys::shortcut_of(s, &c.id) {
                Some(k) => line.child(view::keycap(k)),
                None => line.child(text(lines[i].2.clone()).font(theme::UI_SMALL).color(theme::dim())),
            };
            column = column.child(line.help(c.detail.clone()));
            i += 1;
        }
    }
    columns = columns.child(column);
    let mut close = row().gap(6.0).center_y().child(text("Esc closes").font(theme::UI_SMALL).color(theme::dim()));
    if let Some(k) = keys::shortcut_of(s, "palette") {
        close = close.child(text("· the palette finds any of them").font(theme::UI_SMALL).color(theme::dim())).child(view::keycap(k));
    }
    let panel = col().key("help").padding(18.0).gap(12.0).radius(theme::r_card())
        .fill(theme::raised()).border(theme::stroke_focus(), theme::mark()).shadow(16.0, 48.0, Color::BLACK.alpha(0.55))
        .child(row().w_full().center_y().child(text("Commands").font(theme::UI_BOLD).color(theme::text())).child(spacer()).child(close))
        .child(reader::follow_scroll(s, col().key("help-body").w_full().max_h(640.0).min_h(0.0).child(columns).scroll(ScrollSpec { stick_bottom: false })))
        .child(reader::foot(s, &pieces));
    Some(el().w_full().h_full().absolute(0.0, 0.0).fill(theme::bg().alpha(0.6))
        .on_press(|s: &mut State| s.help = false)
        .child(row().w_full().pt(60.0).items_start().child(spacer()).child(panel.on_press(|_: &mut State| {})).child(spacer())))
}

// ---------------------------------------------------------------------------------------------
// The palette

/// A key that is one letter, whatever the modifiers made of it.
fn letter(ev: &KeyEvent) -> Option<char> {
    match &ev.key {
        Key::Command(c) => Some(c.to_ascii_lowercase()),
        Key::Text(t) if t.chars().count() == 1 => t.chars().next().map(|c| c.to_ascii_lowercase()),
        _ => None,
    }
}

/// The palette's own keys while it is open (671 §2): the arrows, Ctrl-N and Ctrl-P move, and
/// Shift-Tab moves back; Tab composes with no chip and moves with one; Enter chooses; Esc drops
/// the chip, then the query, then the palette; Alt-A takes archived boxes in or out of scope.
pub fn palette_key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool {
    let Some(p) = s.palette.as_mut() else { return false };
    let chip = !p.command.is_empty() || !p.sandbox.is_empty();
    let ctrl = ev.mods.ctrl && !ev.mods.alt;
    match ev.key {
        Key::Down => p.step(1),
        Key::Up => p.step(-1),
        Key::PageDown => p.step(PALETTE_ROWS as i64),
        Key::PageUp => p.step(-(PALETTE_ROWS as i64)),
        Key::Tab if ev.mods.shift => p.step(-1),
        Key::Tab if chip => p.step(1),
        Key::Tab => choose(s, true, tx),
        Key::Enter => choose(s, false, tx),
        Key::Escape => {
            if chip {
                p.command.clear();
                p.sandbox.clear();
            } else if !p.query.text.is_empty() {
                p.set_text("");
            } else {
                s.close_palette();
                return true;
            }
            p.picked.clear();
            s.ask_palette();
        }
        _ if ctrl && letter(ev) == Some('n') => p.step(1),
        _ if ctrl && letter(ev) == Some('p') => p.step(-1),
        _ if ev.mods.alt && letter(ev) == Some('a') => toggle_archived(s),
        _ => return false,
    }
    true
}

fn toggle_archived(s: &mut State) {
    let Some(p) = s.palette.as_mut() else { return };
    p.archived = !p.archived;
    p.picked.clear();
    s.ask_palette();
}

/// Chooses the picked row (671 §2–3). With no chip, a box is gone to, or with `complete` (Tab,
/// its `commands` control) becomes the chip; a box command becomes the chip; a window command
/// runs, or with `complete` fills in its name. With a chip, the row and the chip are run
/// together, on that box. A row that cannot be chosen says why and stays.
pub fn choose(s: &mut State, complete: bool, tx: &mut Tx) {
    let Some(p) = s.palette.as_mut() else { return };
    let Some(r) = p.at().map(|i| p.rows[i].clone()) else {
        p.reason = Some(if p.found { "nothing is picked".into() } else { "still looking".into() });
        return;
    };
    if !r.why.is_empty() {
        p.reason = Some(r.why);
        return;
    }
    let command = r.command.as_ref().filter(|_| r.r#box.is_empty()).map(|c| c.command.clone());
    if p.command.is_empty() && p.sandbox.is_empty() {
        match command {
            None if complete => {
                p.sandbox = r.r#box;
                p.set_text("");
                p.picked.clear();
                s.ask_palette();
            }
            None => {
                s.close_palette();
                s.navigate(&r.r#box);
            }
            Some(id) => {
                let spec = s.spec.command(&id).cloned();
                let p = s.palette.as_mut().unwrap();
                match spec {
                    Some(c) if c.target == SpecTarget::Box => {
                        p.command = id;
                        p.set_text("");
                        p.picked.clear();
                        s.ask_palette();
                    }
                    Some(c) if complete => {
                        p.set_text(&c.label);
                        p.picked.clear();
                        s.ask_palette();
                    }
                    _ => run_command(s, &id, tx),
                }
            }
        }
        return;
    }
    let (id, target) = match command {
        Some(id) => (id, p.sandbox.clone()),
        None => (p.command.clone(), r.r#box),
    };
    run_on(s, &id, vec![target], tx);
}

/// A palette row: a box with its state, name, short ID, state, source and server, and a
/// `commands` control to make it the chip; a command with what it does and its shortcut; either
/// dimmed with why when it cannot be chosen.
fn palette_row(s: &State, i: usize, r: &PaletteRow, on: bool, chip: bool) -> El {
    let why = (!r.why.is_empty()).then(|| r.why.clone());
    let ink = if why.is_some() { theme::dim() } else { theme::text() };
    let mut line = row().key(format!("pal-{}", row_id(r))).w_full().h(30.0).px(12.0).gap(10.0).center_y().radius(theme::r_row())
        .fill(if on { theme::selection() } else { Color::TRANSPARENT }).motion(theme::SNAPPY);
    match (&r.command, s.box_by_id(&r.r#box)) {
        (Some(c), _) if r.r#box.is_empty() => {
            line = line.child(icon(&icons::COMMAND, 14.0).color(if on { theme::mark() } else { theme::grey() }))
                .child(text(c.label.clone()).font(theme::UI).color(ink))
                .child(text(why.clone().unwrap_or_else(|| c.detail.clone())).font(theme::UI_SMALL).color(if why.is_some() { theme::warn().alpha(0.8) } else { theme::dim() }).truncate().shrink())
                .child(spacer());
            if let Some(k) = keys::shortcut_of(s, &c.command) {
                line = line.child(view::keycap(k));
            }
        }
        (_, Some(b)) => {
            let (dot, dot_c) = view::glyph(s, false, b.state.as_str());
            let short = b.id.strip_prefix("sbx_").unwrap_or(&b.id).chars().take(8).collect::<String>();
            line = line.child(icon(&icons::BOX, 14.0).color(if on { theme::mark() } else { theme::grey() }))
                .child(text(dot).font(theme::UI).color(dot_c))
                .child(text(b.name.clone()).font(theme::UI).color(ink).truncate().shrink())
                .child(text(short).font(theme::UI_SMALL).color(theme::dim()))
                .child(text(b.state.as_str().to_string()).font(theme::UI_SMALL).color(theme::grey()))
                .child(text(if b.source.is_empty() { "no source".into() } else { b.source.clone() }).font(theme::UI_SMALL).color(theme::grey()).truncate().shrink());
            if !b.server.is_empty() {
                line = line.child(text(b.server.clone()).font(theme::UI_SMALL).color(theme::dim()));
            }
            if let Some(w) = why {
                line = line.child(text(w).font(theme::UI_SMALL).color(theme::warn().alpha(0.8)).truncate().shrink());
            }
            line = line.child(spacer());
            if !chip {
                let mut more = row().key(format!("pal-commands-{}", b.key)).px(6.0).h(20.0).center_y().radius(theme::r_key())
                    .hover(theme::text().alpha(0.08)).cursor(rune::Cursor::Pointer)
                    .child(text("commands").font(theme::UI_SMALL).color(theme::mark()));
                more = more.help("the commands for this discobox  Tab".to_string());
                line = line.child(more.on_press_tx(move |s: &mut State, tx: &mut Tx| {
                    if let Some(p) = s.palette.as_mut() {
                        p.pick(i);
                    }
                    choose(s, true, tx);
                }));
            }
        }
        _ => line = line.child(text(r.r#box.clone()).font(theme::UI).color(theme::dim())),
    }
    line.on_hover(move |s: &mut State| if let Some(p) = s.palette.as_mut() { p.pick(i) })
        .on_press(move |s: &mut State| if let Some(p) = s.palette.as_mut() { p.pick(i) })
        .on_double_press_tx(move |s: &mut State, tx: &mut Tx| {
            if let Some(p) = s.palette.as_mut() {
                p.pick(i);
            }
            choose(s, false, tx);
        })
}

/// A key the palette answers, and what it does, as its hint line says it.
fn hint(key: &str, what: &str) -> El {
    row().gap(4.0).center_y().child(view::keycap(key.to_string())).child(text(what.to_string()).font(theme::UI_SMALL).color(theme::dim()))
}

pub fn palette_view(s: &State) -> Option<El> {
    let p = s.palette.as_ref()?;
    let chip_on = !p.command.is_empty() || !p.sandbox.is_empty();
    let field = text_input(&p.query, theme::UI.lh(22.0)).key("palette-query").single_line().grow(1.0).color(theme::text())
        .placeholder(if chip_on { "Narrow it down" } else { "Find a discobox or a command" }, theme::dim())
        .on_edit(|s: &mut State| &mut s.palette.as_mut().unwrap().query)
        .on_input_change(|s: &mut State, _| {
            if let Some(p) = s.palette.as_mut() {
                p.picked.clear();
            }
            s.ask_palette();
        })
        .on_input_key(palette_key);
    let mut input = row().w_full().h(36.0).px(10.0).gap(10.0).center_y().radius(theme::r_row()).fill(theme::sunken())
        .child(icon(&icons::SEARCH, 16.0).color(theme::grey()));
    if chip_on {
        let label = if p.command.is_empty() {
            s.box_by_id(&p.sandbox).map(|b| b.name.clone()).unwrap_or_else(|| p.sandbox.clone())
        } else {
            s.spec.command(&p.command).map(|c| c.label.clone()).unwrap_or_else(|| p.command.clone())
        };
        let glyph = if p.command.is_empty() { &icons::BOX } else { &icons::COMMAND };
        input = input.child(row().key("palette-chip").h(24.0).pl(8.0).pr(4.0).gap(6.0).center_y().radius(theme::r_key())
            .fill(theme::mark().alpha(0.16)).border(theme::stroke_quiet(), theme::mark()).cursor(rune::Cursor::Pointer)
            .help("drop the chip, keeping the query  Esc".to_string())
            .child(icon(glyph, 12.0).color(theme::mark()))
            .child(text(label).font(theme::UI_BOLD).color(theme::text()))
            .child(icon(&icons::CHEVRON, 10.0).color(theme::grey()))
            .child(icon(&icons::CLOSE, 10.0).color(theme::grey()))
            .on_press(|s: &mut State| {
                if let Some(p) = s.palette.as_mut() {
                    p.command.clear();
                    p.sandbox.clear();
                    p.picked.clear();
                }
                s.ask_palette();
            }));
    }
    input = input.child(field);

    let mut scope = row().w_full().px(4.0).gap(8.0).center_y()
        .child(text("All servers · all folders").font(theme::UI_SMALL).color(theme::grey()));
    let archived = row().key("palette-archived").gap(4.0).px(4.0).center_y().radius(theme::r_key())
        .hover(theme::text().alpha(0.08)).cursor(rune::Cursor::Pointer)
        .child(el().size(12.0, 12.0).child(if p.archived { icon(&icons::CHECK, 12.0).color(theme::mark()) } else { el() }))
        .child(text("include archived").font(theme::UI_SMALL).color(theme::grey()))
        .child(view::keycap(keys::label("alt+a")))
        .on_press(|s: &mut State| toggle_archived(s));
    scope = scope.child(archived).child(spacer())
        .child(text(if p.found { format!("{} results", p.rows.len()) } else { "looking…".into() }).font(theme::UI_SMALL).color(theme::dim()));

    let mut list = col().w_full().gap(1.0);
    let at = p.at();
    for (i, r) in p.rows.iter().enumerate().skip(p.offset).take(PALETTE_ROWS) {
        list = list.child(palette_row(s, i, r, at == Some(i), chip_on));
    }
    if p.found && p.rows.is_empty() {
        let what = if p.query.text.is_empty() { "nothing applies".to_string() } else { format!("no applicable matches for \u{201c}{}\u{201d}", p.query.text) };
        list = list.child(text(what).font(theme::UI).color(theme::dim()).padding(12.0));
    }
    // What the core's search says about the servers it searched: every one, whatever the list's
    // filter.
    if !p.partial.is_empty() {
        list = list.child(text(format!("partial results: {}", p.partial.join(", "))).font(theme::UI_SMALL).color(theme::warn()).px(12.0));
    }
    for n in &p.stale {
        list = list.child(text(format!("{} {}", n.title, n.detail)).font(theme::UI_SMALL).color(theme::warn()).px(12.0).truncate());
    }
    if let Some(why) = &p.reason {
        list = list.child(text(why.clone()).font(theme::UI_SMALL).color(theme::warn()).px(12.0).truncate());
    }
    let hints = row().w_full().px(4.0).gap(14.0).center_y()
        .child(hint(&keys::label("up"), "")).child(hint(&keys::label("down"), "move"))
        .child(hint(&keys::label("tab"), if chip_on { "next" } else { "compose" }))
        .child(hint(&keys::label("enter"), "choose"))
        .child(hint(&keys::label("escape"), if chip_on { "drop the chip" } else { "back" }));
    let panel = col().key("palette").w(720.0).padding(8.0).gap(6.0).radius(theme::r_card())
        .fill(theme::raised()).border(theme::stroke_focus(), theme::mark()).shadow(16.0, 48.0, Color::BLACK.alpha(0.55))
        .child(input).child(scope).child(list).child(el().h(1.0).w_full().fill(theme::line())).child(hints);
    Some(el().w_full().h_full().absolute(0.0, 0.0).fill(theme::bg().alpha(0.6))
        .on_press(|s: &mut State| s.close_palette())
        .child(row().w_full().pt(90.0).items_start().child(spacer()).child(panel.on_press(|_: &mut State| {})).child(spacer())))
}

// ---------------------------------------------------------------------------------------------
// The list's filter

/// Marks a choice, as the core says marking it does: in place of its group's choice, or a tag
/// alongside the others. `toggle` (Space, a press) lets a tag marked already go; Enter only ever
/// marks, as the TUI's card does, so a tag it is on is kept.
///
/// The choices are the last listing's, counted against its filter: while another filter is out
/// and not yet listed, a choice would be made against the one it replaces — dropping a tag
/// marked a moment before — so none is taken until it lands.
fn choose_filter(s: &mut State, i: usize, toggle: bool) {
    if s.asked.is_some() {
        s.message = Some("the list is being read with the last filter".into());
        return;
    }
    let Some(c) = s.listing.choices.get(i) else { return };
    let f = if toggle && c.marked && c.several { c.unmark.clone() } else { c.mark.clone() };
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
            choose_filter(s, i, true);
        }
        Key::Enter => {
            let i = m.picked;
            s.filter_menu = None;
            choose_filter(s, i, false);
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
                choose_filter(s, i, true);
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
