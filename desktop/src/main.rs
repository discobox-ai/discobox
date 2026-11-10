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
mod icons;
#[cfg(test)]
mod identity_tests;
mod keys;
mod mark;
mod overlay;
#[cfg(test)]
mod parity_tests;
mod sheet;
#[cfg(test)]
mod sheet_tests;
#[cfg(test)]
mod surface_tests;
mod term;
mod theme;
mod view;

use std::collections::{HashMap, HashSet};
use std::sync::Mutex;

use backend::{Backend, Bridge, Event, Inbox, Mock, Out};
use discobox_bridge::wire;
use wire::{
    Args, Box as Sandbox, Command, Menu as DesktopMenu, Commands, Context, Entry, Filter, Listing, Note, Palette as Found, PaletteQuery, PaletteRow,
    Result as CommandResult, Row, Sessions, Spec, SpecTarget, Status, StreamState, Subscribe, Tab, TabKind, Topic,
};
use rune::{App, AppSpec, KeyEvent, MenuItem, Mods, TextEdit, Tx, Waker};
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
}

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
    /// The forms and questions commands wait on, drawn as sheets one at a time.
    pub sheets: sheet::Sheets,
    /// The boxes this window has gone to, latest first, which rank the palette's empty query
    /// (ADR 26-11-06-671 §6): at most `RECENT` of them, for as long as the window lasts.
    pub recent: Vec<String>,
    /// A box the palette went to that the filter hid, and the filter it stands in for: shown
    /// through a view that is never saved, and put back when the window leaves the box.
    pub reveal: Option<(String, Filter)>,
    /// A box to put the cursor on once a listing shows it: one the palette went to that the
    /// filter hid, until the view revealing it lands.
    point: Option<String>,
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
    /// What arrived on a stream before its pane was made: a shell's first output can come
    /// before its result.
    early: HashMap<u32, Vec<Event>>,
}

const DEFAULT_SIZE: (u16, u16) = (100, 30);

/// How many boxes gone to the palette ranks by (ADR 26-11-06-671 §6).
const RECENT: usize = 100;

impl State {
    fn new() -> State {
        let (backend, spec, inbox, bridged) = BOOT.lock().unwrap().take().unwrap_or_else(boot_mock);
        theme::set_tokens(&spec);
        let mut s = State {
            backend, inbox, spec, boxes: vec![], listing: Listing::default(), loaded: false, list_error: None, cursor: 0,
            selected: HashSet::new(), range: None, filter_menu: None, asked: None, filter_at: (24.0, 96.0), workspaces: HashMap::new(), live: None, focus: Focus::List,
            sidebar: true, message: None, palette: None, menu: None, bar_at: HashMap::new(), help: false, sheets: sheet::Sheets::default(), recent: vec![], reveal: None, point: None,
            bridged, pending: HashMap::new(), list_sub: 0, next_id: 1,
            next_sub: 1, next_stream: 1, subs: HashMap::new(), seen: HashMap::new(), early: HashMap::new(),
        };
        s.follow_listing(None, String::new());
        if !bridged {
            // The mock lists at once; taking it here lets a headless scene open a box.
            for e in s.inbox.drain() {
                if let Event::Listing(sub, l) = e {
                    s.take_listing(sub, l);
                }
            }
        }
        s
    }

    /// Follows a topic under a new subscription ID, which is returned.
    fn subscribe(&mut self, mut sub: Subscribe) -> u32 {
        sub.sub = self.next_sub;
        self.next_sub += 1;
        if sub.topic == Topic::Sessions {
            self.subs.insert(sub.sub, sub.r#box.clone());
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
    }

    /// Opens a menu of the menu bar, or the pane's menu, at a place: its commands from the spec,
    /// then as the core rules on them where it was opened.
    pub fn open_commands_menu(&mut self, kind: MenuKind, at: (f32, f32)) {
        self.close_overlays();
        let ids: Vec<String> = match &kind {
            MenuKind::Bar(menu) => self.in_menu(*menu),
            MenuKind::Pane => self.in_menu(DesktopMenu::Session),
            MenuKind::Actions(_) => return,
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
            _ => self.row_box(self.cursor).cloned(),
        }
    }

    /// The boxes a box command acts on, by key: the one on screen while its panes have the
    /// keys, else what the list's selection says.
    fn targets(&self) -> Vec<String> {
        match (&self.live, self.focus) {
            (Some(id), Focus::Panes) => vec![id.clone()],
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
        Pane { stream, exec, screen: Screen::new(DEFAULT_SIZE.0 as usize, DEFAULT_SIZE.1 as usize), exited: false, status: None }
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
        self.unreveal(Some(&sbx.key));
        self.visited(&sbx.key);
        self.ensure_workspace(&sbx.key);
        self.live = Some(sbx.key);
        self.focus = Focus::Panes;
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

    /// A box's workspace, made and subscribed to its sessions on first use.
    fn ensure_workspace(&mut self, sandbox: &str) {
        if !self.workspaces.contains_key(sandbox) {
            self.workspaces.insert(sandbox.to_string(), Workspace {
                sandbox: sandbox.to_string(), panes: vec![], active: [None, None], focus: Side::Terminals, maximized: false, opening: true, error: None,
            });
            self.subscribe(Subscribe { topic: Topic::Sessions, r#box: sandbox.to_string(), ..Default::default() });
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
        // Whatever the command still asked is moot once it has finished.
        self.sheets.finished(r.id);
        let Some(why) = self.pending.remove(&r.id) else { return };
        let failed = r.status == Status::Failed;
        match why {
            // A form or question dismissed: nothing came of it, and nothing to say. A session's
            // cancel still lets its stream go below.
            Pending::Enter(_) | Pending::Verb(..) if r.status == Status::Canceled => self.message = None,
            Pending::Enter(id) if r.status == Status::Enter => enter(self, &id, &r, tx),
            // What came of it (`renamed to …`, `settings saved`), or nothing to say.
            Pending::Enter(_) if r.status == Status::Done => self.message = (!r.note.is_empty()).then(|| r.note.clone()),
            Pending::End(stream) if r.status == Status::Done => self.drop_pane(stream),
            Pending::Session(stream, sandbox) if r.status == Status::Done => {
                let Some(exec) = r.session.clone() else { return };
                self.unreveal(Some(&r.r#box));
                self.visited(&r.r#box);
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
    "attach", "shell", "upgrade", "repair", "stop", "start", "archive", "unarchive", "purge", "rename", "settings",
    "quit", "help", "palette", "terminal", "end", "maximize", "detach", "actions",
    "next pane", "previous pane", "next box", "previous box", "sidebar",
    "refresh", "servers", "archived",
];

/// Why a command cannot run in this app yet, for one it does not answer: what it says when run,
/// and what its menu entry says.
pub fn lacking(spec: &Spec, id: &str) -> Option<String> {
    if ANSWERED.contains(&id) {
        return None;
    }
    Some(match spec.command(id) {
        Some(c) if matches!(id, "new" | "review config") => {
            format!("{} needs a dialog, which the desktop app does not have yet", c.label)
        }
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
        "refresh" => {
            s.message = Some("refreshing…".into());
            s.command(id, vec![], context, Args::default(), Pending::Enter(id.into()));
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
            s.unreveal(Some(&r.r#box));
            s.visited(&r.r#box);
            if let Some(i) = s.row_of(&r.r#box) {
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
            s.unreveal(None);
            s.live = None;
            s.focus = Focus::List;
        }
        "palette" => s.open_palette(),
        "help" => {
            s.close_overlays();
            s.help = true;
        }
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

fn setup(s: &mut State, waker: Waker) { s.inbox.set_waker(waker); }

/// Drain what the backend sent into the window.
fn poll(s: &mut State, _t: f64, tx: &mut Tx) -> bool {
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
            Event::Question(q) => sheet::take_question(s, q),
            Event::Form(f) => sheet::take_form(s, f),
            Event::FieldErrors(e) => sheet::take_errors(s, e),
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
        // A native menu's key equivalent reaches AppKit's menu ahead of the window's keys, so a
        // sheet a command waits on is kept from here as the window's keys are.
        &[$(($menu, $id, (|s: &mut State, tx: &mut Tx| if !s.sheets.is_open() { run_command(s, $id, tx) }) as fn(&mut State, &mut Tx))),*]
    };
}

/// The commands the native menu bar carries, by the menu they are in (macOS; elsewhere the
/// palette is the menu).
const MENU: &[(&str, &str, fn(&mut State, &mut Tx))] = menu_commands![
    ("Box", "new"), ("Shell", "shell"), ("Shell", "terminal"), ("Shell", "end"),
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
            ("purge", |s| {
                let old = s.boxes.iter().find(|b| b.state == wire::State::Archived).map(|b| b.key.clone()).unwrap_or_default();
                run_on(s, "purge", vec![old], &mut Tx::default());
                poll(s, 0.0, &mut Tx::default());
            }),
        ],
        fonts: theme::FONTS,
        families: (theme::FAMILY, theme::FAMILY),
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
