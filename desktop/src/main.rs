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
#[cfg(test)]
mod command_tests;
#[cfg(test)]
mod create_tests;
mod harnesses;
#[cfg(test)]
mod harness_tests;
mod icons;
#[cfg(test)]
mod identity_tests;
mod keys;
mod mark;
mod overlay;
#[cfg(test)]
mod parity_tests;
#[cfg(test)]
mod service_tests;
mod sheet;
#[cfg(test)]
mod sheet_tests;
#[cfg(test)]
mod surface_tests;
mod term;
mod theme;
#[cfg(test)]
mod tool_tests;
mod tools;
mod view;
#[cfg(test)]
mod workspace_tests;

use std::collections::{HashMap, HashSet};
use std::sync::Mutex;

use backend::{Backend, Bridge, Event, Inbox, Mock, Out};
use discobox_bridge::wire;
use wire::{
    Address, Args, Box as Sandbox, Chip, Command, Composer, Menu as DesktopMenu, Commands, Context, Entry, Filter, Follow, Header, Listing, Note,
    Palette as Found, PaletteQuery, PaletteRow, Provisioning, Result as CommandResult, Row, Service, ServiceVerb, Sessions, Spec, SpecTarget,
    Status, Stream, StreamState, Subscribe, Tab, TabEnd, TabKind, Tool, Topic,
};
use rune::{App, AppSpec, Drag, Ime, KeyEvent, MenuItem, Mods, Phase, TextEdit, Tx, Waker};
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
    /// Its program ended and the tab is held with its last screen (the core's `Tab.End`), or its
    /// stream is gone: nothing more is sent to it.
    pub exited: bool,
    /// What the tab says over its program: reconnecting, or what its ending left it saying.
    pub status: Option<String>,
}

impl Pane {
    fn new(stream: u32, exec: Tab) -> Pane {
        Pane { stream, exec, screen: Screen::new(DEFAULT_SIZE.0 as usize, DEFAULT_SIZE.1 as usize), exited: false, status: None }
    }

    pub fn side(&self) -> Side { if matches!(self.exec.kind, TabKind::Shell | TabKind::Tool) { Side::Shells } else { Side::Terminals } }

    /// Whether it draws one of the box's declared services: read-only, stopped rather than ended,
    /// and numbered apart from the other tabs.
    pub fn service(&self) -> bool { self.exec.kind == TabKind::Service }

    /// Whether what it says over its screen is quiet: a held tab's last word, or a service's
    /// state over its card, rather than a stream reconnecting.
    pub fn settled(&self) -> bool { self.exited || (self.service() && !self.exec.live) }
}

/// One box's sessions as the window holds them. Switching away parks it (the core's
/// `Workspace.Park`): its streams stay attached and its screens keep emulating, so switching
/// back shows where it is now, but it polls nothing and its ports are no longer forwarded —
/// both go with the screen, and come back with it.
pub struct Workspace {
    pub sandbox: String,
    /// Its tabs, each column in the order its chips are drawn: the order sessions arrived in,
    /// until a chip is dragged elsewhere in its column.
    pub panes: Vec<Pane>,
    /// The visible pane of each column, as an index into `panes`.
    pub active: [Option<usize>; 2],
    pub focus: Side,
    pub maximized: bool,
    pub opening: bool,
    /// Why its sessions could not be read; the subscription keeps trying, and the next snapshot
    /// read clears it.
    pub error: Option<String>,
    /// Its sessions and header subscriptions while it is on screen; none while it is parked.
    sessions_sub: u32,
    header_sub: u32,
    /// What the header says of the box, as the core said it last.
    pub header: Option<Header>,
    /// What a box that was not up when its workspace opened is being made to do (the
    /// provisioning topic), until it says nothing more; its subscription, while it follows.
    pub step: String,
    provisioning_sub: u32,
    /// The terminals column's share of the width the two columns have.
    pub split: f32,
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

    /// Every pane that wears a number, in tab order: all but the services, which are counted
    /// apart (the core's `Column.Number`), so one starting or stopping never renumbers the tab
    /// being reached for.
    pub fn numbered(&self) -> Vec<usize> { self.strip().into_iter().filter(|&i| !self.panes[i].service()).collect() }

    /// The services' tabs, in their column's order: the first wears 1.
    pub fn services(&self) -> Vec<usize> { self.column(Side::Terminals).into_iter().filter(|&i| self.panes[i].service()).collect() }

    /// Puts a tab that arrived where the core's order puts it: before the first tab of its column
    /// that `order` lists after it, else at its column's end. The tabs shown stay the same tabs.
    fn insert(&mut self, pane: Pane, order: &[String]) {
        let rank = |exec: &str| order.iter().position(|e| e == exec);
        let mine = rank(&pane.exec.exec);
        let side = pane.side();
        let at = self.panes.iter().position(|p| p.side() == side && matches!((mine, rank(&p.exec.exec)), (Some(m), Some(r)) if r > m));
        let shown = self.active.map(|a| a.map(|i| self.panes[i].stream));
        match at {
            Some(i) => self.panes.insert(i, pane),
            None => self.panes.push(pane),
        }
        self.active = shown.map(|s| s.and_then(|s| self.panes.iter().position(|p| p.stream == s)));
    }

    /// Whether it is worth keeping when switched away from (the core's `Workspace.Parkable`):
    /// one whose primary has not arrived has nothing to keep, and is let go instead.
    pub fn parkable(&self) -> bool { self.panes.iter().any(|p| p.exec.kind == TabKind::Primary) }

    /// Whether it is set aside, attached, while another box is on screen.
    pub fn parked(&self) -> bool { self.sessions_sub == 0 }

    /// Moves a tab to where another one in its column is, the tabs between stepping over: a chip
    /// dragged along its strip. The panes shown and the one with the keys stay the same panes.
    pub fn move_pane(&mut self, from: usize, to: usize) {
        if from == to || from >= self.panes.len() || to >= self.panes.len() || self.panes[from].side() != self.panes[to].side() {
            return;
        }
        let shown = self.active.map(|a| a.map(|i| self.panes[i].stream));
        let p = self.panes.remove(from);
        self.panes.insert(to, p);
        self.active = shown.map(|s| s.and_then(|s| self.panes.iter().position(|p| p.stream == s)));
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
    /// A session a shell or terminal makes, on its stream, in a box; and the box that was on
    /// screen when it was asked for.
    Session(u32, String, Option<String>),
    /// A box's action menu, where it was asked for.
    Actions((f32, f32)),
    /// A verb, for the box it is about.
    Verb(String, String),
    /// Ending the session on a stream.
    End(u32),
    /// The harnesses page, asked of a server (the list's when it was asked; the primary for "").
    Page(String),
    /// One of the harnesses page's actions, on the harness by its name.
    Harness(String),
    /// Saving or discarding a box's harness changes.
    Change,
    /// A create from the composer, and the prompt it was sent with: the composer lets go of it
    /// once the server has taken the create.
    Create(String),
    /// A link pressed in a pane: the URL it goes to opened, or the box it names gone to — only
    /// from the box that was on screen when it was pressed (`live` then), as the TUI drops an
    /// answer that took its time: a person who moved on is not sent back.
    Follow(Option<String>),
}

/// The focused pane's find through its scrollback, open: what is typed, on the pane's stream.
pub struct FindBar {
    pub query: TextEdit,
    pub stream: u32,
}

/// The palette (ADR 26-11-06-671): the query, at most one chip — a command, or a box by its key —
/// the scope, and what the core found for them, which the keys pick among. Every change asks the
/// core again (the palette topic); a row is kept picked by what it is, not where it is.
pub struct Palette {
    pub query: TextEdit,
    pub command: String,
    pub sandbox: String,
    pub archived: bool,
    pub rows: Vec<PaletteRow>,
    /// What the core says about the servers it searched: those with no rows yet, and those
    /// whose rows are behind.
    pub partial: Vec<String>,
    pub stale: Vec<Note>,
    /// Whether the core has answered the query asked last.
    pub found: bool,
    /// The picked row's identity (`row_id`), and the first row drawn.
    pub picked: String,
    pub offset: usize,
    /// Why the last choice did nothing, or why the query could not be answered.
    pub reason: Option<String>,
    sub: u32,
}

/// How many rows the palette draws at once, and a page of its keys.
pub const PALETTE_ROWS: usize = 12;

/// A palette row's identity: a box by its key, a command by its ID.
pub fn row_id(r: &PaletteRow) -> String {
    match &r.command {
        Some(c) if r.r#box.is_empty() => format!("command:{}", c.command),
        _ => format!("box:{}", r.r#box),
    }
}

impl Palette {
    fn new() -> Palette {
        let mut query = TextEdit::new("");
        query.focused = true;
        Palette {
            query, command: String::new(), sandbox: String::new(), archived: false, rows: vec![], partial: vec![], stale: vec![], found: false,
            picked: String::new(), offset: 0, reason: None, sub: 0,
        }
    }

    /// The picked row's place in the rows.
    pub fn at(&self) -> Option<usize> { self.rows.iter().position(|r| row_id(r) == self.picked) }

    /// Moves the pick by `by` rows, kept on the rows, and the rows drawn with it.
    pub fn step(&mut self, by: i64) {
        if self.rows.is_empty() {
            return;
        }
        let at = self.at().map_or(0, |i| i as i64);
        let to = (at + by).clamp(0, self.rows.len() as i64 - 1) as usize;
        self.pick(to);
    }

    pub fn pick(&mut self, i: usize) {
        let Some(r) = self.rows.get(i) else { return };
        self.picked = row_id(r);
        self.reason = None;
        if i < self.offset {
            self.offset = i;
        } else if i >= self.offset + PALETTE_ROWS {
            self.offset = i + 1 - PALETTE_ROWS;
        }
    }

    /// Replaces the query's text, the caret at its end.
    pub fn set_text(&mut self, text: &str) {
        self.query = TextEdit::new(text);
        self.query.focused = true;
    }
}

/// What a menu drawn in the window is.
#[derive(Clone, PartialEq, Debug)]
pub enum MenuKind {
    /// A box's actions, from its row or its header: the core's lifecycle entries for it.
    Actions(String),
    /// One of the menu bar's menus (the spec's `menus`, in their order).
    Bar(DesktopMenu),
    /// The focused pane's menu: the session commands.
    Pane,
    /// A box's services, by its key, as the services command read them: each entry a service by
    /// its ID.
    Services(String, Vec<Service>),
    /// One of a box's services: its output, then the core's verbs for it, each entry by
    /// `OUTPUT` or the verb.
    Service(String, Service),
    /// A harness's actions, by its ID: each entry an action as the core offers it.
    Harness(String),
    /// A harness's files, by its ID, to pick the one to edit: each entry a file by its path.
    HarnessFiles(String),
    /// A box's tools, by its key, and its addresses, as the tools command read them: each entry a
    /// tool to run, an address to copy, or a tool's files to edit (`tools::choose`).
    Tools(String, Vec<Tool>, Vec<Address>),
    /// One tool's files, in a box by its key, to pick the one to edit: each entry a file by its
    /// name.
    ToolFiles(String, Tool),
}

/// The service menu's entry that shows the service's output: its tab.
pub const OUTPUT: &str = "output";

/// The list's filter, open: the core's choices, and the one the keys are on.
pub struct FilterMenu {
    pub picked: usize,
}

/// A menu drawn in the window — a box's actions where the platform has no native one, a menu of
/// the menu bar, a pane's — and its entries as the core rules on them where it was opened: a box's
/// actions from the `actions` command, the others from the commands topic (`sub`) as it answers.
pub struct Menu {
    pub kind: MenuKind,
    pub at: (f32, f32),
    pub entries: Vec<Entry>,
    pub picked: usize,
    pub sub: u32,
}

pub struct State {
    backend: Box<dyn Backend + Send>,
    inbox: Inbox,
    pub spec: Spec,
    /// Every box listed, whatever the filter: what a box is looked up in.
    pub boxes: Vec<Sandbox>,
    /// The core's list: the rows the filter shows, in the order they are drawn, with their
    /// sections, nesting and remarks, the notes, the filter and its choices.
    pub listing: Listing,
    /// Whether a listing has landed; until one has, an empty list is a question, not an answer.
    pub loaded: bool,
    /// Why the listing could not be read last time; the last one stands.
    pub list_error: Option<String>,
    /// The row under the cursor, as an index into the listing's rows.
    pub cursor: usize,
    /// The rows marked to act on together, by box key.
    pub selected: HashSet<String>,
    /// Where a range being drawn started: the rows from it to the cursor are what a command
    /// acts on until it is folded into the selection or let go.
    pub range: Option<usize>,
    pub filter_menu: Option<FilterMenu>,
    /// The filter the app asked the listing for, until the first snapshot of it lands.
    pub asked: Option<Filter>,
    /// Where the filter control is drawn, which is where its menu opens.
    pub filter_at: (f32, f32),
    pub workspaces: HashMap<String, Workspace>,
    /// The box on screen; `None` is the list of every box.
    pub live: Option<String>,
    pub focus: Focus,
    pub sidebar: bool,
    pub message: Option<String>,
    pub palette: Option<Palette>,
    pub menu: Option<Menu>,
    /// Where each menu of the menu bar is drawn, which is where it opens.
    pub bar_at: HashMap<DesktopMenu, (f32, f32)>,
    /// The help: every command by its menu, with its shortcut.
    pub help: bool,
    /// The forms, questions and edits commands wait on, drawn as sheets one at a time.
    pub sheets: sheet::Sheets,
    /// The harnesses page, beside the list in place of the workspace while it is up.
    pub harnesses: Option<harnesses::HarnessPage>,
    /// A harness's config card, over the window.
    pub card: Option<harnesses::Card>,
    /// A box's harness changes under review, over the window.
    pub review: Option<harnesses::Review>,
    /// The boxes this window has gone to, latest first, which rank the palette's empty query
    /// (ADR 26-11-06-671 §6): at most `RECENT` of them, for as long as the window lasts.
    pub recent: Vec<String>,
    /// A box the palette went to that the filter hid, and the filter it stands in for: shown
    /// through a view that is never saved, and put back when the window leaves the box.
    pub reveal: Option<(String, Filter)>,
    /// A box to put the cursor on once a listing shows it: one the palette went to that the
    /// filter hid, until the view revealing it lands.
    point: Option<String>,
    pub find: Option<FindBar>,
    pub bridged: bool,
    pending: HashMap<u64, Pending>,
    /// The listing subscription: the filter is changed by subscribing again under a new one.
    list_sub: u32,
    next_id: u64,
    next_sub: u32,
    next_stream: u32,
    /// Each box's sessions subscription, by its ID, and the last snapshot it sent: a snapshot
    /// is sent only when it changes, so one held back while a session was made is taken again.
    subs: HashMap<u32, String>,
    seen: HashMap<u32, Result<Sessions, String>>,
    /// Each box's header subscription, by its ID.
    headers: HashMap<u32, String>,
    /// Where each tab chip was laid out, by its stream, in window points: what a chip dragged
    /// along its strip is over.
    pub chips: HashMap<u32, [f32; 4]>,
    /// The chip being dragged, by its stream, and where it was in its workspace's tabs when the
    /// drag began: a cancelled drag puts it back there.
    pub dragging: Option<u32>,
    drag_home: Option<usize>,
    /// How wide the two columns are together, which a drag of the split divides.
    pub columns_w: f32,
    /// What arrived on a stream before its pane was made: a shell's first output can come
    /// before its result.
    early: HashMap<u32, Vec<Event>>,
    /// The composer: the prompt a create from it gives the new box (Return creates, Shift+Return
    /// is a new line), and the line under it, which says what the create would be made with —
    /// the core's summary of the run options (the composer topic).
    pub composer: TextEdit,
    pub summary: Vec<Chip>,
    composer_sub: u32,
    /// The prompt as the CLI was last told it (the draft), and when it last changed if it has
    /// since: it is told again once it has been still a moment, and on the way out.
    draft: String,
    edited: Option<f64>,
    now: f64,
    /// A warning a running command gave (uncommitted changes left out, a settings file not read
    /// whole): it stays on the status line past the steps that follow, until the next command.
    warned: bool,
}

const DEFAULT_SIZE: (u16, u16) = (100, 30);

/// How many boxes gone to the palette ranks by (ADR 26-11-06-671 §6).
const RECENT: usize = 100;

/// How far a chip is dragged, in points, before it is moving rather than being pressed.
const DRAG_SLOP: f32 = 4.0;

/// The least share of the width either column keeps when the split is dragged.
pub const MIN_SPLIT: f32 = 0.2;
/// The cell a point on a screen is in, as a column, a row (negative above it), and whether it
/// is in the cell's right half: where a selection's edge falls.
fn cell_at(x: f32, y: f32) -> (usize, i32, bool) {
    let col = (x / theme::cell_w()).max(0.0);
    (col.floor() as usize, (y / theme::CELL_H).floor() as i32, col.fract() >= 0.5)
}

impl State {
    fn new() -> State { State::with(BOOT.lock().unwrap().take().unwrap_or_else(boot_mock)) }

    /// The window on what `main` connected, or on a mock a test made.
    fn with((backend, spec, inbox, bridged, draft): Boot) -> State {
        theme::set_tokens(&spec);
        let mut s = State {
            backend, inbox, spec, boxes: vec![], listing: Listing::default(), loaded: false, list_error: None, cursor: 0,
            selected: HashSet::new(), range: None, filter_menu: None, asked: None, filter_at: (24.0, 96.0), workspaces: HashMap::new(), live: None, focus: Focus::List,
            sidebar: true, message: None, palette: None, menu: None, bar_at: HashMap::new(), help: false, sheets: sheet::Sheets::default(), harnesses: None, card: None, review: None, recent: vec![], reveal: None, point: None, find: None,
            bridged, pending: HashMap::new(), list_sub: 0, next_id: 1,
            next_sub: 1, next_stream: 1, subs: HashMap::new(), seen: HashMap::new(), headers: HashMap::new(), chips: HashMap::new(),
            dragging: None, drag_home: None, columns_w: 0.0, early: HashMap::new(),
            composer: TextEdit::new(draft.clone()), summary: vec![], composer_sub: 0, draft: draft.clone(), edited: None, now: 0.0, warned: false,
        };
        // The prompt the last window left is back: closing a window is not how one is lost.
        if !draft.is_empty() {
            s.composer.focus = s.composer.text.len();
            s.composer.anchor = s.composer.focus;
            s.message = Some("the prompt you left is back".into());
        }
        s.composer_sub = s.subscribe(Subscribe { topic: Topic::Composer, ..Default::default() });
        s.follow_listing(None, String::new());
        if !bridged {
            // The mock answers at once; taking it here lets a headless scene open a box, and draw
            // the composer's line.
            for e in s.inbox.drain() {
                match e {
                    Event::Listing(sub, l) => s.take_listing(sub, l),
                    Event::Composer(sub, c) => s.take_composer(sub, c),
                    _ => {}
                }
            }
        }
        s
    }

    /// Follows a topic under a new subscription ID, which is returned.
    fn subscribe(&mut self, mut sub: Subscribe) -> u32 {
        sub.sub = self.next_sub;
        self.next_sub += 1;
        match sub.topic {
            Topic::Sessions => {
                self.subs.insert(sub.sub, sub.r#box.clone());
            }
            Topic::Header => {
                self.headers.insert(sub.sub, sub.r#box.clone());
            }
            _ => {}
        }
        let id = sub.sub;
        self.backend.send(Out::Subscribe(sub));
        id
    }

    /// Ends a subscription, if there is one.
    pub fn unsubscribe(&mut self, sub: u32) {
        if sub != 0 {
            self.backend.send(Out::Unsubscribe(sub));
        }
    }

    /// Follows the listing narrowed to `filter` (none: what this machine's last window left), or
    /// revealing a box, in place of the subscription there was: what that one still sends is
    /// dropped.
    fn follow_listing(&mut self, filter: Option<Filter>, reveal: String) {
        self.unsubscribe(self.list_sub);
        self.list_sub = self.subscribe(Subscribe { topic: Topic::Listing, filter, reveal, ..Default::default() });
    }

    /// Narrows the list to another filter, from the core's choices or the archived toggle. The
    /// rows under the cursor and the marks are another set now, so both start again. A box the
    /// palette revealed is let go: this filter is the person's own.
    pub fn set_filter(&mut self, filter: Filter) {
        self.reveal = None;
        self.cursor = 0;
        self.selected.clear();
        self.range = None;
        self.asked = Some(filter.clone());
        self.follow_listing(Some(filter), String::new());
    }

    /// Shows a box the filter hides, through a view that is never saved over the filter
    /// (ADR 26-11-06-671 §5); the filter comes back when the window leaves it (`unreveal`).
    fn reveal(&mut self, key: &str) {
        let Some(saved) = self.reveal.as_ref().map(|(_, f)| f.clone()).or_else(|| self.filter()) else { return };
        self.reveal = Some((key.to_string(), saved.clone()));
        self.asked = Some(saved.clone());
        self.selected.clear();
        self.range = None;
        self.message = Some("showing the palette's box outside your filter; the filter comes back when you leave it".into());
        self.follow_listing(Some(saved), key.to_string());
    }

    /// Puts back the filter a revealed box stood in for, unless `key` is that box.
    fn unreveal(&mut self, key: Option<&str>) {
        let Some((at, saved)) = self.reveal.clone() else { return };
        if key == Some(at.as_str()) {
            return;
        }
        self.reveal = None;
        self.asked = Some(saved.clone());
        self.follow_listing(Some(saved), String::new());
    }

    /// Notes a box gone to, for the palette's ranking.
    fn visited(&mut self, key: &str) {
        self.recent.retain(|k| k != key);
        self.recent.insert(0, key.to_string());
        self.recent.truncate(RECENT);
    }

    /// Opens the palette, empty, and asks the core what it finds.
    fn open_palette(&mut self) {
        self.close_overlays();
        self.palette = Some(Palette::new());
        self.ask_palette();
    }

    /// Asks the core what the palette's query finds, in place of what it asked before.
    pub fn ask_palette(&mut self) {
        let Some(p) = self.palette.as_ref() else { return };
        let current = match &self.live {
            Some(k) => k.clone(),
            None => self.listing.rows.get(self.cursor).map(|r| r.r#box.clone()).unwrap_or_default(),
        };
        let query = PaletteQuery {
            text: p.query.text.clone(), command: p.command.clone(), r#box: p.sandbox.clone(), archived: p.archived, current,
            recent: self.recent.clone(),
        };
        let old = p.sub;
        self.unsubscribe(old);
        let context = Some(self.context());
        let sub = self.subscribe(Subscribe { topic: Topic::Palette, context, palette: Some(query), ..Default::default() });
        let p = self.palette.as_mut().unwrap();
        p.sub = sub;
        p.found = false;
    }

    /// What the core found for the palette's query: the rows, the scope, and the chip the text
    /// committed as it was typed. The pick stays on its row while it is there.
    fn take_palette(&mut self, sub: u32, r: Result<Found, String>) {
        let Some(p) = self.palette.as_mut() else { return };
        if sub != p.sub {
            return;
        }
        let found = match r {
            Ok(f) => f,
            Err(e) => {
                p.reason = Some(e);
                return;
            }
        };
        if let Some(c) = found.commit {
            p.command = c.command;
            p.set_text(&c.text);
        }
        p.archived = found.archived;
        p.rows = found.rows;
        p.partial = found.partial;
        p.stale = found.stale;
        p.found = true;
        match p.at() {
            Some(i) => p.pick(i),
            None => {
                p.offset = 0;
                p.picked = p.rows.first().map(row_id).unwrap_or_default();
            }
        }
    }

    /// Closes the palette, ending what it follows.
    pub fn close_palette(&mut self) {
        if let Some(p) = self.palette.take() {
            self.unsubscribe(p.sub);
        }
    }

    /// Closes the palette, the menus and the help, ending what they follow.
    pub fn close_overlays(&mut self) {
        self.close_palette();
        if let Some(m) = self.menu.take() {
            self.unsubscribe(m.sub);
        }
        self.filter_menu = None;
        self.help = false;
        self.close_find();
    }

    /// Opens a menu of the menu bar, or the pane's menu, at a place: its commands from the spec,
    /// then as the core rules on them where it was opened.
    pub fn open_commands_menu(&mut self, kind: MenuKind, at: (f32, f32)) {
        self.close_overlays();
        let ids: Vec<String> = match &kind {
            MenuKind::Bar(menu) => self.in_menu(*menu),
            MenuKind::Pane => self.in_menu(DesktopMenu::Session),
            MenuKind::Actions(_) | MenuKind::Services(..) | MenuKind::Service(..) | MenuKind::Harness(_) | MenuKind::HarnessFiles(_)
            | MenuKind::Tools(..) | MenuKind::ToolFiles(..) => return,
        };
        let entries = ids.iter().filter_map(|id| self.spec.command(id)).map(|c| {
            let why = lacking(&self.spec, &c.id).unwrap_or_default();
            Entry { command: c.id.clone(), label: c.label.clone(), detail: c.detail.clone(), enabled: why.is_empty(), why, uses: 0 }
        }).collect();
        let context = Some(self.context());
        let targets = self.targets();
        let sub = self.subscribe(Subscribe { topic: Topic::Commands, context, targets, ..Default::default() });
        self.menu = Some(Menu { kind, at, entries, picked: 0, sub });
    }

    /// The commands of one menu, by ID, in the registry's order.
    pub fn in_menu(&self, menu: DesktopMenu) -> Vec<String> {
        self.spec.commands.iter().filter(|c| c.bindings.desktop.menu == Some(menu)).map(|c| c.id.clone()).collect()
    }

    /// The core's commands as they stand where a menu was opened, each time it rules again: an
    /// entry it lists takes its ruling, either way, and one the app does not answer stays as the
    /// app says.
    fn take_commands(&mut self, sub: u32, r: Result<Commands, String>) {
        let Some(m) = self.menu.as_mut().filter(|m| m.sub == sub) else { return };
        let Ok(c) = r else { return };
        for e in m.entries.iter_mut().filter(|e| lacking(&self.spec, &e.command).is_none()) {
            if let Some(ruled) = c.entries.iter().find(|x| x.command == e.command) {
                e.enabled = ruled.enabled;
                e.why = ruled.why.clone();
            }
        }
    }

    /// The filter the list is on: the one asked for while it is being read, else the last
    /// listing's. None before any listing has landed — there is no filter to change yet, and
    /// one changed from nothing would be saved over the one this machine's windows open on.
    pub fn filter(&self) -> Option<Filter> {
        if let Some((_, saved)) = &self.reveal {
            return Some(saved.clone());
        }
        self.asked.clone().or_else(|| self.loaded.then(|| self.listing.filter.clone()))
    }

    /// The rows the filter shows, as drawn.
    pub fn rows(&self) -> &[Row] { &self.listing.rows }

    /// The box on a row.
    pub fn row_box(&self, i: usize) -> Option<&Sandbox> { self.listing.rows.get(i).and_then(|r| self.box_by_id(&r.r#box)) }

    /// The row a box is on, if the filter shows it.
    pub fn row_of(&self, key: &str) -> Option<usize> { self.listing.rows.iter().position(|r| r.r#box == key) }

    /// The span of a range being drawn, from its start to the cursor.
    fn range_span(&self) -> Option<(usize, usize)> { self.range.map(|a| (a.min(self.cursor), a.max(self.cursor))) }

    /// Whether a row is among what a command acts on: marked, or inside the range being drawn.
    pub fn is_selected(&self, i: usize) -> bool {
        self.range_span().is_some_and(|(lo, hi)| (lo..=hi).contains(&i))
            || self.listing.rows.get(i).is_some_and(|r| self.selected.contains(&r.r#box))
    }

    /// What the list's command acts on, in row order: the range being drawn, else the rows
    /// marked, else the row under the cursor — as the TUI's list answers it.
    pub fn list_targets(&self) -> Vec<String> {
        let rows = &self.listing.rows;
        if let Some((lo, hi)) = self.range_span() {
            return rows[lo..=hi.min(rows.len().saturating_sub(1))].iter().map(|r| r.r#box.clone()).collect();
        }
        let marked: Vec<String> = rows.iter().filter(|r| self.selected.contains(&r.r#box)).map(|r| r.r#box.clone()).collect();
        if !marked.is_empty() {
            return marked;
        }
        rows.get(self.cursor).map(|r| vec![r.r#box.clone()]).unwrap_or_default()
    }

    /// Marks the row under the cursor, or lets it go; with a range being drawn, folds the range
    /// into the marks instead (the select gesture).
    pub fn toggle_select(&mut self) {
        if let Some((lo, hi)) = self.range_span() {
            for r in &self.listing.rows[lo..=hi.min(self.listing.rows.len().saturating_sub(1))] {
                self.selected.insert(r.r#box.clone());
            }
            self.range = None;
            return;
        }
        let Some(key) = self.listing.rows.get(self.cursor).map(|r| r.r#box.clone()) else { return };
        if !self.selected.remove(&key) {
            self.selected.insert(key);
        }
    }

    /// Starts a range at the cursor, or lets the one being drawn go (the range gesture).
    pub fn toggle_range(&mut self) {
        self.range = match self.range {
            Some(_) => None,
            None if self.listing.rows.is_empty() => None,
            None => Some(self.cursor),
        };
    }

    pub fn clear_selection(&mut self) {
        self.selected.clear();
        self.range = None;
    }

    /// A press on a row, as a desktop list takes one: on its own it moves the cursor there and
    /// lets the marks go; with ⌘ (Ctrl elsewhere) it marks the row or lets it go; with Shift it
    /// marks every row from the last one pressed to this one.
    pub fn press_row(&mut self, i: usize, mods: Mods) {
        if i >= self.listing.rows.len() {
            return;
        }
        self.focus = Focus::List;
        if mods.shift {
            let from = self.range.unwrap_or(self.cursor);
            let (lo, hi) = (from.min(i), from.max(i));
            for r in &self.listing.rows[lo..=hi] {
                self.selected.insert(r.r#box.clone());
            }
            self.range = None;
            self.cursor = i;
            return;
        }
        self.range = None;
        self.cursor = i;
        if mods.cmd {
            self.toggle_select();
            return;
        }
        self.selected.clear();
        // A press on a running box beside a workspace switches to it, as the TUI's tree does.
        if self.live.is_some() && self.row_box(i).is_some_and(Sandbox::running) {
            self.open_row(i);
        }
    }

    /// Moves the cursor (the list's gestures); a range being drawn follows it.
    pub fn move_cursor(&mut self, to: usize) { self.cursor = to.min(self.listing.rows.len().saturating_sub(1)); }

    /// Where a command is asked from, for the core's rules: the box whose workspace is on
    /// screen, and whether a pane or the list has the keys. The list is always beside it.
    /// Whether the box on screen's workspace is shown: one is live and no page covers it. What
    /// a workspace not shown does — its program's copies included — reaches nobody.
    pub fn workspace_shown(&self) -> bool { self.live.is_some() && self.harnesses.is_none() }

    /// Whether the focused pane has the keys and the input method: its workspace is shown, the
    /// panes have the focus, and nothing in front of it — a sheet, a menu, the palette, help, a
    /// harness's card or review — takes them first. The key path and the input method both ask
    /// this, so a surface added in front of the panes cannot be missed by one of them.
    pub fn pane_has_keys(&self) -> bool {
        self.workspace_shown()
            && self.focus == Focus::Panes
            && self.live_ws().is_some_and(|w| !w.panes.is_empty())
            && !self.sheets.is_open()
            && self.menu.is_none()
            && self.filter_menu.is_none()
            && self.palette.is_none()
            && !self.help
            && self.card.is_none()
            && self.review.is_none()
    }

    fn context(&self) -> Context {
        let shown = self.live.is_some();
        Context {
            workspace: self.live.clone().unwrap_or_default(),
            has_workspace: shown,
            // A page beside the list covers the workspace: it is held, not on screen.
            workspace_shown: self.workspace_shown(),
            pane_focused: shown && self.focus == Focus::Panes && self.harnesses.is_none(),
            list_has_keys: self.focus == Focus::List,
            tree: true,
            tree_shown: self.sidebar,
            tree_fits: true,
            page_shown: self.harnesses.is_some(),
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
            (Some(id), Focus::Panes) if self.harnesses.is_none() => self.box_by_id(id).cloned(),
            _ => self.row_box(self.cursor).cloned(),
        }
    }

    /// The boxes a box command acts on, by key: the one on screen while its panes have the
    /// keys, else what the list's selection says.
    fn targets(&self) -> Vec<String> {
        match (&self.live, self.focus) {
            (Some(id), Focus::Panes) if self.harnesses.is_none() => vec![id.clone()],
            _ => self.list_targets(),
        }
    }

    /// A listing snapshot: the core's list, taken whole, with the cursor kept on its box and the
    /// marks on the boxes still shown. One from a subscription since replaced is dropped.
    fn take_listing(&mut self, sub: u32, l: Result<Listing, String>) {
        if sub != self.list_sub {
            return;
        }
        let l = match l {
            Ok(l) => l,
            Err(e) => {
                self.list_error = Some(e);
                return;
            }
        };
        let at = self.listing.rows.get(self.cursor).map(|r| r.r#box.clone());
        let anchor = self.range.and_then(|i| self.listing.rows.get(i)).map(|r| r.r#box.clone());
        self.loaded = true;
        self.asked = None;
        self.list_error = None;
        self.boxes = l.boxes.clone();
        self.listing = l;
        let last = self.listing.rows.len().saturating_sub(1);
        let at = match self.point.take() {
            Some(k) if self.row_of(&k).is_some() => Some(k),
            Some(k) => {
                self.point = Some(k);
                at
            }
            None => at,
        };
        self.cursor = at.and_then(|id| self.row_of(&id)).unwrap_or(self.cursor.min(last));
        self.range = self.range.map(|i| anchor.and_then(|id| self.row_of(&id)).unwrap_or(i.min(last))).filter(|_| !self.listing.rows.is_empty());
        let shown: HashSet<String> = self.listing.rows.iter().map(|r| r.r#box.clone()).collect();
        self.selected.retain(|k| shown.contains(k));
    }

    fn new_pane(&mut self, exec: Tab) -> Pane {
        let stream = self.next_stream;
        self.next_stream += 1;
        Pane::new(stream, exec)
    }

    /// Show the box on a row: its workspace if it has one, attaching on first use.
    pub fn open_row(&mut self, index: usize) {
        if let Some(key) = self.listing.rows.get(index).map(|r| r.r#box.clone()) {
            self.open_box(&key);
        }
    }

    /// Show a box: its workspace if it has one, attaching on first use. A box that is not
    /// running is only pointed at — starting one is a command of its own. The cursor goes to its
    /// row when the filter shows it.
    pub fn open_box(&mut self, key: &str) {
        let Some(sbx) = self.box_by_id(key).cloned() else { return };
        if let Some(i) = self.row_of(key) {
            self.cursor = i;
        }
        if !sbx.running() && !self.workspaces.contains_key(&sbx.key) {
            self.message = Some(format!("{} is {} — start it from its menu", sbx.name, sbx.state.as_str()));
            return;
        }
        self.show_workspace(&sbx.key);
    }

    /// Goes to a box the palette chose (ADR 26-11-06-671 §3): its workspace when it has one or is
    /// running, else its row, without starting it; one the filter hides is shown outside it.
    pub fn navigate(&mut self, key: &str) {
        if self.box_by_id(key).is_none() {
            self.message = Some("the box is no longer listed".into());
            return;
        }
        if self.row_of(key).is_none() {
            self.reveal(key);
            self.point = Some(key.to_string());
        }
        self.visited(key);
        self.open_box(key);
        if self.live.as_deref() != Some(key) {
            self.focus = Focus::List;
        }
    }

    /// Puts a box's workspace on screen, made on first use and unparked after: the one there
    /// was is parked, or let go if it has nothing to keep. This is every way a box takes the
    /// screen, so it is also where the box counts as gone to (the palette's ranking) and where a
    /// view the palette revealed for another box is put back.
    fn show_workspace(&mut self, sandbox: &str) {
        // A box taking the screen takes it from the page beside the list.
        self.harnesses = None;
        self.unreveal(Some(sandbox));
        self.visited(sandbox);
        if self.live.as_deref() != Some(sandbox) {
            self.leave_workspace();
            if !self.workspaces.contains_key(sandbox) {
                // A box not up yet says what it is being made to do while its workspace opens.
                let up = self.box_by_id(sandbox).is_some_and(|b| b.running() && b.provisioning.is_empty());
                let provisioning_sub = if up { 0 } else { self.subscribe(Subscribe { topic: Topic::Provisioning, r#box: sandbox.to_string(), ..Default::default() }) };
                self.workspaces.insert(sandbox.to_string(), Workspace {
                    sandbox: sandbox.to_string(), panes: vec![], active: [None, None], focus: Side::Terminals, maximized: false, opening: true,
                    error: None, sessions_sub: 0, header_sub: 0, header: None, split: 0.5, step: String::new(), provisioning_sub,
                });
            }
            self.live = Some(sandbox.to_string());
        }
        self.follow_workspace(sandbox);
        self.focus = Focus::Panes;
    }

    /// Follows what only a workspace on screen follows: its sessions, polled, and its header,
    /// whose subscription is its port forward.
    fn follow_workspace(&mut self, sandbox: &str) {
        if self.workspaces.get(sandbox).is_none_or(|w| w.sessions_sub != 0) {
            return;
        }
        let sessions = self.subscribe(Subscribe { topic: Topic::Sessions, r#box: sandbox.to_string(), ..Default::default() });
        let header = self.subscribe(Subscribe { topic: Topic::Header, r#box: sandbox.to_string(), ..Default::default() });
        let ws = self.workspaces.get_mut(sandbox).unwrap();
        (ws.sessions_sub, ws.header_sub) = (sessions, header);
    }

    /// Stops following a workspace's sessions and header: what either still sends is dropped.
    fn unfollow_workspace(&mut self, sandbox: &str) {
        let Some(ws) = self.workspaces.get_mut(sandbox) else { return };
        let subs = [std::mem::take(&mut ws.sessions_sub), std::mem::take(&mut ws.header_sub)];
        for sub in subs.into_iter().filter(|&s| s != 0) {
            self.subs.remove(&sub);
            self.seen.remove(&sub);
            self.headers.remove(&sub);
            self.backend.send(Out::Unsubscribe(sub));
        }
    }

    /// Takes the workspace on screen off it: parked, attached, when it has its primary; let go
    /// when it has nothing to keep.
    fn leave_workspace(&mut self) {
        let Some(key) = self.live.take() else { return };
        if self.workspaces.get(&key).is_some_and(Workspace::parkable) {
            self.unfollow_workspace(&key);
        } else {
            self.release_workspace(&key);
        }
    }

    /// Lets go of a workspace: every tab's stream and both its subscriptions. Every session goes
    /// on in the box.
    fn release_workspace(&mut self, sandbox: &str) {
        self.unfollow_workspace(sandbox);
        let Some(ws) = self.workspaces.remove(sandbox) else { return };
        if ws.provisioning_sub != 0 {
            self.backend.send(Out::Unsubscribe(ws.provisioning_sub));
        }
        for p in ws.panes.iter().filter(|p| !p.exited) {
            self.backend.send(Out::Close(p.stream));
        }
        for p in &ws.panes {
            self.chips.remove(&p.stream);
        }
        if self.live.as_deref() == Some(sandbox) {
            self.live = None;
            self.focus = Focus::List;
        }
    }

    /// Ask the core for a session in a box — a shell, or another of its harness's terminals —
    /// on a stream allocated for its pane. The pane is made when the result says what it is.
    fn new_session(&mut self, shell: Option<String>) {
        let (cmd, targets, sandbox) = if let Some(b) = shell {
            ("shell", vec![b.clone()], b)
        } else {
            ("terminal", vec![], self.live.clone().unwrap_or_default())
        };
        let stream = self.next_stream;
        self.next_stream += 1;
        let (cols, rows) = DEFAULT_SIZE;
        let context = self.context();
        self.command(cmd, targets, context, Args { stream, cols: cols.into(), rows: rows.into(), ..Default::default() }, Pending::Session(stream, sandbox, self.live.clone()));
    }

    fn end_focused(&mut self) {
        let Some(id) = self.live.clone() else { return };
        let ws = &self.workspaces[&id];
        let Some(i) = ws.focused() else { return };
        let pane = &ws.panes[i];
        // A service is the box's own furniture: stopped from its menu, never ended.
        if pane.service() {
            self.message = Some(format!("a service is stopped rather than ended — stop it from {}", self.spec.command("services").map_or("services", |c| c.label.as_str())));
            return;
        }
        // A tab held after its program finished has no session left to end: it is dismissed.
        if pane.exited {
            let stream = pane.stream;
            self.drop_pane(stream);
            return;
        }
        let args = Args { stream: pane.stream, exec: pane.exec.exec.clone(), ..Default::default() };
        let context = self.context();
        let stream = args.stream;
        self.command("end", vec![], context, args, Pending::End(stream));
    }

    /// Takes a pane away once its session is ended.
    fn drop_pane(&mut self, stream: u32) {
        self.chips.remove(&stream);
        if self.dragging == Some(stream) {
            self.dragging = None;
        }
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

    /// A tab chip dragged along its strip: once it has moved a few points it follows the
    /// pointer, taking the place of each chip in its column whose middle the pointer passes.
    /// Letting go leaves it there, and a cancelled drag (Escape, the window losing focus) puts it
    /// back; a press that never moved is a press.
    pub fn drag_chip(&mut self, stream: u32, d: &mut Drag) {
        match d.phase {
            // Where the pointer is, in window points, is where it went down plus how far it
            // has travelled: the chip itself moves under it.
            Phase::Down => {
                d.data = self.chips.get(&stream).map_or(0.0, |r| r[0]) + d.x;
                self.drag_home = self.live_ws().and_then(|w| w.panes.iter().position(|p| p.stream == stream));
            }
            Phase::Move => {
                if self.dragging.is_none() && d.dx.abs() < DRAG_SLOP {
                    return;
                }
                self.dragging = Some(stream);
                let x = d.data + d.dx;
                let chips = &self.chips;
                let Some(ws) = self.live.as_ref().and_then(|k| self.workspaces.get_mut(k)) else { return };
                let Some(from) = ws.panes.iter().position(|p| p.stream == stream) else { return };
                let side = ws.panes[from].side();
                let over = ws.panes.iter().position(|p| {
                    p.side() == side && p.stream != stream && chips.get(&p.stream).is_some_and(|r| x >= r[0] && x < r[0] + r[2])
                });
                if let Some(to) = over {
                    let r = chips[&ws.panes[to].stream];
                    // Past the middle, the way it is going: a wider chip is not swapped back at once.
                    let mid = r[0] + r[2] / 2.0;
                    if (to > from && x >= mid) || (to < from && x < mid) {
                        ws.move_pane(from, to);
                    }
                }
            }
            Phase::Up => {
                let home = self.drag_home.take();
                if self.dragging.take().is_some()
                    && d.cancelled()
                    && let Some(home) = home
                    && let Some(ws) = self.live_ws_mut()
                    && let Some(at) = ws.panes.iter().position(|p| p.stream == stream)
                {
                    ws.move_pane(at, home);
                }
            }
        }
    }

    /// The divider between the columns dragged: the terminals' share of the width follows the
    /// pointer, keeping each column at least `MIN_SPLIT` of it.
    pub fn drag_split(&mut self, d: &mut Drag) {
        let w = self.columns_w.max(1.0);
        let Some(ws) = self.live_ws_mut() else { return };
        match d.phase {
            Phase::Down => d.data = ws.split,
            Phase::Move | Phase::Up if d.cancelled() => ws.split = d.data,
            Phase::Move | Phase::Up => ws.split = (d.data + d.dx / w).clamp(MIN_SPLIT, 1.0 - MIN_SPLIT),
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

    /// The `n`th tab that wears a number, from 0 (the pane gesture).
    fn goto_pane(&mut self, n: usize) {
        if let Some(ws) = self.live_ws_mut()
            && let Some(&i) = ws.numbered().get(n)
        {
            ws.show(i);
            self.focus = Focus::Panes;
        }
    }

    /// The `n`th service's tab, from 0 (the service gesture).
    fn goto_service(&mut self, n: usize) {
        if let Some(ws) = self.live_ws_mut()
            && let Some(&i) = ws.services().get(n)
        {
            ws.show(i);
            self.focus = Focus::Panes;
        }
    }

    /// Previous or next box: switch to it when it is running, otherwise point at it.
    fn step_box(&mut self, by: i32) {
        let n = self.listing.rows.len() as i32;
        if n == 0 {
            return;
        }
        self.cursor = (self.cursor as i32 + by).clamp(0, n - 1) as usize;
        let Some(b) = self.row_box(self.cursor).cloned() else { return };
        if b.running() || self.workspaces.contains_key(&b.key) {
            self.open_box(&b.key);
        } else {
            self.unreveal(Some(&b.key));
            self.focus = Focus::List;
        }
    }

    /// The pane with the keys, in the box on screen.
    pub fn focused_pane(&self) -> Option<&Pane> {
        let ws = self.live_ws()?;
        ws.focused().map(|i| &ws.panes[i])
    }

    fn focused_pane_mut(&mut self) -> Option<&mut Pane> {
        let ws = self.live_ws_mut()?;
        let i = ws.focused()?;
        Some(&mut ws.panes[i])
    }

    /// Bytes for a pane's program that are not a key: a mouse report, a wheel's arrows, a
    /// paste. A session that ended is sent nothing.
    pub fn send_to(&mut self, stream: u32, bytes: &[u8]) {
        if bytes.is_empty() || self.pane_mut(stream).is_none_or(|p| p.exited) {
            return;
        }
        self.backend.input(stream, bytes);
    }

    /// The copy gesture: the focused pane's selection to the clipboard, and the highlight let
    /// go. With nothing selected it copies nothing; the chord is never the program's.
    pub fn copy_selection(&mut self, tx: &mut Tx) {
        let Some(p) = self.focused_pane_mut() else { return };
        if let Some(text) = p.screen.selection_text() {
            tx.copy(text);
            p.screen.clear_selection();
        }
    }

    /// The paste gesture: what the clipboard holds, into the focused pane, as its program asked
    /// to be pasted to.
    pub fn paste(&mut self, tx: &mut Tx) {
        let Some(stream) = self.focused_pane().filter(|p| !p.exited).map(|p| p.stream) else { return };
        tx.read_clipboard(move |s: &mut State, content, _| s.paste_text(stream, content.text()));
    }

    pub fn paste_text(&mut self, stream: u32, text: &str) {
        let Some(p) = self.pane_mut(stream) else { return };
        if text.is_empty() {
            return;
        }
        p.screen.to_bottom();
        let bytes = p.screen.paste(text);
        self.send_to(stream, &bytes);
    }

    /// The find gesture: a find bar over the focused pane, on what it last looked for.
    pub fn open_find(&mut self) {
        let Some(p) = self.focused_pane_mut() else { return };
        p.screen.open_find();
        let query = p.screen.find.as_ref().map(|f| f.query.clone()).unwrap_or_default();
        let stream = p.stream;
        let mut edit = TextEdit::new(&query);
        edit.focused = true;
        self.find = Some(FindBar { query: edit, stream });
    }

    /// What the find bar holds, looked for again from the live screen back.
    pub fn find_changed(&mut self) {
        let Some(f) = &self.find else { return };
        let (stream, query) = (f.stream, f.query.text.clone());
        if let Some(p) = self.pane_mut(stream) {
            p.screen.find(&query);
        }
    }

    /// The next match back through the scrollback, or on toward the live screen.
    pub fn find_next(&mut self, back: bool) {
        let Some(stream) = self.find.as_ref().map(|f| f.stream) else { return };
        if let Some(p) = self.pane_mut(stream) {
            p.screen.find_next(back);
        }
    }

    /// Whether the find bar has the keys: it is open, on the pane that has them. One whose
    /// pane lost them — another box or pane focused, the list, its session gone — is closed.
    pub fn finding(&mut self) -> bool {
        if self.find.is_some() && self.find_stream().is_none() {
            self.close_find();
        }
        self.find.is_some()
    }

    /// The stream of the pane the find bar is open on while that pane has the keys: where the
    /// bar is drawn, and whether it is.
    pub fn find_stream(&self) -> Option<u32> {
        let f = self.find.as_ref()?;
        (self.focus == Focus::Panes && self.focused_pane().is_some_and(|p| p.stream == f.stream)).then_some(f.stream)
    }

    /// The pane on a stream, in any workspace.
    pub fn pane(&self, stream: u32) -> Option<&Pane> {
        self.workspaces.values().flat_map(|w| w.panes.iter()).find(|p| p.stream == stream)
    }

    /// Closes the find bar, leaving the pane where it scrolled to.
    pub fn close_find(&mut self) {
        let Some(f) = self.find.take() else { return };
        if let Some(p) = self.pane_mut(f.stream) {
            p.screen.close_find();
        }
    }

    /// A screen back (`pages` > 0) or on through the focused pane's scrollback.
    pub fn page_pane(&mut self, pages: i32) {
        if let Some(p) = self.focused_pane_mut() {
            p.screen.page(pages);
        }
    }

    /// The `mouse` command's flow: takes the focused pane's mouse back from its program, so a
    /// drag selects, or gives it back.
    fn toggle_mouse(&mut self) {
        let Some(p) = self.focused_pane_mut() else { return };
        p.screen.seized = !p.screen.seized;
        let wants = p.screen.wants_mouse();
        self.message = Some(match (p.screen.seized, wants) {
            (true, true) => "the mouse selects; the program hears none of it",
            (true, false) => "the mouse selects, and will when the program asks for it",
            (false, true) => "the mouse is the program's again",
            (false, false) => "the mouse is the program's whenever it asks",
        }.into());
    }

    /// The `repaint` command: the pane's size asserted again, since another window on the same
    /// session may have laid it out at its own, then the core asked for the screen again at it.
    fn repaint_focused(&mut self) {
        let Some(p) = self.focused_pane() else {
            self.message = Some("no pane to repaint".into());
            return;
        };
        let stream = p.stream;
        self.resend_size(stream);
        let context = self.context();
        self.command("repaint", vec![], context, Args { stream, ..Default::default() }, Pending::Enter("repaint".into()));
    }

    /// An input method on the focused pane: its composition drawn at the cursor, and what it
    /// commits typed.
    pub fn ime(&mut self, ime: &Ime) -> bool {
        if !self.pane_has_keys() || self.finding() {
            return false;
        }
        let Some(p) = self.focused_pane_mut() else { return false };
        match ime {
            Ime::Preedit(text, _) => p.screen.preedit = (!text.is_empty()).then(|| text.clone()),
            Ime::Commit(text) => {
                p.screen.preedit = None;
                let text = text.clone();
                self.send(text.as_bytes());
            }
            Ime::Enabled | Ime::Disabled => p.screen.preedit = None,
        }
        true
    }

    // -----------------------------------------------------------------------------------------
    // The mouse on a pane: points are the screen's own, from its top-left.

    /// A press, drag and release on a pane (`index` in its workspace). A left ⌘-click (Ctrl-click
    /// elsewhere) follows a link, one a program made or a URL in its text, and a plain left click
    /// follows a box's address even while the program has the mouse (`follow`); a program that
    /// asked for the mouse hears it unless it was taken back or Shift is held, as in every
    /// terminal; otherwise the primary button selects, copied on release.
    pub fn pane_drag(&mut self, stream: u32, index: usize, d: &rune::Drag, tx: &mut Tx) {
        let mods = tx.mods();
        let button = if tx.button() == rune::PointerButton::Middle { term::Button::Middle } else { term::Button::Left };
        if d.phase == rune::Phase::Down {
            self.select_pane(index);
        }
        let claimed = self.spec.link_schemes.clone();
        let Some(p) = self.pane_mut(stream) else { return };
        let (col, row, right) = cell_at(d.x, d.y);
        let (x, y) = (col.min(p.screen.cols.saturating_sub(1)), row.clamp(0, p.screen.rows as i32 - 1) as usize);
        let report = |p: &mut Pane, act| p.screen.mouse(button, act, x, y, mods.shift, mods.alt, mods.ctrl);
        let bytes = match d.phase {
            rune::Phase::Down => {
                // A link the console claims (a box's address) is the window's to follow on a left
                // press, before the program is told of it, unless the mouse was taken back to
                // select with; any link is followed with the link modifier. The second press of a
                // double click on a box's address is the first's, which already went to its box.
                let link = (button == term::Button::Left).then(|| p.screen.link_at(x, y as i32, &claimed)).flatten();
                if let Some(link) = link.filter(|l| mods.cmd || (l.claimed && !p.screen.seized)) {
                    if !(link.claimed && d.clicks > 1) {
                        self.follow(stream, link.uri);
                    }
                    return;
                }
                if p.screen.reports_mouse() && !mods.shift {
                    report(p, term::MouseAct::Press)
                } else {
                    if button == term::Button::Left {
                        p.screen.select_start(col, row, right, d.clicks, mods.alt);
                    }
                    None
                }
            }
            rune::Phase::Move if p.screen.selecting => {
                p.screen.select_to(col, row, right);
                None
            }
            rune::Phase::Move => report(p, term::MouseAct::Motion),
            rune::Phase::Up => match report(p, term::MouseAct::Release) {
                Some(b) => Some(b),
                None => {
                    if let Some(text) = p.screen.select_end() {
                        tx.copy(text);
                    }
                    None
                }
            },
        };
        if let Some(b) = bytes {
            self.send_to(stream, &b);
        }
    }

    /// Asks the core where a link pressed in a pane goes (the bridge's `follow`): the URL to
    /// open, through the forward of the box the pane is one of, or the box a discobox link names.
    fn follow(&mut self, stream: u32, uri: String) {
        let sandbox = self.workspaces.iter().find(|(_, w)| w.panes.iter().any(|p| p.stream == stream)).map(|(k, _)| k.clone()).unwrap_or_default();
        let id = self.next_id;
        self.next_id += 1;
        self.pending.insert(id, Pending::Follow(self.live.clone()));
        self.backend.send(Out::Follow(Follow { id, uri, r#box: sandbox }));
    }

    /// The pointer moving over a pane with no button held: news only to a program that asked
    /// for every motion.
    pub fn pane_hover(&mut self, stream: u32, x: f32, y: f32) {
        let Some(p) = self.pane_mut(stream) else { return };
        let (col, row, _) = cell_at(x, y);
        p.screen.hover = (row >= 0 && (row as usize) < p.screen.rows && col < p.screen.cols).then_some((col, row as usize));
        if p.screen.pressed() || p.screen.selecting || !p.screen.reports_mouse() {
            return;
        }
        let (col, row, _) = cell_at(x, y);
        if row < 0 || row >= p.screen.rows as i32 || col >= p.screen.cols {
            return;
        }
        if let Some(b) = p.screen.mouse(term::Button::None, term::MouseAct::Motion, col, row as usize, false, false, false) {
            self.send_to(stream, &b);
        }
    }

    /// The secondary button on a pane (`index` in its workspace): a program with the mouse hears
    /// it; otherwise over a selection it copies it and lets it go, as Windows terminals do; else
    /// it opens the pane's menu where it was pressed.
    pub fn pane_secondary(&mut self, stream: u32, index: usize, at: rune::PointerEvent, tx: &mut Tx) {
        let mods = tx.mods();
        self.select_pane(index);
        let Some(p) = self.pane_mut(stream) else { return };
        let (col, row, _) = cell_at(at.x, at.y);
        let (col, row) = (col.min(p.screen.cols.saturating_sub(1)), row.clamp(0, p.screen.rows as i32 - 1) as usize);
        if p.screen.reports_mouse() && !mods.shift {
            let mut b = p.screen.mouse(term::Button::Right, term::MouseAct::Press, col, row, mods.shift, mods.alt, mods.ctrl).unwrap_or_default();
            b.extend(p.screen.mouse(term::Button::Right, term::MouseAct::Release, col, row, mods.shift, mods.alt, mods.ctrl).unwrap_or_default());
            self.send_to(stream, &b);
        } else if let Some(text) = p.screen.selection_text() {
            tx.copy(text);
            p.screen.clear_selection();
        } else {
            self.open_commands_menu(MenuKind::Pane, (at.window_x, at.window_y));
        }
    }

    /// A wheel or trackpad over a pane, to whoever can scroll (`Screen::wheel`): a notched
    /// wheel turns a tick a notch (Rune gives a notch as 40 points), a trackpad the lines its
    /// points come to.
    pub fn pane_wheel(&mut self, stream: u32, w: &rune::WheelEvent) {
        const NOTCH: f32 = 40.0;
        let Some(p) = self.pane_mut(stream) else { return };
        let (col, row, _) = cell_at(w.x, w.y);
        let (col, row) = (col.min(p.screen.cols.saturating_sub(1)), row.clamp(0, p.screen.rows as i32 - 1) as usize);
        let lines = if w.precise { w.dy / theme::CELL_H } else { w.dy / NOTCH * term::WHEEL_LINES };
        if let Some(b) = p.screen.wheel(lines, col, row) {
            self.send_to(stream, &b);
        }
    }

    /// What the programs on screen copied (OSC 52), to the clipboard. A box that is not on
    /// screen — parked, or covered by a page (`workspace_shown`) — keeps running, and what its
    /// program copies is dropped, then and later: it would replace what the user just copied
    /// where they are, as the TUI's rule has it.
    fn take_copies(&mut self, tx: &mut Tx) {
        let live = self.workspace_shown().then(|| self.live.clone()).flatten();
        for (key, ws) in self.workspaces.iter_mut() {
            for p in ws.panes.iter_mut() {
                if let Some(text) = p.screen.copied.take()
                    && live.as_ref() == Some(key)
                {
                    tx.copy(text);
                }
            }
        }
    }


    pub fn send(&mut self, bytes: &[u8]) {
        let Some(ws) = self.live_ws_mut() else { return };
        let Some(i) = ws.focused() else { return };
        let pane = &mut ws.panes[i];
        // A service runs on pipes, and its card is text: nothing is typed into either.
        if pane.exited || pane.service() {
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

    /// A box's action menu, from its row: the core's entries for it — for every box marked,
    /// when it is one of them, as the TUI's menu is for the selection.
    pub fn open_menu(&mut self, sandbox: String, at: (f32, f32)) {
        self.close_overlays();
        let context = Context { list_has_keys: true, ..self.context() };
        let targets = match self.row_of(&sandbox) {
            Some(i) if self.is_selected(i) => self.list_targets(),
            _ => vec![sandbox],
        };
        self.command("actions", targets, context, Args::default(), Pending::Actions(at));
    }

    /// Where the services menu and a service's menu open: over the workspace's terminals.
    fn services_at(&self) -> (f32, f32) { (if self.sidebar { 340.0 } else { 40.0 }, 96.0) }

    /// A box's services, as the services command read them: each by its name, with what it is
    /// doing and what it is for, or why it cannot run.
    pub fn open_services_menu(&mut self, sandbox: String, services: Vec<Service>) {
        self.close_overlays();
        let entries = services.iter().map(|v| Entry {
            command: v.id.clone(), label: v.name.clone(), detail: v.detail.clone(), enabled: true, why: String::new(), uses: 0,
        }).collect();
        let at = self.services_at();
        self.menu = Some(Menu { kind: MenuKind::Services(sandbox, services), at, entries, picked: 0, sub: 0 });
    }

    /// What to do with one service: show its output — its tab, while the workspace holds one —
    /// then the verbs the core offers for it, each as it stands.
    pub fn open_service_menu(&mut self, sandbox: String, service: Service) {
        self.close_overlays();
        let shown = self.workspaces.get(&sandbox).is_some_and(|w| w.panes.iter().any(|p| p.exec.service == service.id));
        let mut entries = vec![Entry {
            command: OUTPUT.into(), label: "view output".into(), detail: "show the service log".into(), enabled: shown,
            why: if shown { String::new() } else { "no output pane is available".into() }, uses: 0,
        }];
        entries.extend(service.actions.iter().map(|a| Entry {
            command: a.verb.as_str().into(), label: a.label.clone(), detail: a.detail.clone(), enabled: a.enabled, why: a.why.clone(), uses: 0,
        }));
        let picked = entries.iter().position(|e| e.enabled).unwrap_or(0);
        let at = self.services_at();
        self.menu = Some(Menu { kind: MenuKind::Service(sandbox, service), at, entries, picked, sub: 0 });
    }

    /// Shows a service's tab in its box's workspace: its output.
    pub fn show_service(&mut self, sandbox: &str, service: &str) {
        if self.live.as_deref() != Some(sandbox) {
            self.show_workspace(sandbox);
        }
        if let Some(ws) = self.live_ws_mut()
            && let Some(i) = ws.panes.iter().position(|p| p.exec.service == service)
        {
            ws.show(i);
            self.focus = Focus::Panes;
        } else {
            self.message = Some("service output is no longer available".into());
        }
    }

    /// Runs one of a service's verbs: the services command, naming the service and the verb. What
    /// the service does next, the box's sessions say.
    pub fn service_verb(&mut self, sandbox: String, service: &Service, verb: ServiceVerb) {
        let doing = match verb {
            ServiceVerb::Start => "starting",
            ServiceVerb::Stop => "stopping",
            ServiceVerb::Restart => "restarting",
        };
        self.message = Some(format!("{doing} {}…", service.name));
        let context = self.context();
        let args = Args { service: service.id.clone(), verb: Some(verb), ..Default::default() };
        self.command("services", vec![sandbox], context, args, Pending::Enter("services".into()));
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
    /// hold yet, placed in the core's order. A session's tab is never closed here (its stream says
    /// when it ends); a service's is, when the snapshot no longer lists it on the run it draws.
    fn take_sessions(&mut self, sub: u32, r: Result<Sessions, String>) {
        let Some(sbx) = self.subs.get(&sub).cloned() else { return };
        self.seen.insert(sub, r.clone());
        // A session still being made in this box is in no pane yet, and may already be listed:
        // while one is, only the primary and the services open, and the rest once it is made
        // (retake_sessions).
        let making = self.pending.values().any(|p| matches!(p, Pending::Session(_, b, _) if *b == sbx));
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
        let gone: Vec<u32> = ws.panes.iter()
            .filter(|p| p.service() && !sessions.iter().any(|e| e.exec == p.exec.exec && e.run == p.exec.run))
            .map(|p| p.stream)
            .collect();
        for stream in gone {
            if self.pane_mut(stream).is_some_and(|p| !p.exited) {
                self.backend.send(Out::Close(stream));
            }
            self.drop_pane(stream);
        }
        let ws = &self.workspaces[&sbx];
        let order: Vec<String> = sessions.iter().map(|e| e.exec.clone()).collect();
        let fresh: Vec<Tab> = sessions.into_iter()
            .filter(|e| !ws.panes.iter().any(|p| p.exec.exec == e.exec) && (!making || matches!(e.kind, TabKind::Primary | TabKind::Service)))
            .collect();
        let mut opens = vec![];
        for e in fresh {
            let mut pane = self.new_pane(e);
            if pane.service() {
                // A service runs on pipes: its output, and its transcript, end lines with a bare
                // line feed.
                pane.screen.newline_mode();
                // One not running says its state over its card.
                if !pane.exec.status.is_empty() {
                    pane.status = Some(pane.exec.status.clone());
                }
            }
            opens.push((pane.exec.exec.clone(), pane.stream));
            self.workspaces.get_mut(&sbx).unwrap().insert(pane, &order);
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

    /// What a create would be made with, said under the composer.
    fn take_composer(&mut self, sub: u32, r: Result<Composer, String>) {
        if sub == self.composer_sub && let Ok(c) = r {
            self.summary = c.chips;
        }
    }

    /// What a box opening is being made to do, for its workspace; once it says nothing more, the
    /// box is up and the subscription goes.
    fn take_provisioning(&mut self, sub: u32, r: Result<Provisioning, String>) {
        let Some(ws) = self.workspaces.values_mut().find(|w| w.provisioning_sub == sub && sub != 0) else { return };
        let Ok(p) = r else { return };
        let done = p.step.is_empty() && !ws.step.is_empty();
        ws.step = p.step;
        if done {
            ws.provisioning_sub = 0;
            self.backend.send(Out::Unsubscribe(sub));
        }
    }

    /// A stream attached: when it is a workspace's primary, the box is up, and what it was being
    /// made to do is over. The watch behind the provisioning topic says what an attach waits on
    /// and runs until it is stopped — once the box is provisioned it says "connecting to the
    /// terminal" — so the attach connecting is what ends it, as the TUI's ends with its attach.
    fn provisioned(&mut self, stream: u32) {
        let Some(ws) = self.workspaces.values_mut()
            .find(|w| w.panes.iter().any(|p| p.stream == stream && p.exec.kind == TabKind::Primary)) else { return };
        ws.step.clear();
        let sub = std::mem::take(&mut ws.provisioning_sub);
        if sub != 0 {
            self.backend.send(Out::Unsubscribe(sub));
        }
    }

    /// The composer's prompt changed: it is told to the CLI once it has been still a moment.
    pub fn composer_changed(&mut self) { self.edited = Some(self.now); }

    /// Tells the CLI the prompt if it changed and has been still for `still` seconds (`0` on the
    /// way out, when a prompt still in the composer is told even unchanged: there is one draft on
    /// the machine, and the window closed last is the one whose prompt comes back — the TUI's
    /// rule. An empty composer that was never anything else tells nothing).
    pub fn save_draft(&mut self, still: f64) {
        let leaving = still == 0.0;
        let due = self.edited.is_some_and(|at| self.now - at >= still);
        let prompt = self.composer.text.clone();
        if !(due || (leaving && !prompt.is_empty())) || (!leaving && prompt == self.draft) {
            return;
        }
        self.edited = None;
        self.draft = prompt.clone();
        self.backend.send(Out::Draft(prompt));
    }

    /// The composer, on screen with the keys: where `new` goes from a workspace, as the TUI's
    /// opens its composer card.
    pub fn show_composer(&mut self) {
        self.unreveal(None);
        self.leave_workspace();
        self.live = None;
        self.focus = Focus::List;
        self.composer.focused = true;
    }

    /// A header snapshot, for the workspace that follows it.
    fn take_header(&mut self, sub: u32, r: Result<Header, String>) {
        let Some(sbx) = self.headers.get(&sub) else { return };
        if let (Some(ws), Ok(h)) = (self.workspaces.get_mut(sbx), r) {
            ws.header = Some(h);
        }
    }

    /// A stream's state: attached, reconnecting and back, or ended — what an ending does is the
    /// core's (`Tab.End`), and is applied to a workspace parked or on screen alike.
    fn take_stream(&mut self, st: Stream) {
        match st.state {
            StreamState::Attached => {
                self.resend_size(st.stream);
                self.provisioned(st.stream);
            }
            StreamState::Reconnecting | StreamState::Reconnected => {
                if let Some(p) = self.pane_mut(st.stream) {
                    p.status = (st.state == StreamState::Reconnecting).then(|| "reconnecting…".into());
                }
            }
            StreamState::Ended | StreamState::Failed => self.end_stream(st),
        }
    }

    fn end_stream(&mut self, st: Stream) {
        let Some(key) = self.workspaces.iter().find(|(_, w)| w.panes.iter().any(|p| p.stream == st.stream)).map(|(k, _)| k.clone()) else { return };
        let name = self.box_by_id(&key).map(|b| b.name.clone()).unwrap_or_else(|| key.clone());
        match st.end.unwrap_or(TabEnd::Tab) {
            TabEnd::Workspace => {
                if let Some(p) = self.pane_mut(st.stream) {
                    p.exited = true;
                }
                self.release_workspace(&key);
                self.message = Some(if st.why.is_empty() { format!("{name}: the session ended") } else { format!("{name}: {}", st.why) });
            }
            TabEnd::Tab => {
                let title = self.pane_mut(st.stream).map(|p| p.exec.title.clone()).unwrap_or_default();
                self.drop_pane(st.stream);
                if !st.why.is_empty() {
                    self.message = Some(format!("{name}: {title}: {}", st.why));
                }
            }
            TabEnd::Held => {
                if let Some(p) = self.pane_mut(st.stream) {
                    p.exited = true;
                    p.status = Some(if st.status.is_empty() { st.why } else { st.status });
                }
            }
        }
    }

    fn take_result(&mut self, r: CommandResult, tx: &mut Tx) {
        // Whatever the command still asked is moot once it has finished.
        self.sheets.finished(r.id);
        let Some(why) = self.pending.remove(&r.id) else { return };
        let failed = r.status == Status::Failed;
        match why {
            // A form or question dismissed: nothing came of it, and nothing to say. A session's
            // cancel still lets its stream go below.
            Pending::Enter(_) | Pending::Verb(..) | Pending::Harness(_) | Pending::Change if r.status == Status::Canceled => self.message = None,
            Pending::Enter(id) if r.status == Status::Enter => enter(self, &id, &r, tx),
            // What came of it (`renamed to …`, `settings saved`), or nothing to say.
            Pending::Enter(_) if r.status == Status::Done => self.message = (!r.note.is_empty()).then(|| r.note.clone()),
            Pending::End(stream) if r.status == Status::Done => self.drop_pane(stream),
            Pending::Session(stream, sandbox, shown) if r.status == Status::Done => {
                // Nothing was made on the stream: a tool that turned out to run on this machine,
                // handed the box. The note says what came of it.
                let Some(exec) = r.session.clone() else {
                    self.early.remove(&stream);
                    self.message = (!r.note.is_empty()).then(|| r.note.clone());
                    self.retake_sessions(&sandbox);
                    return;
                };
                // A session lands in its own box's workspace, wherever that is. It takes the screen
                // only if nobody moved since asking for it: a box switched to meanwhile stays on
                // screen, and the session waits in its own box's parked workspace — or, with none
                // there, in the box itself, its stream let go, for its workspace to open.
                if self.live == shown || self.live.as_deref() == Some(&r.r#box) {
                    self.show_workspace(&r.r#box);
                } else if !self.workspaces.contains_key(&r.r#box) {
                    self.early.remove(&stream);
                    self.backend.send(Out::Close(stream));
                    self.retake_sessions(&sandbox);
                    return;
                }
                let tool = exec.kind == TabKind::Tool;
                let pane = Pane::new(stream, exec);
                let ws = self.workspaces.get_mut(&r.r#box).unwrap();
                ws.panes.push(pane);
                ws.show(ws.panes.len() - 1);
                // A tool is given the window, as the TUI's opens over the workspace; maximize
                // gives the workspace back with the tool on its tab.
                if tool {
                    ws.maximized = true;
                    self.focus = Focus::Panes;
                    self.message = None;
                }
                for e in self.early.remove(&stream).unwrap_or_default() {
                    self.take_stream_event(e);
                }
                self.resend_size(stream);
                self.retake_sessions(&sandbox);
            }
            Pending::Session(stream, sandbox, _) => {
                self.early.remove(&stream);
                self.retake_sessions(&sandbox);
            }
            Pending::Page(server) if r.status == Status::Enter => harnesses::open_page(self, server, &r),
            Pending::Harness(name) if r.status == Status::Done => harnesses::take(self, &name, &r),
            // The candidate is answered: the review goes, and the listing says what the box has
            // left to save.
            Pending::Change if r.status == Status::Done => {
                self.review = None;
                self.message = (!r.note.is_empty()).then(|| r.note.clone());
            }
            Pending::Follow(from) if from != self.live => return,
            Pending::Follow(_) if r.status == Status::Done => {
                if let Err(e) = tx.open_url(&r.url) {
                    self.message = Some(format!("could not open {}: {e:?}", r.url));
                }
            }
            // A refusal an earlier link left is said of a box gone from the screen.
            Pending::Follow(_) if r.status == Status::Enter => {
                self.message = None;
                self.navigate(&r.r#box);
            }
            Pending::Actions(at) if r.status == Status::Enter => overlay::show_menu(self, r.r#box.clone(), at, r.entries.clone(), tx),
            // The new box's workspace, on its way up; or, left detached, what came of it.
            // A harness's own way out typed into the composer: the window leaves, with the
            // composer emptied so the next window does not open on it.
            Pending::Create(_) if r.status == Status::Enter && r.r#box.is_empty() => {
                self.composer = TextEdit::new("");
                self.composer_changed();
                self.save_draft(0.0);
                tx.quit();
            }
            Pending::Create(_) if r.status == Status::Enter => {
                if !self.warned {
                    self.message = None;
                }
                if let Some(i) = self.row_of(&r.r#box) {
                    self.cursor = i;
                }
                self.show_workspace(&r.r#box);
            }
            Pending::Create(_) if r.status == Status::Done => self.message = (!r.note.is_empty()).then(|| r.note.clone()),
            Pending::Create(_) if r.status == Status::Canceled => self.message = None,
            Pending::Verb(verb, name) => {
                // A verb that acted on anything is worth letting the marks go for: they were put
                // there to run this, and leaving them invites running it twice (the TUI's rule).
                if !failed || !r.note.is_empty() {
                    self.clear_selection();
                }
                if !failed {
                    self.message = Some(if r.note.is_empty() { format!("{verb}: {name}") } else { r.note.clone() });
                }
            }
            _ => {}
        }
        if failed {
            self.message = Some(r.why);
        }
    }

    /// Output, a state or an end on a stream: to its pane, or kept, in order, until the pane is
    /// made. A shell's stream is attached before its result says what it is, so its first output
    /// — or its ending, for one that ends at once — can arrive first.
    fn take_stream_event(&mut self, e: Event) {
        let stream = match &e {
            Event::Data(stream, _) | Event::Exited(stream, _) => *stream,
            Event::Stream(st) => st.stream,
            _ => return,
        };
        if self.pane_mut(stream).is_none() && self.pending.values().any(|p| matches!(p, Pending::Session(s, _, _) if *s == stream)) {
            self.early.entry(stream).or_default().push(e);
            return;
        }
        if let Event::Stream(st) = e {
            self.take_stream(st);
            return;
        }
        let Some(p) = self.pane_mut(stream) else { return };
        match e {
            Event::Data(_, bytes) => {
                let reply = p.screen.feed(&bytes);
                if !reply.is_empty() {
                    self.backend.input(stream, &reply);
                }
            }
            // The stream's last frame. What its ending does arrived before it (`end_stream`);
            // this only settles a pane still showing it.
            Event::Exited(_, msg) => {
                if !p.exited {
                    p.exited = true;
                    p.status = (!msg.is_empty()).then_some(msg);
                }
            }
            _ => {}
        }
    }
}

/// The registry's commands this app answers. Every other one says it is not here yet, and has
/// an entry in the parity ledger (`docs/console/parity.md`) saying which issue brings it.
pub const ANSWERED: &[&str] = &[
    "attach", "shell", "upgrade", "repair", "stop", "start", "archive", "unarchive", "purge", "rename", "settings",
    "quit", "help", "palette", "terminal", "end", "maximize", "detach", "actions", "mouse", "repaint",
    "next pane", "previous pane", "next box", "previous box", "sidebar",
    "refresh", "servers", "archived", "services", "harnesses", "review config", "new", "options", "tools",
];

/// Why a command cannot run in this app yet, for one it does not answer: what it says when run,
/// and what its menu entry says.
pub fn lacking(spec: &Spec, id: &str) -> Option<String> {
    if ANSWERED.contains(&id) {
        return None;
    }
    Some(match spec.command(id) {
        Some(c) => format!("{} is not in the desktop app yet", c.label),
        None => format!("unknown command {id}"),
    })
}

/// Every command, by its registry ID, on what the window has picked: the box on screen while its
/// panes have the keys, else the list's selection. The keys, the menus and a row's actions all
/// come here, and go to the core, which rules on it and runs it.
pub fn run_command(s: &mut State, id: &str, tx: &mut Tx) {
    let targets = match id {
        // A verb takes every box the list's selection holds, in one batch.
        "stop" | "start" | "upgrade" | "repair" | "archive" | "unarchive" | "purge" => s.targets(),
        _ => s.target().map(|b| vec![b.key]).unwrap_or_default(),
    };
    run_on(s, id, targets, tx);
}

/// A command on boxes named explicitly — the palette's chip, a menu's box — never on what is on
/// screen instead (ADR 26-11-06-671 §3). The core rules on it and runs it; a flow that is the
/// app's own is entered when the core says it can run (`enter`). A command the app does not
/// answer yet says so, and is not sent.
pub fn run_on(s: &mut State, id: &str, targets: Vec<String>, _tx: &mut Tx) {
    s.close_overlays();
    s.message = None;
    s.warned = false;
    if let Some(why) = lacking(&s.spec, id) {
        s.message = Some(why);
        return;
    }
    let context = s.context();
    let box_command = s.spec.command(id).is_some_and(|c| c.target == SpecTarget::Box);
    let targets = if box_command { targets } else { vec![] };
    match id {
        "shell" => {
            if let Some(b) = targets.first().cloned() {
                s.new_session(Some(b));
            }
        }
        "terminal" => s.new_session(None),
        "end" => s.end_focused(),
        "repaint" => s.repaint_focused(),
        "actions" => {
            // The box's menu, where the keys are: by its row in the list, by its header beside a pane.
            let Some(key) = targets.first().cloned() else { return };
            let at = match s.row_of(&key) {
                Some(i) if s.focus == Focus::List || s.live.is_none() => (260.0, 120.0 + i as f32 * 28.0),
                _ => (900.0, 30.0),
            };
            s.open_menu(key, at);
        }
        "stop" | "start" | "upgrade" | "repair" | "archive" | "unarchive" | "purge" => {
            let name = match targets.as_slice() {
                [] => return,
                [one] => s.box_by_id(one).map(|b| b.name.clone()).unwrap_or_else(|| one.clone()),
                many => format!("{} boxes", many.len()),
            };
            // A purge is asked first (the core's confirmation, as a sheet): it says nothing until
            // it is answered.
            s.message = match id {
                "stop" => Some("stopping"),
                "start" => Some("starting"),
                "upgrade" => Some("upgrading"),
                "repair" => Some("repairing"),
                "archive" => Some("archiving"),
                "unarchive" => Some("unarchiving"),
                _ => None,
            }.map(|doing| format!("{doing} {name}…"));
            s.command(id, targets, context, Args::default(), Pending::Verb(id.into(), name));
        }
        // The page reads the harnesses of the server the list is narrowed to.
        "harnesses" => harnesses::open(s),
        "refresh" => {
            s.message = Some("refreshing…".into());
            s.command(id, vec![], context, Args::default(), Pending::Enter(id.into()));
        }
        // From a workspace, the composer, to write what the box is for; from the composer, the
        // create card for what it holds (the TUI's composer card, then its create card).
        "new" if s.live.is_some() => {
            s.show_composer();
            s.message = Some("write what the new discobox should do, or create it with nothing".into());
        }
        "new" => {
            let prompt = s.composer.text.clone();
            s.save_draft(0.0);
            s.command(id, vec![], context, Args { prompt: prompt.clone(), ..Default::default() }, Pending::Create(prompt));
        }
        "options" => {
            let prompt = s.composer.text.clone();
            s.command(id, vec![], context, Args { prompt, ..Default::default() }, Pending::Enter(id.into()));
        }
        _ => s.command(id, targets, context, Args::default(), Pending::Enter(id.into())),
    }
}

/// A command's flow that is the app's own, once the core has ruled it can run.
fn enter(s: &mut State, id: &str, r: &CommandResult, tx: &mut Tx) {
    match id {
        // The core has ruled the box can be attached, whatever its state: its primary is
        // what waits for it.
        "attach" => {
            if let Some(i) = s.row_of(&r.r#box) {
                s.cursor = i;
            }
            s.show_workspace(&r.r#box);
        }
        "mouse" => s.toggle_mouse(),
        "maximize" => {
            if let Some(ws) = s.live_ws_mut() {
                ws.maximized = !ws.maximized;
            }
        }
        // Detaching closes the workspace, as the TUI's does: every session goes on in the box,
        // and attaching again opens them anew. Switching to another box is what parks one.
        "detach" => {
            s.unreveal(None);
            if let Some(key) = s.live.clone() {
                s.release_workspace(&key);
                s.message = Some("detached — the discobox is still running".into());
            }
            s.live = None;
            s.focus = Focus::List;
        }
        "palette" => s.open_palette(),
        "review config" => harnesses::open_review(s, r),
        // The box's tools and addresses, over whatever is on screen.
        "tools" => tools::open_menu(s, r),
        // The box's services, on its workspace: a box that declares none says so instead.
        "services" => {
            s.show_workspace(&r.r#box);
            if r.services.is_empty() {
                s.message = (!r.note.is_empty()).then(|| r.note.clone());
            } else {
                s.open_services_menu(r.r#box.clone(), r.services.clone());
            }
        }
        "help" => {
            s.close_overlays();
            s.help = true;
        }
        "quit" => {
            s.save_draft(0.0);
            tx.quit();
        }
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
        // The filter's choices are the listing's; the menu opens on the first one marked.
        "servers" => {
            // The choices are counted against the filter the list is on: the person's own, back
            // in place of a revealed box's view.
            s.unreveal(None);
            let picked = s.listing.choices.iter().position(|c| c.marked).unwrap_or(0);
            s.filter_menu = Some(FilterMenu { picked });
        }
        "archived" => {
            let Some(mut f) = s.filter() else {
                s.message = Some("the list has not been read yet".into());
                return;
            };
            f.archived = !f.archived;
            s.message = Some(if f.archived { "showing archived boxes" } else { "archived boxes hidden" }.into());
            s.set_filter(f);
        }
        _ => {}
    }
}

/// How long the composer is still before its prompt is told to the CLI, in seconds.
const DRAFT_STILL: f64 = 1.0;

impl State {
    /// A running command's step or warning, said on the status line; and for a create, the box
    /// the moment the server takes it: the prompt it carried is running now, and no longer the
    /// composer's — not before, since a create refused leaves it there to send again.
    fn take_progress(&mut self, p: wire::Progress) {
        self.sheets.went_on(p.command);
        if let Some(made) = p.accepted {
            if let Some(Pending::Create(prompt)) = self.pending.get(&p.command)
                && self.composer.text == *prompt
            {
                self.composer = TextEdit::new("");
                self.composer_changed();
            }
            if !self.warned {
                self.message = Some(format!("creating {}…", made.name));
            }
            return;
        }
        if p.warning {
            self.message = Some(p.text);
            self.warned = true;
        } else if !p.text.is_empty() && !self.warned {
            self.message = Some(p.text);
        }
    }
}

/// The window closing, however it is closed: the prompt in the composer is told to the CLI on
/// the way out, and the app quits.
fn close(s: &mut State, tx: &mut Tx) {
    s.save_draft(0.0);
    tx.quit();
}

fn setup(s: &mut State, waker: Waker) { s.inbox.set_waker(waker); }

/// Drain what the backend sent into the window.
fn poll(s: &mut State, t: f64, tx: &mut Tx) -> bool {
    s.now = t;
    s.save_draft(DRAFT_STILL);
    s.sheets.reveal(tx);
    let events = s.inbox.drain();
    if events.is_empty() {
        return false;
    }
    for e in events {
        match e {
            Event::Listing(sub, l) => s.take_listing(sub, l),
            Event::Sessions(sub, r) => s.take_sessions(sub, r),
            Event::Commands(sub, r) => s.take_commands(sub, r),
            Event::Palette(sub, r) => s.take_palette(sub, r),
            Event::Header(sub, r) => s.take_header(sub, r),
            Event::Result(r) => s.take_result(r, tx),
            Event::Question(q) => sheet::take_question(s, q),
            Event::Form(f) => sheet::take_form(s, f),
            Event::FieldErrors(e) => sheet::take_errors(s, e),
            Event::Edit(e) => sheet::take_edit(s, e),
            Event::Composer(sub, r) => s.take_composer(sub, r),
            Event::Provisioning(sub, r) => s.take_provisioning(sub, r),
            Event::Progress(p) => s.take_progress(p),
            e @ (Event::Data(..) | Event::Exited(..) | Event::Stream(..)) => s.take_stream_event(e),
        }
    }
    s.take_copies(tx);
    true
}

fn on_key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool { keys::on_key(s, ev, tx) }

fn on_ime(s: &mut State, ime: &Ime, _tx: &mut Tx) -> bool { s.ime(ime) }

/// What the window opens on: the backend, the spec, its inbox, whether it is the CLI's, and the
/// prompt the last window left.
type Boot = (Box<dyn Backend + Send>, Spec, Inbox, bool, String);

/// What `main` connected, for `State::new` (an `AppSpec` init takes no arguments).
static BOOT: Mutex<Option<Boot>> = Mutex::new(None);

fn boot_mock() -> Boot {
    let inbox = Inbox::default();
    (Box::new(Mock::new(inbox.clone())), backend::bundled_spec(), inbox, false, String::new())
}

macro_rules! menu_commands {
    ($(($menu:literal, $id:literal)),* $(,)?) => {
        // A native menu's key equivalent reaches AppKit's menu ahead of the window's keys, so a
        // sheet a command waits on is kept from here as the window's keys are.
        &[$(($menu, $id, (|s: &mut State, tx: &mut Tx| if !s.sheets.is_open() { run_command(s, $id, tx) }) as fn(&mut State, &mut Tx))),*]
    };
}

/// The commands the native menu bar carries, by the menu they are in (macOS; elsewhere the
/// palette is the menu).
const MENU: &[(&str, &str, fn(&mut State, &mut Tx))] = menu_commands![
    ("Box", "new"), ("Box", "options"), ("Shell", "shell"), ("Shell", "terminal"), ("Shell", "end"),
    ("Box", "attach"), ("Box", "rename"), ("Box", "settings"),
    ("Box", "stop"), ("Box", "start"), ("Box", "upgrade"), ("Box", "repair"), ("Box", "archive"), ("Box", "unarchive"), ("Box", "purge"),
    ("View", "detach"), ("View", "sidebar"), ("View", "palette"), ("View", "maximize"),
    ("View", "servers"), ("View", "archived"), ("View", "refresh"),
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
            Ok((bridge, spec, draft)) => (Box::new(bridge) as Box<dyn Backend + Send>, spec, inbox, true, draft),
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
        // Every scene is drawn in both themes by `desktop:snap` (`--theme dark|light`).
        scenes: &[
            // A scene is one frame: what the mock answered is taken before it is drawn.
            ("list", |_| {}),
            // Two boxes marked and a range drawn, with every row nested and sectioned.
            ("selection", |s| {
                s.toggle_select();
                s.move_cursor(2);
                s.toggle_select();
                s.move_cursor(3);
                s.toggle_range();
                s.move_cursor(4);
            }),
            ("filter", |s| {
                run_command(s, "servers", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
            }),
            ("workspace", |s| {
                s.open_row(0);
                let b = s.live.clone();
                s.new_session(b);
                poll(s, 0.0, &mut Tx::default());
            }),
            // A pane as a terminal: wide characters and emoji at their cells, a link, a
            // selection, and the find bar on a match.
            ("terminal", |s| {
                s.open_row(0);
                poll(s, 0.0, &mut Tx::default());
                let mut out = String::from("\x1b[2J\x1b[H");
                for i in 0..40 {
                    out.push_str(&format!("build step {i}: ok\r\n"));
                }
                out.push_str("wide: 漢字かな 😀 🚀 é done\r\n");
                out.push_str("docs: \x1b]8;;https://example.com/guide\x1b\\the guide\x1b]8;;\x1b\\ is linked\r\n");
                out.push_str("\x1b[1;32mselect\x1b[0m this line\r\n$ ");
                if let Some(p) = s.focused_pane_mut() {
                    p.screen.feed(out.as_bytes());
                    let rows = p.screen.rows as i32;
                    p.screen.select_start(0, rows - 2, false, 3, false);
                }
                s.open_find();
                if let Some(f) = s.find.as_mut() {
                    f.query = TextEdit::new("step 3");
                }
                s.find_changed();
            }),
            ("palette", |s| {
                s.open_row(0);
                poll(s, 0.0, &mut Tx::default());
                run_command(s, "palette", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
            }),
            // A command first: `stop ` commits stop as the chip, and the boxes it can stop follow.
            ("palette-command", |s| {
                run_command(s, "palette", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
                if let Some(p) = s.palette.as_mut() {
                    p.set_text("stop ");
                }
                s.ask_palette();
                poll(s, 0.0, &mut Tx::default());
            }),
            // A box first: the commands it can take.
            ("palette-box", |s| {
                run_command(s, "palette", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
                let key = s.rows()[0].r#box.clone();
                if let Some(p) = s.palette.as_mut() {
                    p.sandbox = key;
                }
                s.ask_palette();
                poll(s, 0.0, &mut Tx::default());
            }),
            ("menu", |s| {
                s.open_row(0);
                poll(s, 0.0, &mut Tx::default());
                let id = s.rows()[0].r#box.clone();
                s.open_menu(id, (420.0, 140.0));
                poll(s, 0.0, &mut Tx::default());
            }),
            // The menu bar's Box menu, over a workspace: every entry the core rules on, with why.
            ("menubar", |s| {
                s.open_row(0);
                poll(s, 0.0, &mut Tx::default());
                let at = s.bar_at.get(&DesktopMenu::Box).copied().unwrap_or((60.0, 30.0));
                s.open_commands_menu(MenuKind::Bar(DesktopMenu::Box), at);
                poll(s, 0.0, &mut Tx::default());
            }),
            // The first box's services as tabs beside its primary, a migration's last output on
            // its card, and the services menu over them.
            ("services", |s| {
                s.open_row(0);
                poll(s, 0.0, &mut Tx::default());
                if let Some(ws) = s.live_ws_mut()
                    && let Some(i) = ws.panes.iter().position(|p| p.exec.service == "migrate")
                {
                    ws.show(i);
                }
                run_command(s, "services", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
            }),
            ("pane-menu", |s| {
                s.open_row(0);
                poll(s, 0.0, &mut Tx::default());
                s.open_commands_menu(MenuKind::Pane, (700.0, 300.0));
                poll(s, 0.0, &mut Tx::default());
            }),
            ("help", |s| {
                run_command(s, "help", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
            }),
            // The core's forms and questions, as sheets.
            ("rename", |s| {
                run_command(s, "rename", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
            }),
            // A field that does not apply, sections, pickers, and a refusal under its field.
            ("settings", |s| {
                run_command(s, "settings", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
                if let Some(f) = s.sheets.form() {
                    let id = f.id;
                    sheet::take_errors(s, wire::FieldErrors { form: id, errors: vec![wire::FieldError { field: "name".into(), why: "a discobox needs a configured name".into() }] });
                }
            }),
            // The composer holding a prompt, with what a create would be made with under it.
            ("composer", |s| {
                s.composer = TextEdit::new("fix the reaper race in the pool agent\nand add a test for it");
                s.composer.focused = true;
            }),
            // The create card for the composer's prompt: where it is cut from, the card's
            // settings, a skills directory added, and the command it would run.
            ("new", |s| {
                s.composer = TextEdit::new("fix the reaper race");
                run_command(s, "new", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
                if let Some(id) = s.sheets.form().map(|f| f.id) {
                    s.backend.send(Out::Submit(wire::Submit { form: id, answers: [("skills:0".to_string(), "./team-skills".to_string())].into(), draft: true, ..Default::default() }));
                    poll(s, 0.0, &mut Tx::default());
                }
            }),
            // The run options: every row.
            ("options", |s| {
                run_command(s, "options", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
            }),
            // A box just created, on its way up: its workspace says what it is being made to do.
            ("created", |s| {
                s.composer = TextEdit::new("fix the reaper race");
                run_command(s, "new", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
                if let Some(id) = s.sheets.form().map(|f| f.id) {
                    s.backend.send(Out::Submit(wire::Submit { form: id, ..Default::default() }));
                }
                poll(s, 0.0, &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
            }),
            ("purge", |s| {
                let old = s.boxes.iter().find(|b| b.state == wire::State::Archived).map(|b| b.key.clone()).unwrap_or_default();
                run_on(s, "purge", vec![old], &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
            }),
            // The harnesses page beside the list, a harness's actions, its config card, and a
            // box's harness changes under review.
            ("harnesses", |s| {
                run_command(s, "harnesses", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
                if let Some(p) = s.harnesses.as_mut() {
                    p.cursor = 1;
                }
                harnesses::open_menu(s, (900.0, 160.0));
            }),
            ("harness-card", |s| {
                run_command(s, "harnesses", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
                harnesses::act(s, wire::HarnessAction::Config, String::new(), (0.0, 0.0));
                poll(s, 0.0, &mut Tx::default());
            }),
            ("harness-review", |s| {
                run_command(s, "review config", &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
            }),
        ],
        fonts: theme::FONTS,
        families: (theme::FAMILY, theme::FAMILY),
        setup: Some(setup),
        poll: Some(poll),
        on_key: Some(on_key),
        ..AppSpec::new("discobox", (1440.0, 900.0), State::new, view::view)
    }).on_close(close);
    for &(menu, id, action) in MENU {
        let Some(label) = keys::label_of(&spec, id) else { continue };
        let mut item = MenuItem::new(title_case(&label), action).id(id).menu(menu);
        if let Some(sc) = keys::bindings(&spec, id).first().and_then(|k| keys::menu_shortcut(k)) {
            item = item.key_equivalent(sc);
        }
        app = app.menu_item(item);
    }
    app.on_ime(on_ime).run();
}
