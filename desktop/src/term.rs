//! One pane's terminal: the bytes a backend stream sends, emulated here, and drawn as rows of
//! monospace runs. This is the native half of "compose the screens from the backend": the
//! sandbox sends what its program wrote, and the window keeps and draws the screen.
//!
//! What a terminal does besides drawing is here too, as termpane does it for the TUI
//! (`termpane/DESIGN.md`), each in cell space so it is tested without a window:
//! - **selection**: press-drag-release, a word on a double click, a line on a triple, a block
//!   with Alt, past the top or bottom edge scrolling; copied on release, as the TUI's is;
//! - **the program's copies** (OSC 52) are copies like a selection's, and a clipboard *read* is
//!   answered with silence;
//! - **links** a program made (OSC 8), opened by a ⌘-click (Ctrl-click elsewhere);
//! - **the mouse**, reported to a program that asked for it in the encoding it asked for, until
//!   the user takes it back (the `mouse` command) or holds Shift; the wheel goes to whoever can
//!   scroll: a program with the mouse, a pager on the alternate screen as arrow keys, else the
//!   scrollback;
//! - **paste**, bracketed when the program asks, with paste markers stripped from the text;
//! - **search** through the scrollback;
//! - **wide characters and emoji** at their cells, whatever the face's own advance;
//! - **an input method's** composition, drawn at the cursor until it commits.

use std::sync::{Arc, Mutex};

use alacritty_terminal::event::{Event, EventListener};
use alacritty_terminal::grid::{Dimensions, Scroll};
use alacritty_terminal::index::{Column, Direction, Line, Point, Side};
use alacritty_terminal::selection::{Selection, SelectionRange, SelectionType};
use alacritty_terminal::term::cell::{Cell, Flags};
use alacritty_terminal::term::search::{Match, RegexSearch};
use alacritty_terminal::term::{Config, Term, TermMode};
use alacritty_terminal::vte::ansi::{Color as AColor, NamedColor, Processor};
use rune::{Color, El, Span, el, memo, rich};

use crate::theme;

/// What the emulator wants said back to the program (cursor reports, device attributes), the
/// title it set, and what the program copied (OSC 52).
#[derive(Clone, Default)]
pub struct Replies(Arc<Mutex<Heard>>);

#[derive(Default)]
struct Heard {
    reply: Vec<u8>,
    title: Option<String>,
    copied: Option<String>,
}

impl EventListener for Replies {
    fn send_event(&self, event: Event) {
        let mut r = self.0.lock().unwrap();
        match event {
            Event::PtyWrite(s) => r.reply.extend_from_slice(s.as_bytes()),
            Event::Title(t) => r.title = Some(t),
            // A clear (empty) is no reason to empty the user's clipboard; the last copy in a burst
            // is what the clipboard would have been left holding.
            Event::ClipboardStore(_, text) if !text.is_empty() => r.copied = Some(text),
            // `Osc52::OnlyCopy` never asks for a load; were one asked, it is answered with nothing.
            _ => {}
        }
    }
}

struct Size {
    cols: usize,
    rows: usize,
}

impl Dimensions for Size {
    fn total_lines(&self) -> usize { self.rows }
    fn screen_lines(&self) -> usize { self.rows }
    fn columns(&self) -> usize { self.cols }
}

/// The lines of scrollback a pane keeps.
const SCROLLBACK: usize = 5000;

/// The lines a wheel tick scrolls the scrollback, and the arrows a pager is sent for one, as
/// the TUI's pane does; a program with the mouse is told of one wheel button per tick.
pub const WHEEL_LINES: f32 = 3.0;

/// A double click's word: a path, a URL or a `--flag` in one click, while the shell's own
/// operators still split (termpane's word characters, as alacritty's escape set).
const WORD_BREAKS: &str = ",│`|\"' ()[]{}<>\t;!$*^\\";

/// A mouse button, as a program is told of it.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Button {
    Left,
    Middle,
    Right,
    /// Motion with no button held.
    None,
    WheelUp,
    WheelDown,
}

/// What a mouse event is.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum MouseAct {
    Press,
    Release,
    Motion,
}

/// The scrollback's find: what is looked for, and the match on screen.
pub struct Find {
    pub query: String,
    regex: Option<RegexSearch>,
    pub hit: Option<Match>,
    /// The query matches nothing in the scrollback or on screen.
    pub missing: bool,
}

pub struct Screen {
    term: Term<Replies>,
    parser: Processor,
    replies: Replies,
    pub cols: usize,
    pub rows: usize,
    /// Bumped on every change, so rows that did not change can be reused.
    pub version: u64,
    pub title: Option<String>,
    /// Whether a line feed is a new line (`newline_mode`).
    newlines: bool,
    /// What the program last copied (OSC 52), for the window to put on the clipboard.
    pub copied: Option<String>,
    /// The user took the mouse back from a program that asked for it (the `mouse` command): it
    /// selects, and the program hears nothing.
    pub seized: bool,
    /// A press reported to the program, whose drag and release it is owed.
    reporting: Option<Button>,
    /// The cell motion was last reported at: motion within a cell is not news.
    moved: Option<(usize, usize)>,
    /// A press is drawing a selection, which its drag extends and its release ends.
    pub selecting: bool,
    pub find: Option<Find>,
    /// What the last find looked for, which the next one opens on.
    last_query: String,
    /// What the wheel has turned that is not yet a whole line, or a whole tick to report.
    wheel_left: f32,
    /// An input method's composition, drawn at the cursor until it commits.
    pub preedit: Option<String>,
}

impl Screen {
    pub fn new(cols: usize, rows: usize) -> Screen {
        let replies = Replies::default();
        let config = Config { scrolling_history: SCROLLBACK, semantic_escape_chars: WORD_BREAKS.into(), ..Config::default() };
        let term = Term::new(config, &Size { cols, rows }, replies.clone());
        Screen {
            term, parser: Processor::new(), replies, cols, rows, version: 0, title: None, copied: None, seized: false,
            reporting: None, moved: None, selecting: false, find: None, last_query: String::new(), wheel_left: 0.0,
            preedit: None, newlines: false,
        }
    }

    /// Feeds program output; returns what the emulator answers back, for the stream's input.
    pub fn feed(&mut self, bytes: &[u8]) -> Vec<u8> {
        let history = self.term.grid().history_size();
        if self.newlines {
            let crlf: Vec<u8> = bytes.iter().flat_map(|&b| if b == b'\n' { vec![b'\r', b'\n'] } else { vec![b] }).collect();
            self.parser.advance(&mut self.term, &crlf);
        } else {
            self.parser.advance(&mut self.term, bytes);
        }
        self.version += 1;
        self.follow_hit(history);
        let mut r = self.replies.0.lock().unwrap();
        if let Some(t) = r.title.take() {
            self.title = Some(t);
        }
        if let Some(c) = r.copied.take() {
            self.copied = Some(c);
        }
        std::mem::take(&mut r.reply)
    }

    pub fn resize(&mut self, cols: usize, rows: usize) -> bool {
        let (cols, rows) = (cols.max(2), rows.max(1));
        if (cols, rows) == (self.cols, self.rows) {
            return false;
        }
        self.term.resize(Size { cols, rows });
        (self.cols, self.rows) = (cols, rows);
        // A resize reflows the grid: a match's points no longer name its text, and searching on
        // from one could start outside the grid. The next step finds again from the live screen.
        if let Some(f) = self.find.as_mut() {
            f.hit = None;
        }
        self.version += 1;
        true
    }

    fn mode(&self) -> TermMode { *self.term.mode() }

    pub fn app_cursor(&self) -> bool { self.mode().contains(TermMode::APP_CURSOR) }

    pub fn to_bottom(&mut self) {
        if self.term.grid().display_offset() != 0 {
            self.term.scroll_display(Scroll::Bottom);
            self.version += 1;
        }
    }

    pub fn scroll(&mut self, lines: i32) {
        self.term.scroll_display(Scroll::Delta(lines));
        self.version += 1;
    }

    /// Takes a line feed as a new line, carriage return included: what a program on pipes writes,
    /// which no terminal turned its newlines into CRLF for — a service, and its transcript. It is
    /// done here rather than with LNM, which alacritty_terminal applies to NEL but not to a line
    /// feed.
    pub fn newline_mode(&mut self) { self.newlines = true; }

    /// What the screen shows, a line at a time: what a test reads off a pane.
    #[cfg(test)]
    pub fn text(&self) -> String {
        let grid = self.term.grid();
        let offset = grid.display_offset() as i32;
        (0..self.rows).map(|y| {
            let row = &grid[Line(y as i32 - offset)];
            (0..self.cols).map(|x| row[Column(x)].c).collect::<String>().trim_end().to_string()
        }).collect::<Vec<_>>().join("\n")
    }

    /// A screen back (`pages` > 0) or on through the scrollback.
    pub fn page(&mut self, pages: i32) { self.scroll(pages * (self.rows as i32 - 1).max(1)); }

    /// Lines scrolled back from the live screen.
    pub fn offset(&self) -> usize { self.term.grid().display_offset() }

    /// The grid point at a cell of the view.
    fn point(&self, col: usize, row: i32) -> Point {
        let col = col.min(self.cols - 1);
        Point::new(Line(row - self.offset() as i32), Column(col))
    }

    // -----------------------------------------------------------------------------------------
    // Paste

    /// Pasted text as the bytes the program is sent: between the bracketed-paste markers when
    /// it asked for them, with any marker in the text taken out — a clipboard ending
    /// `\x1b[201~rm -rf /\r` would otherwise close its own paste and run. Without them, line
    /// ends are the Return a typed line ends with.
    pub fn paste(&self, text: &str) -> Vec<u8> {
        let clean = strip_paste_markers(text);
        if self.mode().contains(TermMode::BRACKETED_PASTE) {
            format!("\x1b[200~{clean}\x1b[201~").into_bytes()
        } else {
            clean.replace("\r\n", "\r").replace('\n', "\r").into_bytes()
        }
    }

    // -----------------------------------------------------------------------------------------
    // Selection

    /// A press that starts a selection at a cell: a word on the second click of a run, a line
    /// on the third, a block with Alt. `right` is whether it is in the cell's right half.
    pub fn select_start(&mut self, col: usize, row: i32, right: bool, clicks: u32, block: bool) {
        let ty = match clicks {
            2 => SelectionType::Semantic,
            n if n >= 3 => SelectionType::Lines,
            _ if block => SelectionType::Block,
            _ => SelectionType::Simple,
        };
        let side = if right { Side::Right } else { Side::Left };
        self.term.selection = Some(Selection::new(ty, self.point(col, row), side));
        self.selecting = true;
        self.version += 1;
    }

    /// The drag that extends it, to a cell; a row above or below the view scrolls the
    /// scrollback a line and keeps selecting.
    pub fn select_to(&mut self, col: usize, row: i32, right: bool) {
        if !self.selecting {
            return;
        }
        if row < 0 {
            self.term.scroll_display(Scroll::Delta(1));
        } else if row >= self.rows as i32 {
            self.term.scroll_display(Scroll::Delta(-1));
        }
        let row = row.clamp(0, self.rows as i32 - 1);
        let point = self.point(col, row);
        if let Some(sel) = self.term.selection.as_mut() {
            sel.update(point, if right { Side::Right } else { Side::Left });
            self.version += 1;
        }
    }

    /// The release that ends it: the text selected, to copy, or none (a plain click), which
    /// lets the selection go.
    pub fn select_end(&mut self) -> Option<String> {
        if !std::mem::take(&mut self.selecting) {
            return None;
        }
        let text = self.selection_text();
        if text.is_none() {
            self.clear_selection();
        }
        text
    }

    pub fn selection_text(&self) -> Option<String> { self.term.selection_to_string().filter(|t| !t.is_empty()) }

    pub fn has_selection(&self) -> bool { self.term.selection.as_ref().is_some_and(|s| !s.is_empty()) }

    pub fn clear_selection(&mut self) {
        if self.term.selection.take().is_some() {
            self.version += 1;
        }
    }

    // -----------------------------------------------------------------------------------------
    // Links

    /// Where the link a program made at a cell (OSC 8) points.
    pub fn link_at(&self, col: usize, row: i32) -> Option<String> {
        let p = self.point(col, row);
        self.term.grid()[p.line][p.column].hyperlink().map(|h| h.uri().to_string())
    }

    // -----------------------------------------------------------------------------------------
    // The mouse

    /// Whether the program is told of the mouse: it asked, and the user has not taken it back.
    pub fn reports_mouse(&self) -> bool { self.wants_mouse() && !self.seized }

    /// Whether a press the program heard is still held.
    pub fn pressed(&self) -> bool { self.reporting.is_some() }

    /// Whether the program asked for the mouse.
    pub fn wants_mouse(&self) -> bool { self.mode().intersects(TermMode::MOUSE_MODE) }

    /// A mouse event at a cell as the bytes the program asked for, or none for one it did not
    /// ask to hear. A press's drag and release are owed to the program that heard the press.
    pub fn mouse(&mut self, button: Button, act: MouseAct, col: usize, row: usize, shift: bool, alt: bool, ctrl: bool) -> Option<Vec<u8>> {
        let mode = self.mode();
        if !self.reports_mouse() && self.reporting.is_none() {
            return None;
        }
        match act {
            MouseAct::Press if !matches!(button, Button::WheelUp | Button::WheelDown | Button::None) => self.reporting = Some(button),
            MouseAct::Release => {
                self.reporting.take()?;
            }
            MouseAct::Motion => {
                let held = self.reporting.is_some();
                if !(mode.contains(TermMode::MOUSE_MOTION) || held && mode.contains(TermMode::MOUSE_DRAG)) {
                    return None;
                }
                if self.moved.replace((col, row)) == Some((col, row)) {
                    return None;
                }
            }
            _ => {}
        }
        let mut code: u32 = match button {
            Button::Left => 0,
            Button::Middle => 1,
            Button::Right => 2,
            Button::None => 3,
            Button::WheelUp => 64,
            Button::WheelDown => 65,
        };
        if act == MouseAct::Motion {
            code = self.reporting.map_or(3, |b| match b { Button::Middle => 1, Button::Right => 2, _ => 0 }) + 32;
        }
        code += 4 * shift as u32 + 8 * alt as u32 + 16 * ctrl as u32;
        let (x, y) = (col.min(self.cols - 1) as u32 + 1, row.min(self.rows - 1) as u32 + 1);
        if mode.contains(TermMode::SGR_MOUSE) {
            let end = if act == MouseAct::Release { 'm' } else { 'M' };
            return Some(format!("\x1b[<{code};{x};{y}{end}").into_bytes());
        }
        // The legacy encodings have no release per button: a release is button 3.
        if act == MouseAct::Release {
            code = 3 + (code & !3);
        }
        let mut out = b"\x1b[M".to_vec();
        out.push((32 + code) as u8);
        for v in [x, y] {
            if mode.contains(TermMode::UTF8_MOUSE) {
                let mut buf = [0u8; 4];
                out.extend_from_slice(char::from_u32(32 + v)?.encode_utf8(&mut buf).as_bytes());
            } else if 32 + v > 255 {
                // Past what a byte can say: the event cannot be told.
                return None;
            } else {
                out.push((32 + v) as u8);
            }
        }
        Some(out)
    }

    /// A wheel's turn, in lines (positive back through the scrollback), at a cell: to a program
    /// that has the mouse as a wheel button a tick, to a pager on the alternate screen as an
    /// arrow key a line, else the scrollback's own scroll. A notched wheel turns WHEEL_LINES a
    /// notch; a trackpad turns what it moved, and what is not yet a whole line (or a whole tick,
    /// for a program) is kept for the next turn, so a slow scroll still scrolls. What is
    /// returned is for the program.
    pub fn wheel(&mut self, lines: f32, col: usize, row: usize) -> Option<Vec<u8>> {
        // A turn the other way starts over: what was left was going the other way.
        if lines * self.wheel_left < 0.0 {
            self.wheel_left = 0.0;
        }
        self.wheel_left += lines;
        let mode = self.mode();
        if self.reports_mouse() {
            let ticks = (self.wheel_left / WHEEL_LINES).trunc();
            self.wheel_left -= ticks * WHEEL_LINES;
            let button = if ticks > 0.0 { Button::WheelUp } else { Button::WheelDown };
            let mut out = vec![];
            for _ in 0..ticks.abs() as usize {
                out.extend(self.mouse(button, MouseAct::Press, col, row, false, false, false)?);
            }
            return (!out.is_empty()).then_some(out);
        }
        let whole = self.wheel_left.trunc();
        self.wheel_left -= whole;
        let whole = whole as i32;
        if whole == 0 {
            return None;
        }
        if mode.contains(TermMode::ALT_SCREEN) {
            if !mode.contains(TermMode::ALTERNATE_SCROLL) {
                return None;
            }
            let key = match (whole > 0, self.app_cursor()) {
                (true, true) => "\x1bOA",
                (true, false) => "\x1b[A",
                (false, true) => "\x1bOB",
                (false, false) => "\x1b[B",
            };
            return Some(key.repeat(whole.unsigned_abs() as usize).into_bytes());
        }
        self.scroll(whole);
        None
    }

    // -----------------------------------------------------------------------------------------
    // Find

    /// Opens the find on what it last looked for, found again, or keeps the one open.
    pub fn open_find(&mut self) {
        if self.find.is_none() {
            let query = self.last_query.clone();
            self.find(&query);
        }
    }

    /// Closes the find, keeping what it looked for to open on next time.
    pub fn close_find(&mut self) {
        if let Some(f) = self.find.take() {
            self.last_query = f.query;
            self.version += 1;
        }
    }

    /// Looks for `query` (as typed, not a pattern; any case unless it has a capital), from the
    /// live screen's end back.
    pub fn find(&mut self, query: &str) {
        let regex = (!query.is_empty()).then(|| RegexSearch::new(&escape(query)).ok()).flatten();
        self.find = Some(Find { query: query.into(), regex, hit: None, missing: false });
        self.find_next(true);
    }

    /// The next match back through the scrollback (`back`), or on toward the live screen,
    /// wrapping at either end, scrolled to.
    pub fn find_next(&mut self, back: bool) {
        let Some(f) = self.find.as_mut() else { return };
        let Some(regex) = f.regex.as_mut() else {
            f.hit = None;
            f.missing = false;
            self.version += 1;
            return;
        };
        let (direction, side) = if back { (Direction::Left, Side::Left) } else { (Direction::Right, Side::Right) };
        let origin = match &f.hit {
            Some(h) if back => *h.start(),
            Some(h) => *h.end(),
            None => Point::new(Line(self.term.screen_lines() as i32 - 1), Column(self.cols - 1)),
        };
        // Starting at a match's own edge finds it again; one cell past it does not.
        let origin = match (&f.hit, back) {
            (Some(_), true) => origin.sub(&self.term, alacritty_terminal::index::Boundary::None, 1),
            (Some(_), false) => origin.add(&self.term, alacritty_terminal::index::Boundary::None, 1),
            (None, _) => origin,
        };
        let hit = self.term.search_next(regex, origin, direction, side, None);
        f.missing = hit.is_none();
        f.hit = hit.clone();
        if let Some(h) = hit {
            self.show_line(h.start().line);
        }
        self.version += 1;
    }

    /// Keeps a find's match on its text as output scrolls it into the scrollback: its lines
    /// move up by what the scrollback grew. Once the scrollback is full, lines fall off its top
    /// and how far the text moved is unknown, so the match goes, and the next step finds again
    /// from the live screen; so does a match on a screen switched away from.
    fn follow_hit(&mut self, history: usize) {
        let Some(hit) = self.find.as_mut().and_then(|f| f.hit.take()) else { return };
        let now = self.term.grid().history_size();
        // A scrollback cleared (`CSI 3 J`) shrinks, and says nothing of where its lines went either.
        if now >= SCROLLBACK || now < history || self.term.mode().contains(TermMode::ALT_SCREEN) {
            return;
        }
        let up = (now - history) as i32;
        let shift = |p: &Point| Point::new(p.line - up, p.column);
        if let Some(f) = self.find.as_mut() {
            f.hit = Some(shift(hit.start())..=shift(hit.end()));
        }
    }

    /// Scrolls so a grid line is on screen.
    fn show_line(&mut self, line: Line) {
        let offset = self.offset() as i32;
        let top = -offset;
        let bottom = top + self.rows as i32 - 1;
        if line.0 < top {
            self.term.scroll_display(Scroll::Delta(top - line.0));
        } else if line.0 > bottom {
            self.term.scroll_display(Scroll::Delta(bottom - line.0));
        }
    }

    /// The text of a view row, as it is drawn: for tests and the find's matches.
    #[cfg(test)]
    pub fn row_text(&self, row: usize) -> String {
        let line = Line(row as i32 - self.offset() as i32);
        let r = &self.term.grid()[line];
        (0..self.cols).map(|x| &r[Column(x)]).filter(|c| !c.flags.contains(Flags::WIDE_CHAR_SPACER)).map(|c| c.c).collect::<String>().trim_end().to_string()
    }

    // -----------------------------------------------------------------------------------------
    // Drawing

    /// The screen as elements: one keyed, memoised row per line, the cursor, and the input
    /// method's composition at it.
    pub fn view(&self, key: &str, focused: bool) -> El {
        let grid = self.term.grid();
        let offset = grid.display_offset() as i32;
        let content = self.term.renderable_content();
        let selection = content.selection;
        let hit = self.find.as_ref().and_then(|f| f.hit.clone());
        let mut body = el().w(self.cols as f32 * theme::cell_w()).h(self.rows as f32 * theme::CELL_H);
        for y in 0..self.rows {
            let line = Line(y as i32 - offset);
            let row = &grid[line];
            let cells: Vec<Drawn> = (0..self.cols).map(|x| {
                let point = Point::new(line, Column(x));
                Drawn::of(&row[Column(x)], mark(point, selection.as_ref(), hit.as_ref()))
            }).collect();
            // The palette a row is drawn in is part of every memo's key already: Rune hashes the
            // window's appearance into it.
            let version = hash_row(&cells);
            body = body.child(memo(format!("{key}-r{y}"), version, || draw_row(&cells, y)));
        }
        let cursor = content.cursor;
        let show = self.term.mode().contains(TermMode::SHOW_CURSOR) && offset == 0;
        if show && cursor.point.line.0 >= 0 {
            let (x, y) = (cursor.point.column.0 as f32 * theme::cell_w(), cursor.point.line.0 as f32 * theme::CELL_H);
            let c = el().key(format!("{key}-cursor")).absolute(x, y).size(theme::cell_w(), theme::CELL_H);
            let c = if focused { c.fill(theme::term_fg().alpha(0.85)).ime_cursor() } else { c.border(1.0, theme::grey()) };
            body = body.child(c);
            if let Some(p) = self.preedit.as_ref().filter(|p| !p.is_empty() && focused) {
                let span = Span::new(0, p.len()).color(theme::term_fg()).underline(theme::mark());
                body = body.child(rich(p.clone(), vec![span]).font(theme::TERM).color(theme::term_fg())
                    .absolute(x, y).fill(theme::term_bg()));
            }
        }
        if offset > 0 {
            body = body.child(rune::text(format!("↑ {offset} lines")).font(theme::UI_SMALL).color(theme::on_gold())
                .absolute(self.cols as f32 * theme::cell_w() - 96.0, 2.0).px(6.0).radius(4.0).fill(theme::gold()));
        }
        body
    }
}

/// Pasted text with every bracketed-paste marker taken out, until none is left: taking one out
/// can join its two sides into another (`\x1b[20\x1b[201~1~`), which would end the paste early
/// and run the rest as typed. One scan: a marker is dropped as soon as what is kept ends in one,
/// which catches any nesting without a pass per layer, so a clipboard a box filled (OSC 52) costs
/// its length and no more. termpane also strips the 8-bit forms (`\x9b201~`); text here is a
/// `str`, where U+009B is two bytes no terminal reads as CSI, so there are none.
fn strip_paste_markers(text: &str) -> String {
    const MARKERS: [&str; 2] = ["\x1b[200~", "\x1b[201~"];
    let mut out = String::with_capacity(text.len());
    for c in text.chars() {
        out.push(c);
        if c == '~'
            && let Some(m) = MARKERS.iter().find(|m| out.ends_with(*m))
        {
            out.truncate(out.len() - m.len());
        }
    }
    out
}

/// `query` as a pattern that matches it literally.
fn escape(query: &str) -> String {
    let mut out = String::with_capacity(query.len());
    for c in query.chars() {
        if "\\.+*?()|[]{}^$#&-~".contains(c) {
            out.push('\\');
        }
        out.push(c);
    }
    out
}

/// What marks a cell beyond its own colours.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Default)]
enum Marked {
    #[default]
    No,
    Selected,
    Found,
}

fn mark(point: Point, selection: Option<&SelectionRange>, hit: Option<&Match>) -> Marked {
    if hit.is_some_and(|h| h.contains(&point)) {
        Marked::Found
    } else if selection.is_some_and(|s| s.contains(point)) {
        Marked::Selected
    } else {
        Marked::No
    }
}

/// One cell as it is drawn.
#[derive(Clone, PartialEq)]
struct Drawn {
    c: char,
    /// Combining marks and joiners that follow it in the cell (accents, an emoji's ZWJ parts).
    zero: Vec<char>,
    fg: AColor,
    bg: AColor,
    flags: Flags,
    link: bool,
    marked: Marked,
}

impl Drawn {
    fn of(cell: &Cell, marked: Marked) -> Drawn {
        Drawn {
            c: cell.c, zero: cell.zerowidth().map(<[char]>::to_vec).unwrap_or_default(), fg: cell.fg, bg: cell.bg,
            flags: cell.flags, link: cell.hyperlink().is_some(), marked,
        }
    }
}

fn hash_row(cells: &[Drawn]) -> u64 {
    use std::hash::{Hash, Hasher};
    let mut h = std::collections::hash_map::DefaultHasher::new();
    for d in cells {
        d.c.hash(&mut h);
        d.zero.hash(&mut h);
        format!("{:?}{:?}", d.fg, d.bg).hash(&mut h);
        d.flags.bits().hash(&mut h);
        d.link.hash(&mut h);
        d.marked.hash(&mut h);
    }
    h.finish()
}

fn resolve(c: AColor, fg: bool) -> Option<Color> {
    match c {
        AColor::Spec(rgb) => Some(theme::hex((rgb.r as u32) << 16 | (rgb.g as u32) << 8 | rgb.b as u32)),
        AColor::Indexed(i) => Some(theme::xterm(i)),
        AColor::Named(n) => match n {
            NamedColor::Foreground | NamedColor::BrightForeground | NamedColor::Cursor => fg.then(theme::term_fg),
            NamedColor::DimForeground => Some(theme::grey()),
            NamedColor::Background => None,
            n if (n as usize) < 16 => Some(theme::xterm(n as u8)),
            n => {
                // Dim colours follow the 16 they dim.
                let base = (n as usize).saturating_sub(NamedColor::DimBlack as usize) as u8;
                Some(theme::xterm(base.min(7)).mix(theme::term_bg(), 0.35))
            }
        },
    }
}

/// A row's text in runs, each drawn from the cell it starts at: a wide character (CJK, an
/// emoji) is a run of its own, at its two cells, since the face it falls back to advances by
/// its own measure and not by two of JetBrains Mono's; the narrow text around it is a run each
/// side. `text` holds the run's characters, `spans` its styles over them.
#[derive(Debug, PartialEq)]
struct Run {
    col: usize,
    text: String,
    spans: Vec<Span>,
}

fn runs(cells: &[Drawn]) -> (Vec<Run>, Vec<(usize, usize, Color)>) {
    let mut out: Vec<Run> = vec![];
    let mut backs: Vec<(usize, usize, Color)> = Vec::new();
    let mut cur = Run { col: 0, text: String::new(), spans: vec![] };
    let mut last_ink = 0;
    let flush = |cur: &mut Run, last_ink: &mut usize, out: &mut Vec<Run>, next: usize| {
        cur.text.truncate(*last_ink);
        if !cur.text.is_empty() {
            let spans = merge(std::mem::take(&mut cur.spans));
            out.push(Run { col: cur.col, text: std::mem::take(&mut cur.text), spans });
        }
        *cur = Run { col: next, text: String::new(), spans: vec![] };
        *last_ink = 0;
    };
    for (x, d) in cells.iter().enumerate() {
        if d.flags.contains(Flags::WIDE_CHAR_SPACER) {
            continue;
        }
        let wide = d.flags.contains(Flags::WIDE_CHAR);
        let (mut ink, mut back) = (resolve(d.fg, true).unwrap_or(theme::term_fg()), resolve(d.bg, false));
        if d.flags.contains(Flags::INVERSE) {
            let b = back.unwrap_or(theme::term_bg());
            back = Some(ink);
            ink = b;
        }
        if d.flags.contains(Flags::DIM) {
            ink = ink.mix(theme::term_bg(), 0.4);
        }
        match d.marked {
            Marked::Selected => back = Some(theme::selection()),
            Marked::Found => {
                back = Some(theme::gold());
                ink = theme::on_gold();
            }
            Marked::No => {}
        }
        let width = if wide { 2 } else { 1 };
        if let Some(b) = back {
            match backs.last_mut() {
                Some((_, end, col)) if *end == x && *col == b => *end = x + width,
                _ => backs.push((x, x + width, b)),
            }
        }
        if wide {
            flush(&mut cur, &mut last_ink, &mut out, x);
        }
        let start = cur.text.len();
        let ch = if d.flags.contains(Flags::HIDDEN) || d.c == '\0' { ' ' } else { d.c };
        cur.text.push(ch);
        if ch != ' ' {
            cur.text.extend(&d.zero);
            last_ink = cur.text.len();
            let mut span = Span::new(start, cur.text.len()).color(ink);
            if d.flags.contains(Flags::BOLD) {
                span = span.bold();
            }
            if d.flags.contains(Flags::ITALIC) {
                span = span.italic();
            }
            if d.flags.intersects(Flags::ALL_UNDERLINES) || d.link {
                span = span.underline(ink);
            }
            cur.spans.push(span);
        }
        if wide {
            flush(&mut cur, &mut last_ink, &mut out, x + 2);
        }
    }
    flush(&mut cur, &mut last_ink, &mut out, cells.len());
    (out, backs)
}

fn draw_row(cells: &[Drawn], y: usize) -> El {
    let (runs, backs) = runs(cells);
    let mut row = el().w_full().h(theme::CELL_H).absolute(0.0, y as f32 * theme::CELL_H);
    for (a, b, c) in backs {
        row = row.child(el().absolute(a as f32 * theme::cell_w(), 0.0).size((b - a) as f32 * theme::cell_w(), theme::CELL_H).fill(c));
    }
    for r in runs {
        row = row.child(rich(r.text, r.spans).font(theme::TERM).color(theme::term_fg()).absolute(r.col as f32 * theme::cell_w(), 0.0));
    }
    row
}

/// Adjacent cells of one style become one span.
fn merge(spans: Vec<Span>) -> Vec<Span> {
    let mut out: Vec<Span> = Vec::with_capacity(spans.len());
    for s in spans {
        if let Some(last) = out.last_mut()
            && last.end == s.start
            && same_style(last, &s)
        {
            last.end = s.end;
            continue;
        }
        out.push(s);
    }
    out
}

fn same_style(a: &Span, b: &Span) -> bool {
    let (mut a, mut b) = (a.clone(), b.clone());
    (a.start, a.end, b.start, b.end) = (0, 0, 0, 0);
    a == b
}

#[cfg(test)]
mod tests {
    use super::*;

    fn screen(cols: usize, rows: usize, out: &str) -> Screen {
        crate::theme::set_tokens(&crate::backend::bundled_spec());
        let mut s = Screen::new(cols, rows);
        s.feed(out.as_bytes());
        s
    }

    #[test]
    fn a_drag_selects_and_a_click_lets_it_go() {
        let mut s = screen(20, 4, "hello world\r\nsecond line");
        s.select_start(0, 0, false, 1, false);
        s.select_to(4, 0, true);
        assert_eq!(s.select_end().as_deref(), Some("hello"));
        // Across rows, the line end is a newline.
        s.select_start(6, 0, false, 1, false);
        s.select_to(5, 1, true);
        assert_eq!(s.select_end().as_deref(), Some("world\nsecond"));
        // A click with no drag selects nothing, and lets the last selection go.
        s.select_start(3, 1, false, 1, false);
        assert_eq!(s.select_end(), None);
        assert!(!s.has_selection());
    }

    #[test]
    fn a_double_click_is_a_word_and_a_triple_a_line() {
        let mut s = screen(60, 3, "ls --color=auto /tmp/a.txt https://x.dev/p?q=1 (paren)");
        s.select_start(20, 0, false, 2, false);
        assert_eq!(s.select_end().as_deref(), Some("/tmp/a.txt"));
        s.select_start(5, 0, false, 2, false);
        assert_eq!(s.select_end().as_deref(), Some("--color=auto"));
        s.select_start(30, 0, false, 2, false);
        assert_eq!(s.select_end().as_deref(), Some("https://x.dev/p?q=1"), "a URL is one word");
        s.select_start(50, 0, false, 2, false);
        assert_eq!(s.select_end().as_deref(), Some("paren"), "a bracket splits");
        s.select_start(2, 0, false, 3, false);
        assert_eq!(s.select_end().as_deref(), Some("ls --color=auto /tmp/a.txt https://x.dev/p?q=1 (paren)\n"));
    }

    #[test]
    fn an_alt_drag_selects_a_block() {
        let mut s = screen(10, 3, "abcdef\r\nghijkl\r\nmnopqr");
        s.select_start(1, 0, false, 1, true);
        s.select_to(2, 2, true);
        assert_eq!(s.select_end().as_deref(), Some("bc\nhi\nno"));
    }

    #[test]
    fn a_drag_past_the_top_scrolls_back_and_keeps_selecting() {
        let lines: String = (0..10).map(|i| format!("line{i}\r\n")).collect();
        let mut s = screen(10, 3, &lines);
        assert_eq!(s.offset(), 0);
        s.select_start(4, 1, true, 1, false);
        s.select_to(0, -1, false);
        s.select_to(0, -1, false);
        assert_eq!(s.offset(), 2, "dragging above the view scrolls");
        let text = s.select_end().unwrap();
        assert!(text.starts_with("line6") && text.ends_with("line9"), "{text:?}");
    }

    #[test]
    fn a_programs_copy_is_taken_and_a_read_is_answered_with_silence() {
        let mut s = screen(20, 3, "");
        let reply = s.feed(b"\x1b]52;c;aGVsbG8=\x07");
        assert!(reply.is_empty());
        assert_eq!(s.copied.take().as_deref(), Some("hello"));
        // A clear is no reason to empty the user's clipboard.
        s.feed(b"\x1b]52;c;\x07");
        assert_eq!(s.copied, None);
        // A read gets nothing back.
        assert!(s.feed(b"\x1b]52;c;?\x07").is_empty(), "the clipboard was read");
    }

    #[test]
    fn a_link_a_program_made_is_found_at_its_cells() {
        let s = screen(30, 2, "see \x1b]8;;https://example.com/a\x1b\\here\x1b]8;;\x1b\\ now");
        assert_eq!(s.link_at(4, 0).as_deref(), Some("https://example.com/a"));
        assert_eq!(s.link_at(7, 0).as_deref(), Some("https://example.com/a"));
        assert_eq!(s.link_at(9, 0), None);
        assert_eq!(s.link_at(0, 0), None);
    }

    #[test]
    fn the_mouse_is_reported_only_as_the_program_asked() {
        let mut s = screen(300, 5, "");
        assert_eq!(s.mouse(Button::Left, MouseAct::Press, 1, 1, false, false, false), None, "no mode, no report");
        // X10 click reporting: press and a release that is button 3; no motion.
        s.feed(b"\x1b[?1000h");
        assert_eq!(s.mouse(Button::Left, MouseAct::Press, 0, 0, false, false, false).unwrap(), b"\x1b[M !!");
        assert_eq!(s.mouse(Button::Left, MouseAct::Motion, 1, 0, false, false, false), None, "click mode reports no motion");
        assert_eq!(s.mouse(Button::Left, MouseAct::Release, 1, 0, false, false, false).unwrap(), b"\x1b[M#\"!");
        // Past column 223 a byte cannot say it.
        assert_eq!(s.mouse(Button::Left, MouseAct::Press, 250, 0, false, false, false), None);
        // SGR, with drag reporting and modifiers.
        s.feed(b"\x1b[?1002h\x1b[?1006h");
        assert_eq!(s.mouse(Button::Right, MouseAct::Press, 250, 2, false, false, true).unwrap(), b"\x1b[<18;251;3M");
        assert_eq!(s.mouse(Button::Right, MouseAct::Motion, 251, 2, false, false, false).unwrap(), b"\x1b[<34;252;3M");
        assert_eq!(s.mouse(Button::Right, MouseAct::Release, 251, 2, false, false, false).unwrap(), b"\x1b[<2;252;3m");
        assert_eq!(s.mouse(Button::None, MouseAct::Motion, 3, 3, false, false, false), None, "drag mode reports no bare motion");
        s.feed(b"\x1b[?1003h");
        assert_eq!(s.mouse(Button::None, MouseAct::Motion, 3, 3, false, false, false).unwrap(), b"\x1b[<35;4;4M");
        // Taken back, the program hears nothing.
        s.seized = true;
        assert_eq!(s.mouse(Button::Left, MouseAct::Press, 0, 0, false, false, false), None);
        s.seized = false;
        s.feed(b"\x1b[?1003l\x1b[?1002l\x1b[?1000l");
        assert!(!s.wants_mouse());
    }

    #[test]
    fn a_press_the_program_heard_is_owed_its_release() {
        let mut s = screen(20, 5, "\x1b[?1000h\x1b[?1006h");
        s.mouse(Button::Left, MouseAct::Press, 0, 0, false, false, false).unwrap();
        // The program turns the mouse off mid-press; it still hears the release.
        s.feed(b"\x1b[?1000l");
        assert_eq!(s.mouse(Button::Left, MouseAct::Release, 0, 0, false, false, false).unwrap(), b"\x1b[<0;1;1m");
        assert_eq!(s.mouse(Button::Left, MouseAct::Release, 0, 0, false, false, false), None);
    }

    #[test]
    fn the_wheel_goes_to_whoever_can_scroll() {
        let lines: String = (0..20).map(|i| format!("{i}\r\n")).collect();
        let mut s = screen(10, 4, &lines);
        assert_eq!(s.wheel(WHEEL_LINES, 0, 0), None);
        assert_eq!(s.offset(), 3, "a tick scrolls the scrollback three lines");
        // A trackpad's slow turn is kept until it is a whole line.
        assert_eq!(s.wheel(0.4, 0, 0), None);
        assert_eq!(s.offset(), 3);
        s.wheel(0.7, 0, 0);
        assert_eq!(s.offset(), 4, "the turns added up to a line");
        s.to_bottom();
        // A pager on the alternate screen gets an arrow a line.
        s.feed(b"\x1b[?1049h");
        assert_eq!(s.wheel(2.0, 0, 0).unwrap(), b"\x1b[A\x1b[A");
        s.feed(b"\x1b[?1h");
        assert_eq!(s.wheel(-1.0, 0, 0).unwrap(), b"\x1bOB");
        assert_eq!(s.offset(), 0);
        // A program with the mouse gets a wheel button a tick.
        s.feed(b"\x1b[?1000h\x1b[?1006h");
        assert_eq!(s.wheel(WHEEL_LINES, 1, 2).unwrap(), b"\x1b[<64;2;3M");
        assert_eq!(s.wheel(-2.0 * WHEEL_LINES, 1, 2).unwrap(), b"\x1b[<65;2;3M\x1b[<65;2;3M");
        assert_eq!(s.wheel(1.0, 1, 2), None, "a line is not yet a tick");
        assert_eq!(s.wheel(2.0, 1, 2).unwrap(), b"\x1b[<64;2;3M", "three lines are");
    }

    #[test]
    fn a_resize_drops_a_finds_match_and_the_next_step_finds_again() {
        let lines: String = (0..30).map(|i| format!("{:>90}\r\n", format!("word{i}"))).collect();
        let mut s = screen(100, 5, &lines);
        s.find("word12");
        assert!(s.find.as_ref().unwrap().hit.is_some());
        s.resize(40, 5);
        assert!(s.find.as_ref().unwrap().hit.is_none(), "a match kept across a reflow");
        s.find_next(true);
        assert!(s.find.as_ref().unwrap().hit.is_some(), "the next step finds again");
    }

    #[test]
    fn a_find_opens_again_on_what_it_last_looked_for() {
        let mut s = screen(20, 3, "needle\r\nhay");
        s.open_find();
        assert_eq!(s.find.as_ref().unwrap().query, "");
        s.find("needle");
        s.close_find();
        s.open_find();
        let f = s.find.as_ref().unwrap();
        assert_eq!(f.query, "needle");
        assert!(f.hit.is_some(), "and finds it again");
    }

    #[test]
    fn a_paste_is_bracketed_when_asked_and_cannot_close_itself() {
        let mut s = screen(20, 3, "");
        assert_eq!(s.paste("one\ntwo\r\n"), b"one\rtwo\r");
        s.feed(b"\x1b[?2004h");
        assert_eq!(s.paste("a\x1b[201~rm -rf /\r"), b"\x1b[200~arm -rf /\r\x1b[201~");
        // A marker nested in another comes back together when the outer is taken out once.
        assert_eq!(s.paste("\x1b[20\x1b[201~1~cmd\r"), b"\x1b[200~cmd\r\x1b[201~");
        assert_eq!(s.paste("\x1b[20\x1b[20\x1b[200~0~1~x"), b"\x1b[200~x\x1b[201~");
        // Deep nesting costs a scan, not a pass per layer: a box can fill the clipboard.
        let deep = "\x1b[20".repeat(100_000) + &"1~".repeat(100_000) + "tail";
        let start = std::time::Instant::now();
        assert_eq!(s.paste(&deep), b"\x1b[200~tail\x1b[201~");
        assert!(start.elapsed() < std::time::Duration::from_secs(1), "{:?}", start.elapsed());
    }

    #[test]
    fn find_walks_the_scrollback_and_scrolls_to_each_match() {
        let lines: String = (0..30).map(|i| if i % 10 == 3 { format!("needle {i}\r\n") } else { format!("hay {i}\r\n") }).collect();
        let mut s = screen(20, 4, &lines);
        s.find("Needle");
        assert!(s.find.as_ref().unwrap().missing, "a capital matches only its case");
        s.find("needle");
        let f = s.find.as_ref().unwrap();
        assert!(!f.missing);
        let at = |s: &Screen| { let h = s.find.as_ref().unwrap().hit.clone().unwrap(); (h.start().line.0, s.offset()) };
        let (line, offset) = at(&s);
        assert!((line + offset as i32) >= 0, "the match is on screen");
        assert!(s.row_text((line + offset as i32) as usize).starts_with("needle 23"));
        s.find_next(true);
        let (line, offset) = at(&s);
        assert!(s.row_text((line + offset as i32) as usize).starts_with("needle 13"), "back to the one before");
        s.find_next(false);
        let (line, offset) = at(&s);
        assert!(s.row_text((line + offset as i32) as usize).starts_with("needle 23"), "and on again");
        // Output that scrolls the match into the scrollback keeps it on its text.
        s.find("needle 23");
        let before = s.find.as_ref().unwrap().hit.clone().unwrap();
        s.feed(b"more\r\nand more\r\n");
        let after = s.find.as_ref().unwrap().hit.clone().unwrap();
        assert_eq!(after.start().line.0, before.start().line.0 - 2, "the match moved with its text");
        s.find("a.c");
        assert!(s.find.as_ref().unwrap().missing, "the query is text, not a pattern");
        s.close_find();
        assert!(s.find.is_none());
    }

    #[test]
    fn a_wide_character_is_drawn_at_its_own_cells() {
        let s = screen(12, 2, "a漢b😀c");
        let row = &s.term.grid()[Line(0)];
        let cells: Vec<Drawn> = (0..12).map(|x| Drawn::of(&row[Column(x)], Marked::No)).collect();
        let (r, _) = runs(&cells);
        let at: Vec<(usize, &str)> = r.iter().map(|r| (r.col, r.text.as_str())).collect();
        assert_eq!(at, vec![(0, "a"), (1, "漢"), (3, "b"), (4, "😀"), (6, "c")]);
    }

    #[test]
    fn combining_marks_stay_with_their_character() {
        let s = screen(10, 1, "e\u{301}x");
        let row = &s.term.grid()[Line(0)];
        let cells: Vec<Drawn> = (0..10).map(|x| Drawn::of(&row[Column(x)], Marked::No)).collect();
        let (r, _) = runs(&cells);
        assert_eq!(r.len(), 1);
        assert_eq!(r[0].text, "e\u{301}x");
    }
}
