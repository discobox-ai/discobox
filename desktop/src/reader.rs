//! A read-only card's own keys, as the TUI's text cards have them: the help, a harness's config
//! card, and an audit record or recording read in full. Each is found through (`/` or the find
//! gesture: what is typed is lit wherever it is, `n` and `N` step through it), copied whole (`c`
//! or the copy gesture), and scrolled (the arrows, `j` and `k` a line at a time; the page keys,
//! Home and End).
//!
//! A card reads as pieces: the texts it draws, in the order it draws them (`front`). What is
//! found is lit in each piece with Rune's text highlights, the one stepped to keyed
//! [`FOUND`] so the card's scroll centres it. The line keys scroll a line on from where the
//! card is scrolled to, which its scroll reports as it moves (`follow_scroll`), the wheel and the
//! page keys included. One card is in front at a time, so there is one reader: it starts again
//! whenever the card in front is another.

use std::ops::Range;

use rune::{El, Key, KeyEvent, Live, ScrollPos, TextEdit, TextHighlight, Tx, icon, row, text, text_input};

use crate::{State, audit, icons, keys, overlay, theme, view};

/// The key of the match stepped to: the card's scroll centres it.
pub const FOUND: &str = "read-found";

/// How far a line key scrolls: a line of the cards' text.
pub const LINE: f32 = 20.0;

/// What is being looked for in the card in front, and where its keys have got to.
pub struct Reader {
    /// Which card it reads (`front`'s name for it): another in front starts it again.
    of: String,
    pub find: TextEdit,
    /// The match stepped to, of every match in the card's pieces in order.
    pub at: usize,
    /// How far the card is scrolled, as its scroll last reported it.
    pub offset: Live<f32>,
    /// What the copy said, until the next key.
    pub said: Option<String>,
}

impl Default for Reader {
    fn default() -> Reader { Reader { of: String::new(), find: TextEdit::default(), at: 0, offset: Live::new(0.0), said: None } }
}

/// Has a card's scroll report where it is scrolled to, for the line keys.
pub fn follow_scroll(s: &State, scroll: El) -> El {
    let offset = s.reader.offset.clone();
    scroll.on_scroll_offset(move |at: ScrollPos| {
        offset.set(at.offset[1]);
    })
}

/// The read-only card in front of the window: what it is called (another one starts the reader
/// again), the scroll its pieces are in, its pieces, and what copying it copies.
pub struct Front {
    pub name: String,
    pub scroll: &'static str,
    pub pieces: Vec<String>,
    pub copy: String,
}

/// The read-only card in front, in the order the keys reach them: the help, then a harness's
/// card, then what the audit view read.
pub fn front(s: &State) -> Option<Front> {
    if s.help {
        let lines = overlay::help_lines(s);
        let copy = lines.iter().map(|(menu, label, key)| format!("{menu}\t{label}\t{key}")).collect::<Vec<_>>().join("\n");
        return Some(Front { name: "help".into(), scroll: "help-body", pieces: lines.into_iter().map(|(_, label, _)| label).collect(), copy });
    }
    if let Some(c) = &s.card {
        let pieces = crate::harnesses::card_pieces(c);
        return Some(Front { name: format!("card:{}", c.title), scroll: "harness-card-rows", copy: crate::harnesses::card_text(c), pieces });
    }
    let top = s.audit.as_ref()?.reading.last()?;
    let body = audit::reading_body(&top.text.text);
    Some(Front {
        name: format!("audit:{}:{:?}", top.text.record, top.text.part),
        scroll: "audit-text",
        pieces: body.split('\n').map(str::to_string).collect(),
        copy: body,
    })
}

/// Starts the reader again when the card in front is another than the one it read.
pub fn follow(s: &mut State) {
    let name = front(s).map(|f| f.name).unwrap_or_default();
    if s.reader.of != name {
        s.reader = Reader { of: name, ..Default::default() };
    }
}

/// Each match of `query` in `piece`, as byte ranges, ASCII letters folding case.
fn ranges(piece: &str, query: &str) -> Vec<Range<usize>> {
    if query.is_empty() {
        return vec![];
    }
    let (hay, needle) = (piece.to_ascii_lowercase(), query.to_ascii_lowercase());
    hay.match_indices(&needle).map(|(at, m)| at..at + m.len()).collect()
}

/// How many matches every piece holds, in order.
fn counts(pieces: &[String], query: &str) -> Vec<usize> { pieces.iter().map(|p| ranges(p, query).len()).collect() }

/// What a card says about its find: how many matches, and which is stepped to.
pub fn tally(s: &State, pieces: &[String]) -> String {
    let query = s.reader.find.text.as_str();
    if query.is_empty() {
        return String::new();
    }
    match counts(pieces, query).iter().sum::<usize>() {
        0 => "no match".into(),
        n => format!("{} of {n}", s.reader.at % n + 1),
    }
}

/// The highlights a text element draws for the pieces it holds: each piece's matches, the one
/// stepped to keyed [`FOUND`]. `pieces` are the card's, `held` the indices of the ones in this
/// element, and `offsets` where each starts in its text.
pub fn highlights(s: &State, pieces: &[String], held: Range<usize>, offsets: &[usize]) -> Vec<TextHighlight> {
    let query = s.reader.find.text.as_str();
    let counts = counts(pieces, query);
    let total: usize = counts.iter().sum();
    let mut before: usize = counts[..held.start.min(counts.len())].iter().sum();
    let mut out = vec![];
    for (n, i) in held.enumerate() {
        let Some(piece) = pieces.get(i) else { break };
        let at = offsets.get(n).copied().unwrap_or(0);
        for r in ranges(piece, query) {
            let found = total > 0 && before == s.reader.at % total;
            let lit = TextHighlight::new(at + r.start..at + r.end, if found { theme::gold().alpha(0.55) } else { theme::gold().alpha(0.22) });
            out.push(if found { lit.key(FOUND) } else { lit });
            before += 1;
        }
    }
    out
}

/// One piece drawn as a text element of its own: `t` lit with its matches.
pub fn lit(s: &State, pieces: &[String], i: usize, t: El) -> El { t.text_highlights(highlights(s, pieces, i..i + 1, &[0])) }

/// Opens the find on the card in front, its field taking the keys.
fn open_find(s: &mut State) {
    s.reader.find.focused = true;
    s.reader.find.focus = s.reader.find.text.len();
    s.reader.find.anchor = 0;
}

/// Steps `by` matches along, wrapping, and centres the one stepped to.
fn step(s: &mut State, by: i64, tx: &mut Tx) {
    let Some(f) = front(s) else { return };
    let total: usize = counts(&f.pieces, &s.reader.find.text).iter().sum();
    if total == 0 {
        return;
    }
    s.reader.at = (s.reader.at as i64 + by).rem_euclid(total as i64) as usize;
    tx.scroll_center_in(f.scroll, FOUND);
}

/// Scrolls the card `by` lines on from where it is scrolled to; its scroll keeps it in bounds.
fn line(s: &mut State, by: f32, tx: &mut Tx) {
    let Some(f) = front(s) else { return };
    tx.scroll_to_y(f.scroll, (s.reader.offset.get() + by * LINE).max(0.0));
}

/// The keys of the card in front, its own: true for one it took. Esc and what else closes or
/// moves the card are the card's.
pub fn key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool {
    follow(s);
    let Some(f) = front(s) else { return false };
    s.reader.said = None;
    if keys::is_gesture(s, "find", ev, tx) {
        open_find(s);
        return true;
    }
    if keys::is_gesture(s, "copy", ev, tx) {
        copy(s, f.copy, tx);
        return true;
    }
    if ev.mods.ctrl || ev.mods.alt || ev.mods.cmd {
        return false;
    }
    match &ev.key {
        Key::Up => line(s, -1.0, tx),
        Key::Down => line(s, 1.0, tx),
        Key::PageUp => tx.scroll_page(f.scroll, -1),
        Key::PageDown => tx.scroll_page(f.scroll, 1),
        Key::Home => tx.scroll_to_y(f.scroll, 0.0),
        Key::End => tx.scroll_to_bottom(f.scroll),
        Key::Text(t) => match t.as_str() {
            "/" => open_find(s),
            "n" => step(s, 1, tx),
            "N" => step(s, -1, tx),
            "c" => copy(s, f.copy, tx),
            "k" => line(s, -1.0, tx),
            "j" => line(s, 1.0, tx),
            _ => return false,
        },
        _ => return false,
    }
    true
}

fn copy(s: &mut State, text: String, tx: &mut Tx) {
    tx.copy(text);
    s.reader.said = Some("copied".into());
}

/// The find field's own keys: Enter keeps what it found and gives the keys back to the card, Esc
/// lets the find go; Up and Down step through what it found.
fn find_key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool {
    match ev.key {
        Key::Enter if ev.mods.shift => step(s, -1, tx),
        Key::Enter => s.reader.find.focused = false,
        Key::Escape => s.reader.find = TextEdit::new(""),
        Key::Up => step(s, -1, tx),
        Key::Down => step(s, 1, tx),
        _ => return false,
    }
    true
}

/// The card's foot: the find while it is open, with how many it found, and the keys.
pub fn foot(s: &State, pieces: &[String]) -> El {
    let mut out = row().key("read-foot").w_full().gap(12.0).center_y();
    let r = &s.reader;
    if r.find.focused || !r.find.text.is_empty() {
        let field = text_input(&r.find, theme::UI.lh(20.0)).key("read-find").single_line().grow(1.0).color(theme::text())
            .placeholder("find in it", theme::dim())
            .on_edit(|s: &mut State| &mut s.reader.find)
            .on_input_change(|s: &mut State, tx: &mut Tx| {
                s.reader.at = 0;
                if let Some(f) = front(s) {
                    tx.scroll_center_in(f.scroll, FOUND);
                }
            })
            .on_input_key(find_key);
        out = out.child(row().grow(1.0).h(28.0).px(8.0).gap(8.0).center_y().radius(theme::r_row()).fill(theme::sunken())
            .border(theme::stroke_quiet(), if r.find.focused { theme::mark() } else { theme::line_bright() })
            .child(icon(&icons::SEARCH, 14.0).color(theme::grey()))
            .child(field)
            .child(text(tally(s, pieces)).font(theme::UI_SMALL).color(theme::dim())));
        if !r.find.focused {
            out = out.child(hint("n", "next")).child(hint("shift+n", "previous"));
        }
    } else {
        out = out.child(hint("/", "find")).child(hint("c", "copy")).child(hint("up", "scroll"));
    }
    if let Some(said) = &r.said {
        out = out.child(text(said.clone()).font(theme::UI_SMALL).color(theme::grey()));
    }
    out
}

fn hint(key: &str, what: &str) -> El {
    row().gap(4.0).center_y().child(view::keycap(keys::label(key))).child(text(what.to_string()).font(theme::UI_SMALL).color(theme::dim()))
}

#[cfg(test)]
#[path = "reader_tests.rs"]
mod tests;
