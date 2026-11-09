//! `discobox desktop`: the discobox console as a native app, built with Rune.
//!
//! The CLI runs this with `--bridge` and serves the console core over stdin and stdout
//! (`cli/internal/console/bridge`), with the core's spec (`cli/internal/console/spec.go`):
//! colours, glyphs and the commands. Every command is the core's to rule on and run; the app
//! enters the flows that are its own. The layout is the TUI's; the interaction is a desktop
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

use backend::{Backend, Bridge, Event, Inbox, Mock, Out};
use discobox_bridge::wire;
use wire::{Args, Box as Sandbox, Command, Context, Entry, Result as CommandResult, Sessions, Spec, SpecTarget, Status, StreamState, Tab, TabKind, Topic};
use rune::{App, AppSpec, KeyEvent, MenuItem, TextEdit, Tx, Waker};
use term::Screen;

/// Which of the two columns a pane lives in: harness terminals left, shells right.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Side {
    Terminals,
    Shells,
}

pub struct Pane {
    pub stream: u32,
    pub exec: Tab,
    pub screen: Screen,
    pub exited: bool,
    /// Reconnecting, or why it ended.
    pub status: Option<String>,
}

impl Pane {
    pub fn side(&self) -> Side { if matches!(self.exec.kind, TabKind::Shell | TabKind::Tool) { Side::Shells } else { Side::Terminals } }
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
    /// Why its sessions could not be read; the subscription keeps trying, and the next snapshot
    /// read clears it.
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

/// What a command was sent for, so its result lands in the right place.
enum Pending {
    /// A command whose flow, if the core lets it run, is the app's own.
    Enter(String),
    /// A session a shell or terminal makes, on its stream, in a box.
    Session(u32, String),
    /// A box's action menu, where it was asked for.
    Actions((f32, f32)),
    /// A verb, for the box it is about.
    Verb(String, String),
    /// Ending the session on a stream.
    End(u32),
}

pub struct Palette {
    pub query: TextEdit,
    pub picked: usize,
}

/// A box's actions menu, drawn in the window where the platform has no native one.
pub struct Menu {
    pub sandbox: String,
    pub at: (f32, f32),
    pub actions: Vec<Entry>,
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
    next_id: u64,
    next_sub: u32,
    next_stream: u32,
    /// Each box's sessions subscription, by its ID, and the last snapshot it sent: a snapshot
    /// is sent only when it changes, so one held back while a session was made is taken again.
    subs: HashMap<u32, String>,
    seen: HashMap<u32, Result<Sessions, String>>,
    /// What arrived on a stream before its pane was made: a shell's first output can come
    /// before its result.
    early: HashMap<u32, Vec<Event>>,
}

const DEFAULT_SIZE: (u16, u16) = (100, 30);

impl State {
    fn new() -> State {
        let (backend, spec, inbox, bridged) = BOOT.lock().unwrap().take().unwrap_or_else(boot_mock);
        theme::set_tokens(&spec.colors);
        let mut s = State {
            backend, inbox, spec, boxes: vec![], cursor: 0, workspaces: HashMap::new(), live: None, focus: Focus::List,
            sidebar: true, message: None, palette: None, menu: None, bridged, pending: HashMap::new(), next_id: 1,
            next_sub: 1, next_stream: 1, subs: HashMap::new(), seen: HashMap::new(), early: HashMap::new(),
        };
        s.subscribe(Topic::Listing, String::new());
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

    fn subscribe(&mut self, topic: Topic, sandbox: String) -> u32 {
        let sub = self.next_sub;
        self.next_sub += 1;
        if topic == Topic::Sessions {
            self.subs.insert(sub, sandbox.clone());
        }
        self.backend.send(Out::Subscribe { sub, topic, sandbox });
        sub
    }

    /// Where a command is asked from, for the core's rules: the box whose workspace is on
    /// screen, and whether a pane or the list has the keys. The list is always beside it.
    fn context(&self) -> Context {
        let shown = self.live.is_some();
        Context {
            workspace: self.live.clone().unwrap_or_default(),
            has_workspace: shown,
            workspace_shown: shown,
            pane_focused: shown && self.focus == Focus::Panes,
            list_has_keys: self.focus == Focus::List,
            tree: true,
            tree_shown: self.sidebar,
            tree_fits: true,
            page_shown: false,
        }
    }

    /// Send a command for the core to rule on and run; its result lands as `why` says.
    fn command(&mut self, id: &str, targets: Vec<String>, context: Context, args: Args, why: Pending) {
        let cid = self.next_id;
        self.next_id += 1;
        self.pending.insert(cid, why);
        self.backend.send(Out::Command(Command { id: cid, command: id.into(), targets, context, args }));
    }

    pub fn live_ws(&self) -> Option<&Workspace> { self.live.as_ref().and_then(|id| self.workspaces.get(id)) }

    fn live_ws_mut(&mut self) -> Option<&mut Workspace> {
        let id = self.live.clone()?;
        self.workspaces.get_mut(&id)
    }

    pub fn box_by_id(&self, id: &str) -> Option<&Sandbox> { self.boxes.iter().find(|b| b.key == id) }

    /// The box a box command acts on: the one on screen while its panes have the keys, else
    /// the list's cursor.
    fn target(&self) -> Option<Sandbox> {
        match (&self.live, self.focus) {
            (Some(id), Focus::Panes) => self.box_by_id(id).cloned(),
            _ => self.boxes.get(self.cursor).cloned(),
        }
    }

    fn take_listing(&mut self, boxes: Vec<Sandbox>) {
        let at = self.boxes.get(self.cursor).map(|b| b.key.clone());
        self.boxes = view::list_order(boxes);
        self.cursor = at.and_then(|id| self.boxes.iter().position(|b| b.key == id)).unwrap_or(self.cursor.min(self.boxes.len().saturating_sub(1)));
    }

    fn new_pane(&mut self, exec: Tab) -> Pane {
        let stream = self.next_stream;
        self.next_stream += 1;
        Pane { stream, exec, screen: Screen::new(DEFAULT_SIZE.0 as usize, DEFAULT_SIZE.1 as usize), exited: false, status: None }
    }

    /// Show a box: its workspace if it has one, attaching on first use. A box that is not
    /// running is only pointed at — starting one is a command of its own.
    pub fn open_box(&mut self, index: usize) {
        let Some(sbx) = self.boxes.get(index).cloned() else { return };
        self.cursor = index;
        if !sbx.running() && !self.workspaces.contains_key(&sbx.key) {
            self.message = Some(format!("{} is {} — start it from its menu", sbx.name, sbx.state.as_str()));
            return;
        }
        self.ensure_workspace(&sbx.key);
        self.live = Some(sbx.key);
        self.focus = Focus::Panes;
    }

    /// A box's workspace, made and subscribed to its sessions on first use.
    fn ensure_workspace(&mut self, sandbox: &str) {
        if !self.workspaces.contains_key(sandbox) {
            self.workspaces.insert(sandbox.to_string(), Workspace {
                sandbox: sandbox.to_string(), panes: vec![], active: [None, None], focus: Side::Terminals, maximized: false, opening: true, error: None,
            });
            self.subscribe(Topic::Sessions, sandbox.to_string());
        }
    }

    /// Ask the core for a session in a box — a shell, or another of its harness's terminals —
    /// on a stream allocated for its pane. The pane is made when the result says what it is.
    fn new_session(&mut self, shell: bool) {
        let (cmd, targets, sandbox) = if shell {
            let Some(b) = self.target() else { return };
            ("shell", vec![b.key.clone()], b.key)
        } else {
            ("terminal", vec![], self.live.clone().unwrap_or_default())
        };
        let stream = self.next_stream;
        self.next_stream += 1;
        let (cols, rows) = DEFAULT_SIZE;
        let context = self.context();
        self.command(cmd, targets, context, Args { stream, cols: cols.into(), rows: rows.into(), exec: String::new() }, Pending::Session(stream, sandbox));
    }

    fn end_focused(&mut self) {
        let Some(id) = self.live.clone() else { return };
        let ws = &self.workspaces[&id];
        let Some(i) = ws.focused() else { return };
        let pane = &ws.panes[i];
        let args = Args { stream: pane.stream, exec: pane.exec.exec.clone(), ..Default::default() };
        let context = self.context();
        let stream = args.stream;
        self.command("end", vec![], context, args, Pending::End(stream));
    }

    /// Takes a pane away once its session is ended.
    fn drop_pane(&mut self, stream: u32) {
        for ws in self.workspaces.values_mut() {
            if let Some(i) = ws.panes.iter().position(|p| p.stream == stream) {
                ws.panes.remove(i);
                for a in ws.active.iter_mut() {
                    *a = a.and_then(|a| if a == i { None } else if a > i { Some(a - 1) } else { Some(a) });
                }
                ws.fix_active();
            }
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
        if self.boxes[self.cursor].running() || self.workspaces.contains_key(&self.boxes[self.cursor].key) {
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

    /// A box's action menu, from its row: the core's entries for it.
    pub fn open_menu(&mut self, sandbox: String, at: (f32, f32)) {
        let context = Context { list_has_keys: true, ..self.context() };
        self.command("actions", vec![sandbox], context, Args::default(), Pending::Actions(at));
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

    /// A sessions snapshot: a tab for every session the core opens that the workspace does not
    /// hold yet. Nothing is closed here; a stream says when it ends.
    fn take_sessions(&mut self, sub: u32, r: Result<Sessions, String>) {
        let Some(sbx) = self.subs.get(&sub).cloned() else { return };
        self.seen.insert(sub, r.clone());
        // A session still being made in this box is in no pane yet, and may already be listed:
        // while one is, only the primary opens, and the rest once it is made (retake_sessions).
        let making = self.pending.values().any(|p| matches!(p, Pending::Session(_, b) if *b == sbx));
        let Some(ws) = self.workspaces.get_mut(&sbx) else { return };
        ws.opening = false;
        let sessions = match r {
            Ok(s) => {
                ws.error = None;
                s.sessions
            }
            Err(e) => {
                ws.error = Some(e);
                return;
            }
        };
        let fresh: Vec<Tab> = sessions.into_iter()
            .filter(|e| !ws.panes.iter().any(|p| p.exec.exec == e.exec) && (!making || e.kind == TabKind::Primary))
            .collect();
        let mut opens = vec![];
        for e in fresh {
            let pane = self.new_pane(e);
            opens.push((pane.exec.exec.clone(), pane.stream));
            self.workspaces.get_mut(&sbx).unwrap().panes.push(pane);
        }
        self.workspaces.get_mut(&sbx).unwrap().fix_active();
        for (exec, stream) in opens {
            let (cols, rows) = DEFAULT_SIZE;
            self.backend.send(Out::Open { stream, sandbox: sbx.clone(), exec, cols, rows });
        }
    }

    /// Takes a box's last sessions snapshot again, once a session made there has a pane or
    /// failed: what it held back opens now.
    fn retake_sessions(&mut self, sandbox: &str) {
        let subs: Vec<u32> = self.subs.iter().filter(|(_, b)| *b == sandbox).map(|(s, _)| *s).collect();
        for sub in subs {
            if let Some(r) = self.seen.get(&sub).cloned() {
                self.take_sessions(sub, r);
            }
        }
    }

    fn take_result(&mut self, r: CommandResult, tx: &mut Tx) {
        let Some(why) = self.pending.remove(&r.id) else { return };
        let failed = r.status == Status::Failed;
        match why {
            Pending::Enter(id) if r.status == Status::Enter => enter(self, &id, &r, tx),
            Pending::End(stream) if r.status == Status::Done => self.drop_pane(stream),
            Pending::Session(stream, sandbox) if r.status == Status::Done => {
                let Some(exec) = r.session.clone() else { return };
                self.ensure_workspace(&r.r#box);
                let pane = Pane { stream, exec, screen: Screen::new(DEFAULT_SIZE.0 as usize, DEFAULT_SIZE.1 as usize), exited: false, status: None };
                let ws = self.workspaces.get_mut(&r.r#box).unwrap();
                ws.panes.push(pane);
                ws.show(ws.panes.len() - 1);
                self.live = Some(r.r#box.clone());
                self.focus = Focus::Panes;
                for e in self.early.remove(&stream).unwrap_or_default() {
                    self.take_stream_event(e);
                }
                self.resend_size(stream);
                self.retake_sessions(&sandbox);
            }
            Pending::Session(stream, sandbox) => {
                self.early.remove(&stream);
                self.retake_sessions(&sandbox);
            }
            Pending::Actions(at) if r.status == Status::Enter => overlay::show_menu(self, r.r#box.clone(), at, r.entries.clone(), tx),
            Pending::Verb(verb, name) if !failed => {
                self.message = Some(if r.note.is_empty() { format!("{verb}: {name}") } else { r.note.clone() });
            }
            _ => {}
        }
        if failed {
            self.message = Some(r.why);
        }
    }

    /// Output or an end on a stream: to its pane, or kept until the pane is made.
    fn take_stream_event(&mut self, e: Event) {
        let stream = match &e {
            Event::Data(stream, _) | Event::Exited(stream, _) => *stream,
            _ => return,
        };
        let Some(p) = self.pane_mut(stream) else {
            if self.pending.values().any(|p| matches!(p, Pending::Session(s, _) if *s == stream)) {
                self.early.entry(stream).or_default().push(e);
            }
            return;
        };
        match e {
            Event::Data(_, bytes) => {
                let reply = p.screen.feed(&bytes);
                if !reply.is_empty() {
                    self.backend.input(stream, &reply);
                }
            }
            Event::Exited(_, msg) => {
                p.exited = true;
                p.status = (!msg.is_empty()).then_some(msg);
            }
            _ => {}
        }
    }
}

/// The registry's commands this app answers. Every other one says it is not here yet, and has
/// an entry in the parity ledger (`docs/console/parity.md`) saying which issue brings it.
pub const ANSWERED: &[&str] = &[
    "attach", "shell", "upgrade", "repair", "stop", "start", "archive", "unarchive",
    "quit", "palette", "terminal", "end", "maximize", "detach",
    "next pane", "previous pane", "next box", "previous box", "sidebar",
];

/// Every command, by its registry ID. The keys, the palette, the menus and a row's actions all
/// come here, and go to the core, which rules on it and runs it; a flow that is the app's own is
/// entered when the core says it can run (`enter`). A command the app does not answer yet says
/// so, and is not sent.
pub fn run_command(s: &mut State, id: &str, _tx: &mut Tx) {
    s.message = None;
    s.palette = None;
    s.menu = None;
    if !ANSWERED.contains(&id) {
        s.message = Some(match s.spec.command(id) {
            Some(c) if matches!(id, "new" | "rename" | "settings" | "purge" | "review config") => {
                format!("{} needs a dialog, which the desktop app does not have yet", c.label)
            }
            Some(c) => format!("{} is not in the desktop app yet", c.label),
            None => format!("unknown command {id}"),
        });
        return;
    }
    let context = s.context();
    match id {
        "shell" => s.new_session(true),
        "terminal" => s.new_session(false),
        "end" => s.end_focused(),
        "stop" | "start" | "upgrade" | "repair" | "archive" | "unarchive" => {
            if let Some(b) = s.target() {
                let doing = match id {
                    "stop" => "stopping",
                    "start" => "starting",
                    "upgrade" => "upgrading",
                    "repair" => "repairing",
                    "archive" => "archiving",
                    _ => "unarchiving",
                };
                s.message = Some(format!("{doing} {}…", b.name));
                s.command(id, vec![b.key], context, Args::default(), Pending::Verb(id.into(), b.name.clone()));
            }
        }
        _ => {
            let targets = match s.spec.command(id) {
                Some(c) if c.target == SpecTarget::Box => s.target().map(|b| vec![b.key]).unwrap_or_default(),
                _ => vec![],
            };
            s.command(id, targets, context, Args::default(), Pending::Enter(id.into()));
        }
    }
}

/// A command's flow that is the app's own, once the core has ruled it can run.
fn enter(s: &mut State, id: &str, r: &CommandResult, tx: &mut Tx) {
    match id {
        // The core has ruled the box can be attached, whatever its state: its primary is
        // what waits for it.
        "attach" => {
            if let Some(i) = s.boxes.iter().position(|b| b.key == r.r#box) {
                s.cursor = i;
            }
            s.ensure_workspace(&r.r#box);
            s.live = Some(r.r#box.clone());
            s.focus = Focus::Panes;
        }
        "maximize" => {
            if let Some(ws) = s.live_ws_mut() {
                ws.maximized = !ws.maximized;
            }
        }
        "detach" => {
            s.live = None;
            s.focus = Focus::List;
        }
        "palette" => s.palette = Some(Palette { query: { let mut q = TextEdit::new(""); q.focused = true; q }, picked: 0 }),
        "quit" => tx.quit(),
        "next pane" => s.step_pane(1),
        "previous pane" => s.step_pane(-1),
        "next box" => s.step_box(1),
        "previous box" => s.step_box(-1),
        "sidebar" => {
            s.sidebar = !s.sidebar;
            if !s.sidebar {
                s.focus = Focus::Panes;
            }
        }
        _ => {}
    }
}

fn setup(s: &mut State, waker: Waker) { s.inbox.set_waker(waker); }

/// Drain what the backend sent into the window.
fn poll(s: &mut State, _t: f64, tx: &mut Tx) -> bool {
    let events = s.inbox.drain();
    if events.is_empty() {
        return false;
    }
    for e in events {
        match e {
            Event::Listing(boxes) => s.take_listing(boxes),
            Event::Sessions(sub, r) => s.take_sessions(sub, r),
            Event::Result(r) => s.take_result(r, tx),
            Event::Stream(st) => match st.state {
                StreamState::Attached => s.resend_size(st.stream),
                StreamState::Failed => {
                    if let Some(p) = s.pane_mut(st.stream) {
                        p.exited = true;
                        p.status = Some(st.why);
                    }
                }
                state => {
                    if let Some(p) = s.pane_mut(st.stream) {
                        p.status = (state == StreamState::Reconnecting).then(|| "reconnecting…".into());
                    }
                }
            },
            // The app draws no questions or forms yet (#808): each is left unanswered, and says so.
            Event::Question(q) => {
                s.backend.send(Out::Decline(q.id));
                s.message = Some(format!("{}: the desktop app cannot answer this yet", q.title));
            }
            Event::Form(f) => {
                s.backend.send(Out::Dismiss(f.id));
                s.message = Some(format!("{} needs a dialog, which the desktop app does not have yet", f.title));
            }
            Event::Progress(p) => {
                if !p.text.is_empty() {
                    s.message = Some(p.text);
                }
            }
            e @ (Event::Data(..) | Event::Exited(..)) => s.take_stream_event(e),
        }
    }
    true
}

fn on_key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool { keys::on_key(s, ev, tx) }

type Boot = (Box<dyn Backend + Send>, Spec, Inbox, bool);

/// What `main` connected, for `State::new` (an `AppSpec` init takes no arguments).
static BOOT: Mutex<Option<Boot>> = Mutex::new(None);

fn boot_mock() -> Boot {
    let inbox = Inbox::default();
    (Box::new(Mock::new(inbox.clone())), backend::bundled_spec(), inbox, false)
}

macro_rules! menu_commands {
    ($(($menu:literal, $id:literal)),* $(,)?) => {
        &[$(($menu, $id, (|s: &mut State, tx: &mut Tx| run_command(s, $id, tx)) as fn(&mut State, &mut Tx))),*]
    };
}

/// The commands the native menu bar carries, by the menu they are in (macOS; elsewhere the
/// palette is the menu).
const MENU: &[(&str, &str, fn(&mut State, &mut Tx))] = menu_commands![
    ("Box", "new"), ("Shell", "shell"), ("Shell", "terminal"), ("Shell", "end"),
    ("Box", "attach"), ("Box", "stop"), ("Box", "start"), ("Box", "upgrade"), ("Box", "repair"), ("Box", "archive"), ("Box", "unarchive"),
    ("View", "detach"), ("View", "sidebar"), ("View", "palette"), ("View", "maximize"),
    ("View", "next pane"), ("View", "previous pane"), ("View", "next box"), ("View", "previous box"),
];

/// A label as a menu's title: its first letter capitalised.
fn title_case(label: &str) -> String {
    let mut c = label.chars();
    c.next().map(|f| f.to_uppercase().chain(c).collect()).unwrap_or_default()
}

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
            // A scene is one frame: what the mock answered is taken before it is drawn.
            ("workspace", |s| {
                s.open_box(0);
                s.new_session(true);
                poll(s, 0.0, &mut Tx::default());
            }),
            ("palette", |s| {
                s.open_box(0);
                poll(s, 0.0, &mut Tx::default());
                s.palette = Some(Palette { query: TextEdit::new(""), picked: 0 });
            }),
        ],
        setup: Some(setup),
        poll: Some(poll),
        on_key: Some(on_key),
        ..AppSpec::new("discobox", (1440.0, 900.0), State::new, view::view)
    });
    for &(menu, id, action) in MENU {
        let Some(label) = keys::label_of(&spec, id) else { continue };
        let mut item = MenuItem::new(title_case(&label), action).id(id).menu(menu);
        if let Some(sc) = keys::bindings(&spec, id).first().and_then(|k| keys::menu_shortcut(k)) {
            item = item.key_equivalent(sc);
        }
        app = app.menu_item(item);
    }
    app.run();
}
