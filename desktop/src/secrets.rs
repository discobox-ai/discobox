//! The secrets page, a secret read with what is bound to it, a binding read in full, and a
//! secret's binding history.
//!
//! What is listed, what each row says, what reading a secret, a binding or a decision says, what
//! deleting and unbinding ask, and what each action says it did are the core's
//! (`console.DescribeSecret`, `DescribeBinding`, `DescribeDecision`, `DeleteSecretQuestion`,
//! `UnbindQuestion`), carried by the credentials command: the page is opened on the secrets the
//! command read, each action is the same command naming the secret and the action — a form, a
//! question or the history coming back on the way — and its result carries the secrets read
//! again. The page is drawn beside the list, in place of the workspace, and has its own keys
//! while it is up, as a screen does; reading a secret, a binding or the history is a panel over
//! the window.
//!
//! Under the secrets, the requests waiting on the page's server (`console.RequestsOn`, the TUI's
//! requests table): what each asks for, where, who asks and how long ago, in the core's words.
//! Tab moves the keys between the two tables; Enter on a request answers it, through the core's
//! cards (the credentials command's answer action), and the page is read again after.

use rune::{Color, El, Key, KeyEvent, PointerPress, ScrollSpec, Tx, col, el, row, spacer, text};

use crate::harnesses::{hint, panel, t};
use crate::wire::{Args, CardTone, Decision, DetailSection, Entry, Request, Result as CommandResult, Secret, SecretAction, SecretStanding, SecretWords};
use crate::{Menu, MenuKind, Pending, State, theme};

/// The secrets page: the secrets of the server it was opened for, and the one the keys are on.
pub struct SecretPage {
    pub server: String,
    pub rows: Vec<Secret>,
    pub cursor: usize,
    /// What the page says beside its secrets, in the core's words.
    pub words: SecretWords,
    /// The requests waiting on the page's server, oldest first, and the one the keys are on
    /// while they are on the requests table.
    pub requests: Vec<Request>,
    pub request: usize,
    pub on_requests: bool,
}

impl SecretPage {
    pub fn current(&self) -> Option<&Secret> { self.rows.get(self.cursor) }

    pub fn secret(&self, id: &str) -> Option<&Secret> { self.rows.iter().find(|s| s.id == id) }

    /// Takes the secrets read again, with the keys on `about` when it is still one of them (the
    /// one acted on), else on the secret they were on, else where they were.
    fn take(&mut self, rows: Vec<Secret>, about: &str) {
        let on = if about.is_empty() { self.current().map(|s| s.id.clone()).unwrap_or_default() } else { about.to_string() };
        self.rows = rows;
        self.cursor = self.rows.iter().position(|s| s.id == on).unwrap_or(self.cursor.min(self.rows.len().saturating_sub(1)));
    }

    /// Takes the requests read again with the secrets, the keys on the one they were on when it
    /// still waits; with none left, the keys go back to the secrets.
    fn take_requests(&mut self, requests: Vec<Request>) {
        let on = self.requests.get(self.request).map(|r| r.id.clone());
        self.requests = requests;
        self.request = on.and_then(|id| self.requests.iter().position(|r| r.id == id)).unwrap_or(self.request.min(self.requests.len().saturating_sub(1)));
        self.on_requests &= !self.requests.is_empty();
    }

    pub fn current_request(&self) -> Option<&Request> { self.requests.get(self.request).filter(|_| self.on_requests) }
}

/// What is read over the window about a secret, each by its ID so a reading follows the secrets
/// read again: the secret with its bindings (the row the keys are on: 0 binds, then each
/// binding), one binding in full, the history (the decision the keys are on), or one decision.
pub enum Panel {
    Secret { secret: String, picked: usize },
    Binding { secret: String, binding: String },
    History { secret: String, entries: Vec<Decision>, picked: usize },
    Decision { secret: String, entries: Vec<Decision>, at: usize, delegation: bool },
}

/// The credentials command, opening the page on the server the list is narrowed to (the primary
/// for none). What it read opens the page (`open_page`).
pub fn open(s: &mut State) {
    let server = s.listing.filter.server.clone();
    let context = s.context();
    s.command("credentials", vec![], context, Args { server: server.clone(), ..Default::default() }, Pending::Secrets(server));
}

/// The page, on the secrets the core read for it from `server`, which every action on the page
/// goes to whatever the list's filter has moved to since. It takes the keys, as the harnesses
/// page does: nothing reaches a workspace it covers.
pub fn open_page(s: &mut State, server: String, r: &CommandResult) {
    s.close_overlays();
    s.close_pages();
    s.focus = crate::Focus::List;
    s.secrets = Some(SecretPage {
        server, rows: r.secrets.clone(), cursor: 0, words: r.secret_words.clone().unwrap_or_default(),
        requests: r.requests.clone(), request: 0, on_requests: false,
    });
}

pub fn close_page(s: &mut State) {
    s.secrets = None;
    s.secret_panel = None;
}

/// Reads the page's secrets again, keeping the keys where they are: the core's read, which is no
/// use of the command the way opening the page is.
pub fn reread(s: &mut State) {
    let Some(server) = s.secrets.as_ref().map(|p| p.server.clone()) else { return };
    let context = s.context();
    let args = Args { server, secret_action: Some(SecretAction::Read), ..Default::default() };
    s.command("credentials", vec![], context, args, Pending::Secret(SecretAction::Read));
}

/// The secret an action is done to: the one a panel over the page reads, else the one under the
/// page's keys.
fn subject(s: &State) -> Option<Secret> {
    let page = s.secrets.as_ref()?;
    match &s.secret_panel {
        Some(Panel::Secret { secret, .. } | Panel::Binding { secret, .. }) => page.secret(secret).cloned(),
        _ => page.current().cloned(),
    }
}

/// Does one of the page's actions: new stores a secret, and every other is done to the secret
/// under the keys — unbind to one of its bindings. The core asks what it asks first (a form, a
/// question) and answers with the secrets read again; history comes back as a panel.
pub fn act(s: &mut State, action: SecretAction, binding: String) {
    let secret = match action {
        SecretAction::New | SecretAction::Read => None,
        _ => match subject(s) {
            Some(secret) => Some(secret),
            None => {
                s.message = Some("no secret to act on".into());
                return;
            }
        },
    };
    s.close_overlays();
    s.message = match (action, &secret) {
        (SecretAction::History, Some(secret)) => Some(format!("reading {}'s history…", secret.name)),
        _ => None,
    };
    let server = s.secrets.as_ref().map(|p| p.server.clone()).unwrap_or_default();
    let context = s.context();
    let args = Args { server, secret: secret.map(|x| x.id).unwrap_or_default(), secret_action: Some(action), binding, ..Default::default() };
    s.command("credentials", vec![], context, args, Pending::Secret(action));
}

/// What an action came to: the secrets read again with the keys on the one it was about, what
/// it says it did, and the history for the history action. A reading over the window follows
/// what it read: a binding taken back goes back to its secret, a secret deleted closes it.
pub fn take(s: &mut State, action: SecretAction, r: &CommandResult) {
    if action == SecretAction::History {
        s.message = (!r.note.is_empty()).then(|| r.note.clone());
        if !r.history.is_empty() {
            s.secret_panel = Some(Panel::History { secret: r.secret.clone(), entries: r.history.clone(), picked: 0 });
        }
        return;
    }
    if let Some(p) = s.secrets.as_mut() {
        p.take(r.secrets.clone(), &r.secret);
        p.take_requests(r.requests.clone());
        if let Some(w) = r.secret_words.clone() {
            p.words = w;
        }
    }
    // A read again says nothing of its own, and leaves what was said before it standing: an
    // answer that failed is read again after its why is said.
    if action != SecretAction::Read || !r.note.is_empty() {
        s.message = (!r.note.is_empty()).then(|| r.note.clone());
    }
    let Some(p) = s.secrets.as_ref() else { return };
    s.secret_panel = match s.secret_panel.take() {
        Some(Panel::Binding { secret, binding }) => match p.secret(&secret) {
            Some(x) if x.bindings.iter().any(|b| b.id == binding) => Some(Panel::Binding { secret, binding }),
            Some(_) => Some(Panel::Secret { secret, picked: 0 }),
            None => None,
        },
        Some(Panel::Secret { secret, picked }) => p.secret(&secret).map(|x| Panel::Secret { picked: picked.min(x.bindings.len()), secret }),
        other => other,
    };
}

/// Answers a request waiting on the page's server, through the core's cards: the credentials
/// command's answer action naming it. Its result is the page read again.
pub fn answer(s: &mut State, request: String) {
    let Some(server) = s.secrets.as_ref().map(|p| p.server.clone()) else { return };
    s.close_overlays();
    s.message = None;
    let context = s.context();
    let args = Args { server, secret_action: Some(SecretAction::Answer), request, ..Default::default() };
    s.command("credentials", vec![], context, args, Pending::Secret(SecretAction::Answer));
}

/// The page's actions, as a menu: storing a new secret, then what can be done to the one under
/// the keys, each saying what it does.
pub fn open_menu(s: &mut State, at: (f32, f32)) {
    let current = s.secrets.as_ref().and_then(SecretPage::current).cloned();
    s.close_overlays();
    let why = if current.is_some() { String::new() } else { "no secret is chosen".to_string() };
    let entry = |action: SecretAction, label: &str, detail: &str, enabled: bool| Entry {
        command: entry_id(action.as_str()), label: label.into(), detail: detail.into(), enabled,
        why: if enabled { String::new() } else { why.clone() }, uses: 0,
    };
    let on = current.is_some();
    let mut entries = vec![
        Entry { command: entry_id(OPEN), label: "open".into(), detail: "what it is, and what is bound to it".into(), enabled: on, why: why.clone(), uses: 0 },
        entry(SecretAction::Bind, "bind", "hand it over before anything asks", on),
        entry(SecretAction::Edit, "edit", "its name, routes, binding limit or value", on),
        entry(SecretAction::History, "history", "its bindings made and taken back, and answered requests", on),
        entry(SecretAction::Delete, "delete", "remove it and every binding of it", on),
        entry(SecretAction::New, "new secret", "store a credential the project does not have yet", true),
    ];
    entries.push(Entry { command: entry_id(REREAD), label: "read again".into(), detail: "the secrets as the server has them now".into(), enabled: true, why: String::new(), uses: 0 });
    let picked = entries.iter().position(|e| e.enabled).unwrap_or(0);
    s.menu = Some(Menu { kind: MenuKind::Secret(current.map(|x| x.id).unwrap_or_default()), at, entries, picked, sub: 0 });
}

/// The menu's entries that are not the core's actions: reading the secret, and reading the
/// secrets again.
pub const OPEN: &str = "open";
pub const REREAD: &str = "read again";

/// A menu entry's ID: the action's name under the page's own prefix, so it is never read as
/// the registry command of the same name (`new`), whose shortcut the menu would draw.
fn entry_id(name: &str) -> String { format!("secret:{name}") }

/// An entry of a secret's menu, taken.
pub fn pick(s: &mut State, id: &str) {
    let id = id.strip_prefix("secret:").unwrap_or(id);
    match id {
        OPEN => open_secret(s),
        REREAD => reread(s),
        _ => {
            let action = [SecretAction::New, SecretAction::Edit, SecretAction::Delete, SecretAction::Bind, SecretAction::History]
                .into_iter().find(|a| a.as_str() == id);
            if let Some(action) = action {
                act(s, action, String::new());
            }
        }
    }
}

/// Reads the secret under the keys: what it is, then what is bound to it.
pub fn open_secret(s: &mut State) {
    let Some(secret) = s.secrets.as_ref().and_then(SecretPage::current).map(|x| x.id.clone()) else { return };
    s.close_overlays();
    s.secret_panel = Some(Panel::Secret { secret, picked: 0 });
}

/// Where the page's menu opens from the keys: beside its row.
fn row_at(s: &State) -> (f32, f32) {
    let left = if s.sidebar { 340.0 } else { 40.0 };
    (left + 260.0, 130.0 + s.secrets.as_ref().map_or(0, |p| p.cursor) as f32 * ROW_H)
}

/// The page's own keys while it is up: the arrows move, Enter reads the secret, the
/// context-menu key opens the page's actions, Esc leaves. Anything else goes on to the window's
/// shortcuts.
pub fn page_key(s: &mut State, ev: &KeyEvent, _tx: &mut Tx) -> bool {
    let plain = !(ev.mods.ctrl || ev.mods.alt || ev.mods.cmd);
    let Some(p) = s.secrets.as_mut().filter(|_| plain) else { return false };
    if p.on_requests {
        let last = p.requests.len().saturating_sub(1);
        match ev.key {
            Key::Up => p.request = p.request.saturating_sub(1),
            Key::Down => p.request = (p.request + 1).min(last),
            Key::Home => p.request = 0,
            Key::End => p.request = last,
            Key::Tab => p.on_requests = false,
            Key::Escape => close_page(s),
            Key::Enter if !ev.mods.shift => {
                if let Some(id) = p.current_request().map(|r| r.id.clone()) {
                    answer(s, id);
                }
            }
            _ => return false,
        }
        return true;
    }
    let last = p.rows.len().saturating_sub(1);
    match ev.key {
        Key::Up => p.cursor = p.cursor.saturating_sub(1),
        Key::Down => p.cursor = (p.cursor + 1).min(last),
        Key::Home => p.cursor = 0,
        Key::End => p.cursor = last,
        Key::Tab if !p.requests.is_empty() => p.on_requests = true,
        Key::Escape => close_page(s),
        Key::Enter if !ev.mods.shift => open_secret(s),
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

/// A panel's keys, while one is up: Esc goes back the way it came (a binding to its secret, a
/// decision to the history) and closes the rest; the arrows move along a list; Enter reads what
/// they are on — or, on a secret's first row, binds it; Delete takes the binding under the keys
/// back, asking first.
pub fn panel_key(s: &mut State, ev: &KeyEvent) -> bool {
    let Some(panel) = s.secret_panel.as_mut() else { return false };
    match panel {
        Panel::Secret { secret, picked } => {
            let n = s.secrets.as_ref().and_then(|p| p.secret(secret)).map_or(0, |x| x.bindings.len());
            match ev.key {
                Key::Escape => s.secret_panel = None,
                Key::Up => *picked = picked.saturating_sub(1),
                Key::Down => *picked = (*picked + 1).min(n),
                Key::Enter => {
                    let (secret, picked) = (secret.clone(), *picked);
                    secret_row(s, &secret, picked);
                }
                Key::Delete | Key::Backspace if *picked > 0 => {
                    let (secret, picked) = (secret.clone(), *picked);
                    unbind_row(s, &secret, picked);
                }
                _ => {}
            }
        }
        Panel::Binding { secret, binding } => match ev.key {
            Key::Escape => {
                let secret = secret.clone();
                let picked = s.secrets.as_ref().and_then(|p| p.secret(&secret)).and_then(|x| x.bindings.iter().position(|b| b.id == *binding)).map_or(0, |i| i + 1);
                s.secret_panel = Some(Panel::Secret { secret, picked });
            }
            Key::Delete | Key::Backspace => {
                let binding = binding.clone();
                act(s, SecretAction::Unbind, binding);
            }
            _ => {}
        },
        Panel::History { entries, picked, secret } => match ev.key {
            Key::Escape => s.secret_panel = None,
            Key::Up => *picked = picked.saturating_sub(1),
            Key::Down => *picked = (*picked + 1).min(entries.len().saturating_sub(1)),
            Key::Enter => {
                let (secret, entries, at) = (secret.clone(), std::mem::take(entries), *picked);
                s.secret_panel = Some(Panel::Decision { secret, entries, at, delegation: false });
            }
            _ => {}
        },
        Panel::Decision { secret, entries, at, delegation } => match ev.key {
            // Back to the history it was read from, on the decision it was.
            Key::Escape if *delegation => *delegation = false,
            Key::Escape => {
                let (secret, entries, picked) = (secret.clone(), std::mem::take(entries), *at);
                s.secret_panel = Some(Panel::History { secret, entries, picked });
            }
            Key::Enter if entries.get(*at).is_some_and(|e| !e.delegation.is_empty()) => *delegation = !*delegation,
            _ => {}
        },
    }
    true
}

/// A row of a secret's panel taken: the first binds it, any other reads that binding.
fn secret_row(s: &mut State, secret: &str, picked: usize) {
    if picked == 0 {
        act(s, SecretAction::Bind, String::new());
        return;
    }
    let binding = s.secrets.as_ref().and_then(|p| p.secret(secret)).and_then(|x| x.bindings.get(picked - 1)).map(|b| b.id.clone());
    if let Some(binding) = binding {
        s.secret_panel = Some(Panel::Binding { secret: secret.to_string(), binding });
    }
}

/// Takes a binding of a secret's panel back, by its row, asking first.
fn unbind_row(s: &mut State, secret: &str, picked: usize) {
    let binding = s.secrets.as_ref().and_then(|p| p.secret(secret)).and_then(|x| x.bindings.get(picked.wrapping_sub(1))).map(|b| b.id.clone());
    if let Some(binding) = binding {
        act(s, SecretAction::Unbind, binding);
    }
}

// ---------------------------------------------------------------------------------------------
// Drawing

const ROW_H: f32 = 28.0;

/// A tone of the core's, as this window draws it.
pub(crate) fn ink(tone: CardTone) -> Color {
    match tone {
        CardTone::Quiet => theme::dim(),
        CardTone::Standout => theme::gold(),
        CardTone::Command => theme::mark(),
        CardTone::Alert => theme::err(),
        CardTone::Good => theme::done(),
        CardTone::Plain => theme::text(),
    }
}

/// What a secret's row says stands on it, in its colour.
fn standing(x: &Secret) -> El {
    let c = match x.standing {
        SecretStanding::Bound => theme::done(),
        SecretStanding::Refused => theme::err(),
        SecretStanding::Unbound => theme::dim(),
    };
    t(x.standing_text.clone(), c).w(100.0)
}

fn secret_row_view(p: &SecretPage, i: usize, x: &Secret) -> El {
    let on = i == p.cursor && !p.on_requests;
    let id = x.id.clone();
    let r = row().key(format!("secret-{}", x.id)).w_full().h(ROW_H).px(10.0).gap(12.0).center_y().radius(theme::r_row())
        .fill(if on { theme::selection() } else { Color::TRANSPARENT }).hover(theme::text().alpha(0.06)).motion(theme::SNAPPY)
        .cursor(rune::Cursor::Pointer)
        .child(t(x.name.clone(), if on { theme::text() } else { theme::grey() }).font(if on { theme::UI_BOLD } else { theme::UI }).truncate().shrink().grow(1.0))
        .child(t(x.r#where.clone(), if x.nowhere { theme::dim() } else { theme::text() }).w(300.0).truncate())
        .child(standing(x))
        .child(t(x.age.clone(), theme::dim()).w(80.0));
    let pick = move |s: &mut State| {
        if let Some(p) = s.secrets.as_mut() {
            p.cursor = i;
            p.on_requests = false;
        }
    };
    r.on_press(pick)
        .on_double_press_tx(move |s: &mut State, _tx: &mut Tx| {
            pick(s);
            open_secret(s);
        })
        .on_secondary_press(move |s: &mut State, at: PointerPress, _tx: &mut Tx| {
            if let Some(p) = s.secrets.as_mut() {
                p.cursor = p.rows.iter().position(|x| x.id == id).unwrap_or(p.cursor);
            }
            open_menu(s, (at.x, at.y));
        })
}

/// The page: its title band, a row per secret, and what it says when there are none.
pub fn page_view(s: &State) -> El {
    let Some(p) = s.secrets.as_ref() else { return el() };
    let n = p.rows.len();
    let count = format!("{n} secret{}", if n == 1 { "" } else { "s" });
    let mut body = col().key("secrets").w_full().h_full().gap(2.0).min_h(0.0)
        .child(row().w_full().h(26.0).px(10.0).center_y().fill(theme::gold()).radius(theme::r_row())
            .child(text("Secrets").font(theme::UI_BOLD).color(theme::on_gold()))
            .child(spacer())
            .child(text(count).font(theme::UI_BOLD).color(theme::on_gold())));
    if p.rows.is_empty() {
        // Where the action that stores one is, is this window's to say.
        body = body.child(t(format!("{} — new secret, in its actions, stores one", p.words.empty), theme::dim()).padding(10.0));
    }
    let mut rows = col().w_full().gap(2.0);
    for (i, x) in p.rows.iter().enumerate() {
        rows = rows.child(secret_row_view(p, i, x));
    }
    let requests = requests_view(p);
    let mut hints = row().w_full().h(24.0).px(10.0).gap(14.0).center_y();
    hints = if p.on_requests {
        hints.child(hint("enter", "answer")).child(hint("tab", "secrets"))
    } else {
        let h = hints.child(hint("enter", "open")).child(hint("shift+f10", "actions"));
        if p.requests.is_empty() { h } else { h.child(hint("tab", "requests")) }
    };
    let hints = hints
        .child(hint("escape", "back"))
        .child(spacer())
        .child(t(s.message.clone().unwrap_or_default(), theme::grey()).font(theme::UI_SMALL).truncate().shrink());
    body = body.child(rows.grow(1.0).min_h(0.0).scroll(ScrollSpec { stick_bottom: false })).child(requests).child(hints);
    col().key("secrets-page").grow(1.0).h_full().min_w(0.0).padding(10.0).radius(theme::r_frame())
        .border(theme::stroke_focus(), theme::mark()).child(body)
}

/// The requests table under the secrets: its band, counted in the core's words, then a row per
/// request — what it asks, where, who asks and how long ago — or what it says with none.
fn requests_view(p: &SecretPage) -> El {
    let mut c = col().key("requests").w_full().gap(2.0).pt(8.0)
        .child(row().w_full().h(26.0).px(10.0).center_y().radius(theme::r_row())
            .border(theme::stroke_quiet(), if p.on_requests { theme::err() } else { theme::line() })
            .child(text(p.words.requests.clone()).font(theme::UI_BOLD).color(if p.on_requests { theme::text() } else { theme::grey() }))
            .child(spacer())
            .child(text(if p.requests.is_empty() { String::new() } else { p.words.waiting.clone() }).font(theme::UI_BOLD).color(theme::err())));
    if p.requests.is_empty() {
        return c.child(t(p.words.nothing_waiting.clone(), theme::dim()).padding(10.0));
    }
    let mut rows = col().w_full().gap(2.0);
    for (i, r) in p.requests.iter().enumerate() {
        let on = p.on_requests && i == p.request;
        let id = r.id.clone();
        rows = rows.child(row().key(format!("request-{}", r.id)).w_full().h(ROW_H).px(10.0).gap(12.0).center_y().radius(theme::r_row())
            .fill(if on { theme::selection() } else { Color::TRANSPARENT }).hover(theme::text().alpha(0.06)).motion(theme::SNAPPY)
            .cursor(rune::Cursor::Pointer)
            .child(t(r.subject.clone(), if on { theme::text() } else { theme::grey() }).font(if on { theme::UI_BOLD } else { theme::UI }).truncate().shrink().grow(1.0))
            .child(t(r.r#where.clone(), theme::text()).w(240.0).truncate())
            .child(t(r.env_var.clone(), theme::dim()).w(150.0).truncate())
            .child(t(r.requester.clone(), theme::dim()).w(170.0).truncate())
            .child(t(r.age.clone(), theme::dim()).w(80.0))
            .on_press(move |s: &mut State| {
                if let Some(p) = s.secrets.as_mut() {
                    (p.on_requests, p.request) = (true, i);
                }
            })
            .on_double_press_tx(move |s: &mut State, _tx: &mut Tx| answer(s, id.clone())));
    }
    c = c.child(rows.max_h(ROW_H * 6.0 + 12.0).scroll(ScrollSpec { stick_bottom: false }));
    c
}

const LABEL_W: f32 = 190.0;

/// A card the core describes: each section's heading, its fields lined up, then its sentences,
/// a bullet's wrapped rows hung under its text. Every console draws the core's cards with it:
/// a secret's, a binding's, a request's on its sheet.
pub(crate) fn detail(sections: &[DetailSection]) -> El {
    let mut out = col().w_full().gap(10.0);
    for sec in sections {
        let mut c = col().w_full().gap(3.0);
        if !sec.label.is_empty() {
            c = c.child(t(sec.label.clone(), theme::gold()).font(theme::UI_BOLD));
        }
        for f in &sec.fields {
            c = c.child(row().w_full().gap(8.0).items_start()
                .child(t(f.label.clone(), theme::grey()).w(LABEL_W))
                .child(t(f.value.clone(), ink(f.tone)).wrap().grow(1.0).shrink()));
        }
        for l in &sec.lines {
            c = c.child(if l.bullet {
                row().w_full().gap(6.0).items_start().child(t("•", ink(l.tone))).child(t(l.text.clone(), ink(l.tone)).wrap().grow(1.0).shrink())
            } else {
                t(l.text.clone(), ink(l.tone)).wrap().w_full()
            });
        }
        out = out.child(c);
    }
    out
}

/// A row of a panel's list: the picked one lit.
fn list_row(key: String, label: &str, detail: &str, on: bool, press: impl Fn(&mut State) + 'static) -> El {
    let label = t(label.to_string(), if on { theme::text() } else { theme::grey() }).font(if on { theme::UI_BOLD } else { theme::UI }).truncate();
    let r = row().key(key).w_full().h(ROW_H).px(10.0).gap(12.0).center_y().radius(theme::r_row())
        .fill(if on { theme::selection() } else { Color::TRANSPARENT }).hover(theme::text().alpha(0.06)).cursor(rune::Cursor::Pointer);
    // A row with nothing beside its label (a decision) gives the label the row: an empty detail
    // squeezed to no width draws a lone ellipsis.
    let r = if detail.is_empty() {
        r.child(label.shrink().grow(1.0))
    } else {
        r.child(label.w(260.0)).child(t(detail.to_string(), theme::dim()).truncate().shrink().grow(1.0))
    };
    r.on_press(press)
}

/// A panel's title line: what it reads, and the key that closes it.
fn title(what: String, close: &str) -> El {
    row().w_full().center_y().child(text(what).font(theme::UI_BOLD).color(theme::text())).child(spacer()).child(hint("escape", close))
}

/// The panel over the window, when one is up: a secret with what is bound to it, a binding, the
/// history, or a decision.
pub fn panel_view(s: &State) -> Option<El> {
    let open = s.secret_panel.as_ref()?;
    let page = s.secrets.as_ref()?;
    let close = |s: &mut State| s.secret_panel = None;
    match open {
        Panel::Secret { secret, picked } => {
            let x = page.secret(secret)?;
            let mut body = col().w_full().gap(12.0).min_h(0.0).child(title(x.name.clone(), "close"));
            if !x.refused.is_empty() {
                body = body.child(t(x.refused.clone(), theme::err()));
            }
            let mut list = col().w_full().gap(2.0).child(t(x.answer.clone(), theme::grey()));
            let id = x.id.clone();
            list = list.child(list_row("secret-bind".into(), &page.words.bind, &page.words.bind_detail, *picked == 0, move |s: &mut State| secret_row(s, &id, 0)));
            for (i, b) in x.bindings.iter().enumerate() {
                let id = x.id.clone();
                list = list.child(list_row(format!("binding-{}", b.id), &b.label, &b.detail, *picked == i + 1, move |s: &mut State| secret_row(s, &id, i + 1)));
            }
            let foot = row().w_full().gap(14.0).center_y().child(hint("enter", "read")).child(hint("delete", "unbind"));
            body = body.child(col().w_full().gap(12.0).child(detail(&x.card)).child(list).scroll(ScrollSpec { stick_bottom: false }).max_h(560.0)).child(foot);
            Some(panel("secret-panel", 820.0, body, close))
        }
        Panel::Binding { secret, binding } => {
            let x = page.secret(secret)?;
            let b = x.bindings.iter().find(|b| b.id == *binding)?;
            let binding = b.id.clone();
            let unbind = row().key("binding-unbind").h(28.0).px(14.0).center_y().radius(theme::r_row())
                .border(theme::stroke_quiet(), theme::err()).hover(theme::err().alpha(0.12)).cursor(rune::Cursor::Pointer)
                .child(text("Unbind").font(theme::UI).color(theme::err()))
                .on_press(move |s: &mut State| act(s, SecretAction::Unbind, binding.clone()));
            let body = col().w_full().gap(12.0).min_h(0.0)
                .child(title(format!("Binding — {}", b.label), "back"))
                .child(detail(&b.card))
                .child(row().w_full().gap(8.0).center_y().child(hint("delete", "unbind")).child(spacer()).child(unbind));
            Some(panel("binding-panel", 760.0, body, close))
        }
        Panel::History { secret, entries, picked } => {
            let name = page.secret(secret).map(|x| x.name.clone()).unwrap_or_else(|| secret.clone());
            let mut list = col().w_full().gap(2.0);
            for (i, e) in entries.iter().enumerate() {
                list = list.child(list_row(format!("decision-{}", e.id), &e.label, "", *picked == i, move |s: &mut State| {
                    if let Some(Panel::History { secret, entries, .. }) = s.secret_panel.take() {
                        s.secret_panel = Some(Panel::Decision { secret, entries, at: i, delegation: false });
                    }
                }));
            }
            let body = col().w_full().gap(12.0).min_h(0.0)
                .child(title(format!("{} — {name}", page.words.history_title), "close"))
                .child(t(page.words.history_note.clone(), theme::grey()))
                .child(list.scroll(ScrollSpec { stick_bottom: false }).max_h(560.0))
                .child(row().w_full().gap(14.0).center_y().child(hint("enter", "read")));
            Some(panel("history-panel", 820.0, body, close))
        }
        Panel::Decision { entries, at, delegation, .. } => {
            let e = entries.get(*at)?;
            let mut body = col().w_full().gap(12.0).min_h(0.0);
            if *delegation {
                body = body.child(title("The delegation it was handed on under".into(), "back")).child(detail(&e.delegation));
            } else {
                body = body.child(title("Credential decision".into(), "back")).child(detail(&e.card));
                if !e.delegation.is_empty() {
                    body = body.child(row().w_full().gap(14.0).center_y().child(hint("enter", "reads the delegation")));
                }
            }
            Some(panel("decision-panel", 760.0, body, close))
        }
    }
}
