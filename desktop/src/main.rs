//! `discobox desktop`: the discobox console as a native app, built with Rune.
//!
//! The CLI runs this with `--bridge` and serves the console's DataSource over stdin and stdout
//! (`cli/internal/cli/desktop.go`), with the console's UX spec (`cli/internal/tui/spec.go`):
//! colours, glyphs and the commands. The layout is the TUI's; the interaction is a desktop
//! app's — shortcuts and menus run the same commands the TUI's keys do.
//!
//! Without `--bridge` it runs on a mock with local shells: `cargo run --release`, or headless
//! `--snap console.png --scene workspace`.

mod backend;
mod keys;
mod logo;
mod overlay;
#[cfg(test)]
mod command_tests;
#[cfg(test)]
mod parity_tests;
mod term;
mod theme;
mod view;

use std::collections::HashMap;
use std::sync::Mutex;

use backend::{Action, Backend, Bridge, Event, Exec, Inbox, Mock, Sandbox, Spec};
use rune::{App, AppSpec, KeyEvent, MenuItem, TextEdit, Tx, Waker};
use serde_json::{Value, json};
use term::Screen;

/// Which of the two columns a pane lives in: harness terminals left, shells right.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Side {
    Terminals,
    Shells,
}

pub struct Pane {
    pub stream: u32,
    pub exec: Exec,
    pub screen: Screen,
    pub exited: bool,
    /// Reconnecting, or why it ended.
    pub status: Option<String>,
}

impl Pane {
    pub fn side(&self) -> Side { if matches!(self.exec.kind.as_str(), "shell" | "tool") { Side::Shells } else { Side::Terminals } }
}

/// One box's live sessions. Switching away parks it: its streams stay attached and its
/// screens keep emulating, so switching back shows where it is now.
pub struct Workspace {
    pub sandbox: String,
    pub panes: Vec<Pane>,
    /// The visible pane of each column, as an index into `panes`.
    pub active: [Option<usize>; 2],
    pub focus: Side,
    pub maximized: bool,
    pub opening: bool,
    /// Why listing its sessions failed; Enter asks again.
    pub error: Option<String>,
}

impl Workspace {
    pub fn column(&self, side: Side) -> Vec<usize> { (0..self.panes.len()).filter(|&i| self.panes[i].side() == side).collect() }

    pub fn focused(&self) -> Option<usize> { self.active[self.focus as usize].or(self.active[1 - self.focus as usize]) }

    /// Every pane in tab order: the terminals column, then the shells.
    pub fn strip(&self) -> Vec<usize> {
        let mut v = self.column(Side::Terminals);
        v.extend(self.column(Side::Shells));
        v
    }

    fn show(&mut self, i: usize) {
        let side = self.panes[i].side();
        self.active[side as usize] = Some(i);
        self.focus = side;
    }

    fn fix_active(&mut self) {
        for side in [Side::Terminals, Side::Shells] {
            let col = self.column(side);
            let a = &mut self.active[side as usize];
            if a.is_none_or(|i| !col.contains(&i)) {
                *a = col.last().copied();
            }
        }
        if self.active[self.focus as usize].is_none() {
            self.focus = if self.focus == Side::Shells { Side::Terminals } else { Side::Shells };
        }
    }
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Focus {
    List,
    Panes,
}

/// What a request was for, so its answer lands in the right place.
enum Pending {
    Execs(String),
    Open(u32),
    /// A new session's answer, for its box and stream.
    NewSession(String, u32),
    Actions(String, (f32, f32)),
    Do(String, String),
    Quiet,
}

pub struct Palette {
    pub query: TextEdit,
    pub picked: usize,
}

/// A box's actions menu, drawn in the window where the platform has no native one.
pub struct Menu {
    pub sandbox: String,
    pub at: (f32, f32),
    pub actions: Vec<Action>,
    pub picked: usize,
}

pub struct State {
    backend: Box<dyn Backend + Send>,
    inbox: Inbox,
    pub spec: Spec,
    pub boxes: Vec<Sandbox>,
    pub cursor: usize,
    pub workspaces: HashMap<String, Workspace>,
    /// The box on screen; `None` is the list of every box.
    pub live: Option<String>,
    pub focus: Focus,
    pub sidebar: bool,
    pub message: Option<String>,
    pub palette: Option<Palette>,
    pub menu: Option<Menu>,
    pub bridged: bool,
    pending: HashMap<u64, Pending>,
    next_stream: u32,
}

const DEFAULT_SIZE: (u16, u16) = (100, 30);

impl State {
    fn new() -> State {
        let (backend, spec, inbox, bridged) = BOOT.lock().unwrap().take().unwrap_or_else(boot_mock);
        theme::set_tokens(&spec.colors);
        let mut s = State {
            backend, inbox, spec, boxes: vec![], cursor: 0, workspaces: HashMap::new(), live: None, focus: Focus::List,
            sidebar: true, message: None, palette: None, menu: None, bridged, pending: HashMap::new(), next_stream: 1,
        };
        if !bridged {
            // The mock lists at once; taking it here lets a headless scene open a box.
            for e in s.inbox.drain() {
                if let Event::Listing(boxes) = e {
                    s.take_listing(boxes);
                }
            }
        }
        s
    }

    fn ask(&mut self, op: &str, args: Value, why: Pending) {
        debug(|| format!("→ {op} {args}"));
        let id = self.backend.request(op, args);
        self.pending.insert(id, why);
    }

    pub fn live_ws(&self) -> Option<&Workspace> { self.live.as_ref().and_then(|id| self.workspaces.get(id)) }

    fn live_ws_mut(&mut self) -> Option<&mut Workspace> {
        let id = self.live.clone()?;
        self.workspaces.get_mut(&id)
    }

    pub fn box_by_id(&self, id: &str) -> Option<&Sandbox> { self.boxes.iter().find(|b| b.id == id) }

    /// The box a box command acts on: the one on screen while its panes have the keys, else
    /// the list's cursor.
    fn target(&self) -> Option<Sandbox> {
        match (&self.live, self.focus) {
            (Some(id), Focus::Panes) => self.box_by_id(id).cloned(),
            _ => self.boxes.get(self.cursor).cloned(),
        }
    }

    fn take_listing(&mut self, boxes: Vec<Sandbox>) {
        let at = self.boxes.get(self.cursor).map(|b| b.id.clone());
        self.boxes = view::list_order(boxes);
        self.cursor = at.and_then(|id| self.boxes.iter().position(|b| b.id == id)).unwrap_or(self.cursor.min(self.boxes.len().saturating_sub(1)));
    }

    fn new_pane(&mut self, sandbox: &str, exec: Exec) -> Pane {
        let stream = self.next_stream;
        self.next_stream += 1;
        let _ = sandbox;
        Pane { stream, exec, screen: Screen::new(DEFAULT_SIZE.0 as usize, DEFAULT_SIZE.1 as usize), exited: false, status: None }
    }

    /// Show a box: its workspace if it has one, attaching on first use. A box that is not
    /// running is only pointed at — starting one is a command of its own.
    pub fn open_box(&mut self, index: usize) {
        let Some(sbx) = self.boxes.get(index).cloned() else { return };
        self.cursor = index;
        if !sbx.running() && !self.workspaces.contains_key(&sbx.id) {
            self.message = Some(format!("{} is {} — start it from its menu", sbx.name, sbx.state));
            return;
        }
        if let Some(ws) = self.workspaces.get_mut(&sbx.id)
            && ws.error.is_some()
        {
            ws.error = None;
            ws.opening = true;
            self.ask("execs", json!({"sandbox": sbx.id}), Pending::Execs(sbx.id.clone()));
        }
        if !self.workspaces.contains_key(&sbx.id) {
            self.workspaces.insert(sbx.id.clone(), Workspace {
                sandbox: sbx.id.clone(), panes: vec![], active: [None, None], focus: Side::Terminals, maximized: false, opening: true, error: None,
            });
            self.ask("execs", json!({"sandbox": sbx.id}), Pending::Execs(sbx.id.clone()));
        }
        self.live = Some(sbx.id);
        self.focus = Focus::Panes;
    }

    fn new_session(&mut self, shell: bool) {
        let Some(id) = self.live.clone() else {
            self.message = Some("open a discobox first".into());
            return;
        };
        let kind = if shell { "shell" } else { "terminal" };
        let pane = self.new_pane(&id, Exec { id: String::new(), kind: kind.into(), label: "…".into() });
        let stream = pane.stream;
        let ws = self.workspaces.get_mut(&id).unwrap();
        ws.panes.push(pane);
        ws.show(ws.panes.len() - 1);
        self.focus = Focus::Panes;
        let op = if shell { "newShell" } else { "newTerminal" };
        let (cols, rows) = DEFAULT_SIZE;
        self.ask(op, json!({"sandbox": id, "stream": stream, "cols": cols, "rows": rows}), Pending::NewSession(id.clone(), stream));
    }

    fn end_focused(&mut self) {
        let Some(id) = self.live.clone() else { return };
        let ws = self.workspaces.get_mut(&id).unwrap();
        let Some(i) = ws.focused() else { return };
        if ws.panes[i].exec.kind == "primary" {
            self.message = Some("the box's harness terminal cannot be ended".into());
            return;
        }
        let pane = ws.panes.remove(i);
        for a in ws.active.iter_mut() {
            *a = a.and_then(|a| if a == i { None } else if a > i { Some(a - 1) } else { Some(a) });
        }
        ws.fix_active();
        // A session still being made has no exec to end yet: it is ended when its answer comes.
        if !pane.exec.id.is_empty() {
            self.ask("endExec", json!({"sandbox": id, "exec": pane.exec.id, "stream": pane.stream}), Pending::Quiet);
        }
    }

    pub fn select_pane(&mut self, i: usize) {
        if let Some(ws) = self.live_ws_mut()
            && i < ws.panes.len()
        {
            ws.show(i);
        }
        self.focus = Focus::Panes;
    }

    fn step_pane(&mut self, by: i32) {
        let Some(ws) = self.live_ws_mut() else { return };
        let strip = ws.strip();
        if strip.is_empty() {
            return;
        }
        let cur = ws.focused().and_then(|f| strip.iter().position(|&i| i == f)).unwrap_or(0) as i32;
        let next = (cur + by).rem_euclid(strip.len() as i32) as usize;
        ws.show(strip[next]);
        self.focus = Focus::Panes;
    }

    fn goto_pane(&mut self, n: usize) {
        if let Some(ws) = self.live_ws_mut()
            && let Some(&i) = ws.strip().get(n)
        {
            ws.show(i);
            self.focus = Focus::Panes;
        }
    }

    /// Previous or next box: switch to it when it is running, otherwise point at it.
    fn step_box(&mut self, by: i32) {
        let n = self.boxes.len() as i32;
        if n == 0 {
            return;
        }
        self.cursor = (self.cursor as i32 + by).clamp(0, n - 1) as usize;
        if self.boxes[self.cursor].running() || self.workspaces.contains_key(&self.boxes[self.cursor].id) {
            self.open_box(self.cursor);
        } else {
            self.focus = Focus::List;
        }
    }

    pub fn send(&mut self, bytes: &[u8]) {
        let Some(ws) = self.live_ws_mut() else { return };
        let Some(i) = ws.focused() else { return };
        let pane = &mut ws.panes[i];
        if pane.exited {
            return;
        }
        pane.screen.to_bottom();
        let stream = pane.stream;
        self.backend.input(stream, bytes);
    }

    pub fn resize_pane(&mut self, stream: u32, cols: usize, rows: usize) {
        let mut hit = false;
        for ws in self.workspaces.values_mut() {
            for p in ws.panes.iter_mut().filter(|p| p.stream == stream) {
                hit |= p.screen.resize(cols, rows);
            }
        }
        if hit {
            self.backend.resize(stream, cols as u16, rows as u16);
        }
    }

    pub fn scroll_pane(&mut self, stream: u32, lines: i32) {
        for ws in self.workspaces.values_mut() {
            for p in ws.panes.iter_mut().filter(|p| p.stream == stream) {
                p.screen.scroll(lines);
            }
        }
    }

    pub fn open_menu(&mut self, sandbox: String, at: (f32, f32)) {
        self.ask("actions", json!({"targets": [sandbox.clone()]}), Pending::Actions(sandbox, at));
    }

    fn pane_mut(&mut self, stream: u32) -> Option<&mut Pane> {
        self.workspaces.values_mut().flat_map(|w| w.panes.iter_mut()).find(|p| p.stream == stream)
    }

    /// Tells a stream the size its pane is laid out at. A resize sent before the stream was
    /// attached went nowhere, so this follows every attach.
    fn resend_size(&mut self, stream: u32) {
        if let Some(p) = self.pane_mut(stream) {
            let (cols, rows) = (p.screen.cols as u16, p.screen.rows as u16);
            self.backend.resize(stream, cols, rows);
        }
    }

    fn answer(&mut self, why: Pending, r: Result<Value, String>, tx: &mut Tx) {
        match why {
            Pending::Execs(sbx) => {
                let execs: Vec<Exec> = r.as_ref().ok().and_then(|v| serde_json::from_value(v.clone()).ok()).unwrap_or_default();
                if let (Err(e), Some(ws)) = (&r, self.workspaces.get_mut(&sbx)) {
                    ws.opening = false;
                    ws.error = Some(e.clone());
                    return;
                }
                let mut opens = vec![];
                let panes: Vec<Pane> = execs.into_iter().map(|e| self.new_pane(&sbx, e)).collect();
                if let Some(ws) = self.workspaces.get_mut(&sbx) {
                    ws.opening = false;
                    for p in panes {
                        opens.push((p.exec.id.clone(), p.stream));
                        ws.panes.push(p);
                    }
                    ws.fix_active();
                }
                for (exec, stream) in opens {
                    let (cols, rows) = DEFAULT_SIZE;
                    self.ask("open", json!({"sandbox": sbx, "exec": exec, "stream": stream, "cols": cols, "rows": rows}), Pending::Open(stream));
                }
            }
            Pending::Open(stream) => match r {
                Ok(_) => self.resend_size(stream),
                Err(e) => {
                    if let Some(p) = self.pane_mut(stream) {
                        p.exited = true;
                        p.status = Some(e);
                    }
                }
            },
            Pending::NewSession(sandbox, stream) => match r {
                Ok(v) => {
                    let Ok(exec) = serde_json::from_value::<Exec>(v) else { return };
                    match self.pane_mut(stream) {
                        Some(p) => p.exec = exec,
                        // Ended before it was made: end it now that it has an id.
                        None => {
                            self.ask("endExec", json!({"sandbox": sandbox, "exec": exec.id, "stream": stream}), Pending::Quiet);
                            return;
                        }
                    }
                    self.resend_size(stream);
                }
                Err(e) => {
                    if let Some(p) = self.pane_mut(stream) {
                        p.exited = true;
                        p.status = Some(e);
                    }
                }
            },
            Pending::Actions(sandbox, at) => {
                let actions: Vec<Action> = r.ok().and_then(|v| serde_json::from_value(v).ok()).unwrap_or_default();
                overlay::show_menu(self, sandbox, at, actions, tx);
            }
            Pending::Do(verb, name) => {
                self.message = Some(match r {
                    Ok(v) => match v["note"].as_str().filter(|n| !n.is_empty()) {
                        Some(n) => n.to_string(),
                        None => format!("{verb}: {name}"),
                    },
                    Err(e) => format!("{verb} {name}: {e}"),
                });
            }
            Pending::Quiet => {
                if let Err(e) = r {
                    self.message = Some(e);
                }
            }
        }
    }
}

/// Every command, by the id the spec gives it. The keys, the palette, the menus and a row's
/// actions all come here.
pub fn run_command(s: &mut State, id: &str, tx: &mut Tx) {
    s.message = None;
    s.palette = None;
    s.menu = None;
    match id {
        "box.attach" => s.open_box(s.cursor),
        "box.shell" => {
            s.open_box(s.cursor);
            // A box that is not running is only pointed at: no shell in whichever box is on screen.
            if s.live.is_some() && s.live == s.boxes.get(s.cursor).map(|b| b.id.clone()) {
                s.new_session(true);
            }
        }
        "box.stop" | "box.start" | "box.upgrade" | "box.repair" | "box.archive" | "box.unarchive" => {
            if let Some(b) = s.target() {
                let verb = id.trim_start_matches("box.").to_string();
                let doing = match verb.as_str() {
                    "stop" => "stopping",
                    "start" => "starting",
                    "upgrade" => "upgrading",
                    "repair" => "repairing",
                    "archive" => "archiving",
                    _ => "unarchiving",
                };
                s.message = Some(format!("{doing} {}…", b.name));
                s.ask("do", json!({"verb": verb, "sandbox": b.id}), Pending::Do(verb.clone(), b.name.clone()));
            }
        }
        "box.purge" | "box.rename" | "box.settings" | "box.reviewConfig" | "box.new" => {
            let title = s.spec.command(id).map(|c| c.title.clone()).unwrap_or_else(|| id.into());
            s.message = Some(format!("{title} needs a dialog, which the prototype does not have yet"));
        }
        "pane.newShell" => s.new_session(true),
        "pane.newTerminal" => s.new_session(false),
        "pane.end" => s.end_focused(),
        "pane.next" => s.step_pane(1),
        "pane.prev" => s.step_pane(-1),
        "pane.maximize" => {
            if let Some(ws) = s.live_ws_mut() {
                ws.maximized = !ws.maximized;
            }
        }
        "box.next" => s.step_box(1),
        "box.prev" => s.step_box(-1),
        "view.sidebar" => {
            s.sidebar = !s.sidebar;
            if !s.sidebar {
                s.focus = Focus::Panes;
            }
        }
        "view.home" => {
            s.live = None;
            s.focus = Focus::List;
        }
        "app.palette" => s.palette = Some(Palette { query: { let mut q = TextEdit::new(""); q.focused = true; q }, picked: 0 }),
        "app.quit" => tx.quit(),
        _ if id.starts_with("pane.goto.") => {
            if let Ok(n) = id.trim_start_matches("pane.goto.").parse::<usize>() {
                s.goto_pane(n);
            }
        }
        _ => s.message = Some(format!("unknown command {id}")),
    }
}

fn setup(s: &mut State, waker: Waker) {
    s.inbox.set_waker(waker);
    if !s.bridged {
        s.ask("list", json!({}), Pending::Quiet);
    }
}

/// Drain what the backend sent into the window.
fn poll(s: &mut State, _t: f64, tx: &mut Tx) -> bool {
    let events = s.inbox.drain();
    if events.is_empty() {
        return false;
    }
    for e in events {
        match e {
            Event::Listing(boxes) => s.take_listing(boxes),
            Event::Reply(id, r) => {
                debug(|| format!("← {id} {}", match &r { Ok(v) => v.to_string(), Err(e) => format!("error: {e}") }));
                if let Some(why) = s.pending.remove(&id) {
                    s.answer(why, r, tx);
                }
            }
            Event::Data(stream, bytes) => {
                if let Some(p) = s.pane_mut(stream) {
                    let reply = p.screen.feed(&bytes);
                    if !reply.is_empty() {
                        s.backend.input(stream, &reply);
                    }
                }
            }
            Event::Exited(stream, msg) => {
                if let Some(p) = s.pane_mut(stream) {
                    p.exited = true;
                    p.status = (!msg.is_empty()).then_some(msg);
                }
            }
            Event::StreamState(stream, state) => {
                if let Some(p) = s.pane_mut(stream) {
                    p.status = (state == "reconnecting").then(|| "reconnecting…".into());
                }
            }
        }
    }
    true
}

/// `DISCOBOX_DESKTOP_DEBUG=1` logs every request and answer to stderr.
fn debug(line: impl FnOnce() -> String) {
    static ON: std::sync::OnceLock<bool> = std::sync::OnceLock::new();
    if *ON.get_or_init(|| std::env::var_os("DISCOBOX_DESKTOP_DEBUG").is_some()) {
        eprintln!("desktop: {}", line());
    }
}

fn on_key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool { keys::on_key(s, ev, tx) }

type Boot = (Box<dyn Backend + Send>, Spec, Inbox, bool);

/// What `main` connected, for `State::new` (an `AppSpec` init takes no arguments).
static BOOT: Mutex<Option<Boot>> = Mutex::new(None);

fn boot_mock() -> Boot {
    let inbox = Inbox::default();
    (Box::new(Mock::new(inbox.clone())), Spec::bundled(), inbox, false)
}

macro_rules! menu_commands {
    ($($id:literal),* $(,)?) => {
        &[$(($id, (|s: &mut State, tx: &mut Tx| run_command(s, $id, tx)) as fn(&mut State, &mut Tx))),*]
    };
}

/// The commands the native menu bar carries (macOS; elsewhere the palette is the menu).
const MENU: &[(&str, fn(&mut State, &mut Tx))] = menu_commands![
    "box.new", "pane.newShell", "pane.newTerminal", "pane.end",
    "box.attach", "box.stop", "box.start", "box.upgrade", "box.repair", "box.archive", "box.unarchive",
    "view.home", "view.sidebar", "app.palette", "pane.maximize", "pane.next", "pane.prev", "box.next", "box.prev",
];

fn main() {
    let bridged = std::env::args().any(|a| a == "--bridge");
    let boot = if bridged {
        let inbox = Inbox::default();
        match Bridge::connect(inbox.clone()) {
            Ok((bridge, spec)) => (Box::new(bridge) as Box<dyn Backend + Send>, spec, inbox, true),
            Err(e) => {
                eprintln!("discobox desktop: {e}");
                std::process::exit(1);
            }
        }
    } else {
        boot_mock()
    };
    let spec = boot.1.clone();
    *BOOT.lock().unwrap() = Some(boot);

    let mut app = App::new(AppSpec::<State> {
        min_size: Some((760.0, 480.0)),
        scenes: &[
            ("workspace", |s| {
                s.open_box(0);
                s.new_session(true);
            }),
            ("palette", |s| {
                s.open_box(0);
                s.palette = Some(Palette { query: TextEdit::new(""), picked: 0 });
            }),
        ],
        setup: Some(setup),
        poll: Some(poll),
        on_key: Some(on_key),
        ..AppSpec::new("discobox", (1440.0, 900.0), State::new, view::view)
    });
    for &(id, action) in MENU {
        let Some(c) = spec.command(id) else { continue };
        let mut item = MenuItem::new(c.title.clone(), action).id(id).menu(match c.group.as_str() {
            "Session" => "Shell",
            "App" => "discobox",
            g => g,
        });
        if let Some(sc) = c.keys.get("desktop").and_then(|k| k.first()).and_then(|k| keys::menu_shortcut(k)) {
            item = item.key_equivalent(sc);
        }
        app = app.menu_item(item);
    }
    app.run();
}
