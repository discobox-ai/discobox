//! One pane's terminal: the bytes a backend stream sends, emulated here, and drawn as rows of
//! monospace runs. This is the native half of "compose the screens from the backend": the
//! sandbox sends what its program wrote, and the window keeps and draws the screen.

use std::sync::{Arc, Mutex};

use alacritty_terminal::event::{Event, EventListener};
use alacritty_terminal::grid::Dimensions;
use alacritty_terminal::index::{Column, Line};
use alacritty_terminal::term::cell::Flags;
use alacritty_terminal::term::{Config, Term, TermMode};
use alacritty_terminal::vte::ansi::{Color as AColor, NamedColor, Processor};
use rune::{Color, El, Span, el, memo, rich};

use crate::theme;

/// What the emulator wants said back to the program (cursor reports, device attributes), and
/// the title it set.
#[derive(Clone, Default)]
pub struct Replies(Arc<Mutex<(Vec<u8>, Option<String>)>>);

impl EventListener for Replies {
    fn send_event(&self, event: Event) {
        let mut r = self.0.lock().unwrap();
        match event {
            Event::PtyWrite(s) => r.0.extend_from_slice(s.as_bytes()),
            Event::Title(t) => r.1 = Some(t),
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

pub struct Screen {
    term: Term<Replies>,
    parser: Processor,
    replies: Replies,
    pub cols: usize,
    pub rows: usize,
    /// Bumped on every change, so rows that did not change can be reused.
    pub version: u64,
    pub title: Option<String>,
}

impl Screen {
    pub fn new(cols: usize, rows: usize) -> Screen {
        let replies = Replies::default();
        let config = Config { scrolling_history: 5000, ..Config::default() };
        let term = Term::new(config, &Size { cols, rows }, replies.clone());
        Screen { term, parser: Processor::new(), replies, cols, rows, version: 0, title: None }
    }

    /// Feeds program output; returns what the emulator answers back, for the stream's input.
    pub fn feed(&mut self, bytes: &[u8]) -> Vec<u8> {
        self.parser.advance(&mut self.term, bytes);
        self.version += 1;
        let mut r = self.replies.0.lock().unwrap();
        if let Some(t) = r.1.take() {
            self.title = Some(t);
        }
        std::mem::take(&mut r.0)
    }

    pub fn resize(&mut self, cols: usize, rows: usize) -> bool {
        let (cols, rows) = (cols.max(2), rows.max(1));
        if (cols, rows) == (self.cols, self.rows) {
            return false;
        }
        self.term.resize(Size { cols, rows });
        (self.cols, self.rows) = (cols, rows);
        self.version += 1;
        true
    }

    pub fn app_cursor(&self) -> bool { self.term.mode().contains(TermMode::APP_CURSOR) }

    pub fn bracketed_paste(&self) -> bool { self.term.mode().contains(TermMode::BRACKETED_PASTE) }

    pub fn to_bottom(&mut self) {
        if self.term.grid().display_offset() != 0 {
            self.term.scroll_display(alacritty_terminal::grid::Scroll::Bottom);
            self.version += 1;
        }
    }

    pub fn scroll(&mut self, lines: i32) {
        self.term.scroll_display(alacritty_terminal::grid::Scroll::Delta(lines));
        self.version += 1;
    }

    /// The screen as elements: one keyed, memoised row per line, then the cursor.
    pub fn view(&self, key: &str, focused: bool) -> El {
        let grid = self.term.grid();
        let offset = grid.display_offset() as i32;
        let mut body = el().w(self.cols as f32 * theme::cell_w()).h(self.rows as f32 * theme::CELL_H);
        for y in 0..self.rows {
            let line = Line(y as i32 - offset);
            let row = &grid[line];
            let cells: Vec<(char, AColor, AColor, Flags)> =
                (0..self.cols).map(|x| { let c = &row[Column(x)]; (c.c, c.fg, c.bg, c.flags) }).collect();
            // The palette a row is drawn in is part of every memo's key already: Rune hashes the
            // window's appearance into it.
            let version = hash_row(&cells);
            body = body.child(memo(format!("{key}-r{y}"), version, || draw_row(&cells, y)));
        }
        let cursor = self.term.renderable_content().cursor;
        let show = self.term.mode().contains(TermMode::SHOW_CURSOR) && offset == 0;
        if show && cursor.point.line.0 >= 0 {
            let (x, y) = (cursor.point.column.0 as f32 * theme::cell_w(), cursor.point.line.0 as f32 * theme::CELL_H);
            let c = el().key(format!("{key}-cursor")).absolute(x, y).size(theme::cell_w(), theme::CELL_H);
            body = body.child(if focused { c.fill(theme::term_fg().alpha(0.85)) } else { c.border(1.0, theme::grey()) });
        }
        if offset > 0 {
            body = body.child(rune::text(format!("↑ {offset} lines")).font(theme::UI_SMALL).color(theme::on_gold())
                .absolute(self.cols as f32 * theme::cell_w() - 96.0, 2.0).px(6.0).radius(4.0).fill(theme::gold()));
        }
        body
    }
}

fn hash_row(cells: &[(char, AColor, AColor, Flags)]) -> u64 {
    use std::hash::{Hash, Hasher};
    let mut h = std::collections::hash_map::DefaultHasher::new();
    for (c, fg, bg, f) in cells {
        c.hash(&mut h);
        format!("{fg:?}{bg:?}").hash(&mut h);
        f.bits().hash(&mut h);
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

fn draw_row(cells: &[(char, AColor, AColor, Flags)], y: usize) -> El {
    let mut text = String::new();
    let mut spans = Vec::new();
    let mut backs: Vec<(usize, usize, Color)> = Vec::new();
    let mut last_ink = 0;
    for (x, &(c, fg, bg, flags)) in cells.iter().enumerate() {
        if flags.contains(Flags::WIDE_CHAR_SPACER) {
            continue;
        }
        let (mut ink, mut back) = (resolve(fg, true).unwrap_or(theme::term_fg()), resolve(bg, false));
        if flags.contains(Flags::INVERSE) {
            let b = back.unwrap_or(theme::term_bg());
            back = Some(ink);
            ink = b;
        }
        if flags.contains(Flags::DIM) {
            ink = ink.mix(theme::term_bg(), 0.4);
        }
        if let Some(b) = back {
            match backs.last_mut() {
                Some((_, end, col)) if *end == x && *col == b => *end = x + 1,
                _ => backs.push((x, x + 1, b)),
            }
        }
        let start = text.len();
        let ch = if flags.contains(Flags::HIDDEN) || c == '\0' { ' ' } else { c };
        text.push(ch);
        if ch != ' ' {
            last_ink = text.len();
            let mut span = Span::new(start, text.len()).color(ink);
            if flags.contains(Flags::BOLD) {
                span = span.bold();
            }
            if flags.contains(Flags::ITALIC) {
                span = span.italic();
            }
            if flags.intersects(Flags::ALL_UNDERLINES) {
                span = span.underline(ink);
            }
            spans.push(span);
        }
    }
    text.truncate(last_ink);
    let mut row = el().w_full().h(theme::CELL_H).absolute(0.0, y as f32 * theme::CELL_H);
    for (a, b, c) in backs {
        row = row.child(el().absolute(a as f32 * theme::cell_w(), 0.0).size((b - a) as f32 * theme::cell_w(), theme::CELL_H).fill(c));
    }
    if !text.is_empty() {
        row = row.child(rich(text, merge(spans)).font(theme::TERM).color(theme::term_fg()).absolute(0.0, 0.0));
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
