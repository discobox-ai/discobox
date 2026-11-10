//! Sheets: the core's forms and questions, drawn as the desktop draws a dialog that a command
//! waits on (ADR 26-11-07-876 §1, 26-11-07-289 §2).
//!
//! What a form asks, what it opens on, when a field applies and whether the answers are
//! acceptable are the core's: it sends the form with each field's value as the core reads it, the
//! app keeps what is typed and chosen and where the keys are, sends a draft as an answer changes
//! (the form comes back with `applies` recomputed), and submits only what the person changed —
//! every other field answers what it opened holding, so an untouched field is never an edit. A
//! refusal comes back as field errors, said under the field it names, with what was typed kept. A
//! question is a yes or no, or a pick of one; it is answered exactly once. A yes or no dismissed
//! is answered no, and only a pick of one is declined (the core's rule). A sheet goes when its
//! command's result lands.
//!
//! An edit is a file a command needs edited (the core's `edit`): it is opened in a text editor
//! (`open_in_editor`), and its sheet waits for them to say they
//! are finished with it — Done answers `saved`, and Esc gives it up, which saves nothing.
//!
//! A form's fields can grow as they are answered (a repeated run option gains a field once its
//! last is filled): the form comes back after a draft with more fields, and what was typed,
//! chosen and changed is kept by key. Its note says what the answers add up to (the command a
//! create would run), and its action is what its button says.
//!
//! Sheets queue: a question can arrive for one command while another's form is up, and the first
//! to arrive is drawn until it is answered.

use std::collections::HashMap;

use rune::{Color, El, Key, KeyEvent, ScrollSpec, TextEdit, Tx, col, el, row, spacer, text, text_input};

use crate::backend::Out;
use crate::wire::{Answer, Edit, Field, FieldError, FieldErrors, Form, Question, Saved, Submit};
use crate::{State, keys, theme, view};

/// A form a command waits on: the core's fields, and the app's answers to them.
pub struct FormSheet {
    pub id: u64,
    pub command: u64,
    pub title: String,
    pub fields: Vec<Field>,
    /// What the answers so far add up to, as the core says it; and what the submit button says
    /// (the app's own word when the core names none).
    pub note: String,
    pub action: String,
    /// What the form is about, as the core says it, above its fields.
    pub about: String,
    /// What each typed field holds; a picker's is unused.
    pub edits: Vec<TextEdit>,
    /// Each picker's chosen option, by key; a typed field's is unused.
    pub chosen: Vec<String>,
    /// The fields the person has changed: only their answers are sent.
    touched: Vec<bool>,
    /// The field the keys are on, when no typed field has the caret.
    at: usize,
    /// Why the core refused what was submitted last.
    pub errors: Vec<FieldError>,
    /// Submitted, and not yet accepted or refused.
    pub sent: bool,
    /// The field the keys moved to, to scroll into view on the next frame (`Sheets::reveal`):
    /// the fields scroll inside the sheet, and the keys can move to one out of sight.
    pub reveal: Option<String>,
}

/// A question a command waits on, and the answer the keys are on: for a yes or no, 0 is yes and 1
/// is no; for a pick of one, the choice's place.
pub struct QuestionSheet {
    pub question: Question,
    pub picked: usize,
}

/// A file a command needs edited, open in the person's editor; or why it could not be opened.
pub struct EditSheet {
    pub edit: Edit,
    pub opened: Result<(), String>,
}

pub enum Sheet {
    Form(FormSheet),
    Question(QuestionSheet),
    Edit(EditSheet),
}

impl Sheet {
    /// The command the sheet's answer goes to.
    pub fn command(&self) -> u64 {
        match self {
            Sheet::Form(f) => f.command,
            Sheet::Question(q) => q.question.command,
            Sheet::Edit(e) => e.edit.command,
        }
    }
}

/// The sheets waiting, the first drawn.
#[derive(Default)]
pub struct Sheets {
    pub queue: Vec<Sheet>,
    /// What a field reads from in a frame drawn after its sheet went.
    spare: TextEdit,
    /// The questions last answered or dismissed: one sent again as its count climbs
    /// (`Question::count`) can cross its answer on the pipe, and is moot.
    answered: Vec<u64>,
}

impl Sheets {
    pub fn is_open(&self) -> bool { !self.queue.is_empty() }

    pub fn form(&self) -> Option<&FormSheet> {
        match self.queue.first() {
            Some(Sheet::Form(f)) => Some(f),
            _ => None,
        }
    }

    fn form_mut(&mut self) -> Option<&mut FormSheet> {
        match self.queue.first_mut() {
            Some(Sheet::Form(f)) => Some(f),
            _ => None,
        }
    }

    pub fn question(&self) -> Option<&QuestionSheet> {
        match self.queue.first() {
            Some(Sheet::Question(q)) => Some(q),
            _ => None,
        }
    }

    pub fn edit_sheet(&self) -> Option<&EditSheet> {
        match self.queue.first() {
            Some(Sheet::Edit(e)) => Some(e),
            _ => None,
        }
    }

    fn question_mut(&mut self) -> Option<&mut QuestionSheet> {
        match self.queue.first_mut() {
            Some(Sheet::Question(q)) => Some(q),
            _ => None,
        }
    }

    /// The top form's typed field `i`.
    fn edit(&mut self, i: usize) -> &mut TextEdit {
        match self.queue.first_mut() {
            Some(Sheet::Form(f)) if i < f.edits.len() => &mut f.edits[i],
            _ => &mut self.spare,
        }
    }

    /// Takes away the sheets of a command that has finished: whatever they asked is moot now.
    pub fn finished(&mut self, command: u64) { self.queue.retain(|s| s.command() != command); }

    /// Scrolls the field the keys last moved to into view, once: called each frame.
    pub fn reveal(&mut self, tx: &mut Tx) {
        if let Some(Sheet::Form(f)) = self.queue.first_mut()
            && let Some(key) = f.reveal.take()
        {
            tx.scroll_into_view_in("sheet-fields", row_key(&key));
        }
    }

    /// A command whose form was submitted has gone on — it asks something else, has a file
    /// edited, or says a step:
    /// the core took the form, which goes, rather than staying over what the command asks next
    /// until its result lands (a create's questions come long before its result).
    pub fn went_on(&mut self, command: u64) {
        self.queue.retain(|s| !matches!(s, Sheet::Form(f) if f.command == command && f.sent));
    }

    /// Takes the sheet on top away, keeping a question's ID among those answered.
    fn take_top(&mut self) -> Sheet {
        let top = self.queue.remove(0);
        if let Sheet::Question(q) = &top {
            if self.answered.len() == ANSWERED_KEPT {
                self.answered.remove(0);
            }
            self.answered.push(q.question.id);
        }
        top
    }
}

/// How many answered questions are remembered: a resend crosses only the answer just sent.
const ANSWERED_KEPT: usize = 16;

/// The key of a field's row in the sheet, which scrolling reveals.
fn row_key(field: &str) -> String { format!("sheet-row-{field}") }

/// Whether a field takes an answer: one locked, or that does not apply, is drawn with why.
fn answerable(f: &Field) -> bool { f.applies && !f.locked }

fn picker(f: &Field) -> bool { !f.choices.is_empty() }

impl FormSheet {
    fn new(form: Form) -> FormSheet {
        let edits = form.fields.iter().map(|f| {
            let e = TextEdit::new(f.value.clone());
            if f.masked { e.secure() } else { e }
        }).collect();
        // A picker's value is the key the core reads it holding.
        let chosen = form.fields.iter().map(|f| f.value.clone()).collect();
        let touched = vec![false; form.fields.len()];
        let mut sheet = FormSheet {
            id: form.id, command: form.command, title: form.title, fields: form.fields, note: form.note, action: form.action, about: form.about, edits, chosen, touched,
            at: 0, errors: vec![], sent: false, reveal: None,
        };
        if let Some(i) = sheet.fields.iter().position(answerable) {
            sheet.focus(i);
        }
        sheet
    }

    /// The field the keys are on: the typed field with the caret, else the picker last moved to.
    pub fn at(&self) -> usize { self.edits.iter().position(|e| e.focused).unwrap_or(self.at) }

    /// Puts the keys on a field: the caret in a typed one, none in a picker. True when they are
    /// on a picker now, whose caller lets the window's focus go (`Tx::blur`): a field that had
    /// the caret keeps it otherwise.
    fn focus(&mut self, i: usize) -> bool {
        for e in &mut self.edits {
            e.focused = false;
        }
        self.at = i;
        self.reveal = self.fields.get(i).map(|f| f.key.clone());
        match self.fields.get(i) {
            Some(f) if !picker(f) => {
                self.edits[i].focused = true;
                false
            }
            _ => true,
        }
    }

    /// Moves the keys `by` answerable fields along, wrapping; true when they land on a picker.
    fn step(&mut self, by: i64) -> bool {
        let open: Vec<usize> = (0..self.fields.len()).filter(|&i| answerable(&self.fields[i])).collect();
        if open.is_empty() {
            return false;
        }
        let at = self.at();
        let here = open.iter().position(|&i| i == at).map_or(if by > 0 { -1 } else { 0 }, |p| p as i64);
        let next = open[(here + by).rem_euclid(open.len() as i64) as usize];
        self.focus(next)
    }

    /// The answers the person gave, by key: a changed typed field's text, a changed picker's
    /// key. The core reads every other field as it opened (the bridge's merge onto the form's
    /// opening), so nothing here re-derives what an untouched field holds.
    pub fn answers(&self) -> HashMap<String, String> {
        self.fields.iter().enumerate().filter(|&(i, f)| self.touched[i] && !f.locked).map(|(i, f)| {
            let v = if picker(f) { self.chosen[i].clone() } else { self.edits[i].text.clone() };
            (f.key.clone(), v)
        }).collect()
    }

    /// Why the core refused a field's answer, or the answers as a whole (`""`).
    pub fn error(&self, field: &str) -> Option<&str> { self.errors.iter().find(|e| e.field == field).map(|e| e.why.as_str()) }
}

/// A form from the core: a new one goes to the back of the queue; the same one again — the answer
/// to a draft — takes its fields' standing and keeps what was typed and chosen.
pub fn take_form(s: &mut State, form: Form) {
    s.close_overlays();
    if !s.sheets.queue.iter().any(|sh| matches!(sh, Sheet::Form(f) if f.id == form.id)) {
        s.sheets.went_on(form.command);
    }
    let at = s.sheets.queue.iter_mut().find_map(|sh| match sh {
        Sheet::Form(f) if f.id == form.id => Some(f),
        _ => None,
    });
    match at {
        Some(f) => f.refit(form),
        None => s.sheets.queue.push(Sheet::Form(FormSheet::new(form))),
    }
}

impl FormSheet {
    /// The same form again, after a draft: its fields' standing and its note are the core's
    /// now, and what was typed, chosen and changed stays with each field by its key — a field
    /// the form grew opens as the core sends it, and the keys stay on the field they were on.
    fn refit(&mut self, form: Form) {
        let at = self.fields.get(self.at()).map(|f| f.key.clone());
        let mut held: HashMap<String, (TextEdit, String, bool)> = HashMap::new();
        for (i, f) in self.fields.iter().enumerate() {
            held.insert(f.key.clone(), (std::mem::take(&mut self.edits[i]), self.chosen[i].clone(), self.touched[i]));
        }
        let fresh = FormSheet::new(Form { id: self.id, command: self.command, ..form });
        let (mut edits, mut chosen, mut touched) = (fresh.edits, fresh.chosen, fresh.touched);
        for (i, f) in fresh.fields.iter().enumerate() {
            if let Some((edit, choice, changed)) = held.remove(&f.key) {
                (edits[i], chosen[i], touched[i]) = (edit, choice, changed);
            }
        }
        (self.fields, self.title, self.note, self.action, self.about) = (fresh.fields, fresh.title, fresh.note, fresh.action, fresh.about);
        (self.edits, self.chosen, self.touched) = (edits, chosen, touched);
        match at.and_then(|k| self.fields.iter().position(|f| f.key == k)) {
            Some(i) => self.at = i,
            None => self.at = 0,
        }
        // A refusal of a field that no longer applies is moot, and the keys leave it.
        let open: Vec<String> = self.fields.iter().filter(|f| answerable(f)).map(|f| f.key.clone()).collect();
        self.errors.retain(|e| e.field.is_empty() || open.contains(&e.field));
        if !self.fields.get(self.at()).is_some_and(answerable) {
            self.step(1);
        }
    }
}

/// The core's refusal of what was submitted: the form stays up holding it, the keys on the field
/// it names.
pub fn take_errors(s: &mut State, refused: FieldErrors) {
    let Some(f) = s.sheets.queue.iter_mut().find_map(|sh| match sh {
        Sheet::Form(f) if f.id == refused.form => Some(f),
        _ => None,
    }) else {
        return;
    };
    f.sent = false;
    if let Some(i) = refused.errors.iter().find_map(|e| f.fields.iter().position(|x| x.key == e.field)) {
        f.focus(i);
    }
    f.errors = refused.errors;
}

/// A question from the core, on the yes when it has one — or on the no, for a destruction that
/// has to be chosen. One sent again is the question asked, with its directory counted further
/// (`count`): it is updated where it is, the pick kept; one already answered is moot.
pub fn take_question(s: &mut State, question: Question) {
    if s.sheets.answered.contains(&question.id) {
        return;
    }
    let asked = s.sheets.queue.iter_mut().find_map(|sh| match sh {
        Sheet::Question(q) if q.question.id == question.id => Some(q),
        _ => None,
    });
    if let Some(q) = asked {
        q.question = question;
        return;
    }
    s.close_overlays();
    s.sheets.went_on(question.command);
    let picked = usize::from(question.choices.is_empty() && question.default_no);
    s.sheets.queue.push(Sheet::Question(QuestionSheet { question, picked }));
}

/// A file to edit, from the core: opened in the person's editor at once, and waited on.
pub fn take_edit(s: &mut State, edit: Edit) {
    s.close_overlays();
    s.message = None;
    s.sheets.went_on(edit.command);
    let opened = open_in_editor(&edit.path);
    s.sheets.queue.push(Sheet::Edit(EditSheet { edit, opened }));
}

/// Opens the edit on top in the editor again: one closed before it was saved, or that did not
/// come up.
fn reopen(s: &mut State) {
    if let Some(Sheet::Edit(e)) = s.sheets.queue.first_mut() {
        e.opened = open_in_editor(&e.edit.path);
    }
}

/// The person is finished with the edit on top: the command reads back what was saved.
fn finish_edit(s: &mut State) {
    let Some(Sheet::Edit(e)) = s.sheets.queue.first() else { return };
    let edit = e.edit.id;
    s.sheets.queue.remove(0);
    s.backend.send(Out::Saved(Saved { edit, ..Default::default() }));
}

/// Opens a file in a text editor: the system's default text editor on macOS (`open -t`), Notepad
/// on Windows, and on Linux the handler for its type (`xdg-open`), which the CLI makes a text
/// format's (its temp file takes `.txt` for any extension that is not one). The path is never
/// handed to a shell or to the system's file association by extension alone: a harness bundle
/// names the file, and must not get to choose a program to run here. Nothing it runs writes to
/// the app's stdin or stdout, which are the bridge's.
#[cfg(not(test))]
pub fn open_in_editor(path: &str) -> Result<(), String> {
    use std::process::{Command, Stdio};
    let mut c = if cfg!(target_os = "macos") {
        let mut c = Command::new("open");
        c.args(["-t", path]);
        c
    } else if cfg!(windows) {
        let mut c = Command::new("notepad.exe");
        c.arg(path);
        c
    } else {
        let mut c = Command::new("xdg-open");
        c.arg(path);
        c
    };
    let mut child = c.stdin(Stdio::null()).stdout(Stdio::null()).stderr(Stdio::null()).spawn()
        .map_err(|e| format!("could not open it in your editor: {e}"))?;
    // Reaped once the opener exits, which is soon: it hands the file on and goes.
    std::thread::spawn(move || {
        let _ = child.wait();
    });
    Ok(())
}

/// In a test nothing is opened: what would have been is kept, for the test to read.
#[cfg(test)]
pub fn open_in_editor(path: &str) -> Result<(), String> {
    OPENED.with_borrow_mut(|o| o.push(path.to_string()));
    Ok(())
}

#[cfg(test)]
thread_local! {
    pub static OPENED: std::cell::RefCell<Vec<String>> = const { std::cell::RefCell::new(Vec::new()) };
}

fn submit(s: &mut State, draft: bool) {
    let Some(f) = s.sheets.form_mut() else { return };
    if f.sent && !draft {
        return;
    }
    if !draft {
        f.sent = true;
    }
    let m = Submit { form: f.id, answers: f.answers(), draft, ..Default::default() };
    s.backend.send(Out::Submit(m));
}

/// Field `i`'s answer changed: it is sent from now on, its refusal goes, and the answers go as a
/// draft for the core to say again which fields apply.
fn changed(s: &mut State, i: usize) {
    if let Some(f) = s.sheets.form_mut()
        && i < f.fields.len()
    {
        f.touched[i] = true;
        let key = f.fields[i].key.clone();
        f.errors.retain(|e| e.field != key);
    }
    submit(s, true);
}

/// Dismisses the sheet on top: a form is cancelled; a yes or no is answered no, since leaving it
/// is no (the core's rule, as the TUI answers it), and a pick of one is declined.
pub fn dismiss(s: &mut State) {
    if s.sheets.queue.is_empty() {
        return;
    }
    let out = dismissal(&s.sheets.take_top());
    s.backend.send(out);
}

/// What dismissing a sheet sends.
pub fn dismissal(sheet: &Sheet) -> Out {
    match sheet {
        Sheet::Form(f) => Out::Submit(Submit { form: f.id, cancel: true, ..Default::default() }),
        Sheet::Question(q) if q.question.choices.is_empty() => Out::Answer(Answer { question: q.question.id, key: "no".into(), ..Default::default() }),
        Sheet::Question(q) => Out::Answer(Answer { question: q.question.id, declined: true, ..Default::default() }),
        Sheet::Edit(e) => Out::Saved(Saved { edit: e.edit.id, canceled: true, ..Default::default() }),
    }
}

/// Answers the question on top, by key.
fn answer(s: &mut State, key: String) {
    let Some(Sheet::Question(q)) = s.sheets.queue.first() else { return };
    let question = q.question.id;
    s.sheets.take_top();
    s.backend.send(Out::Answer(Answer { question, key, ..Default::default() }));
}

/// The answer a question's pick stands for.
fn picked_key(q: &QuestionSheet) -> String {
    if q.question.choices.is_empty() {
        return if q.picked == 0 { "yes" } else { "no" }.into();
    }
    q.question.choices.get(q.picked).map(|c| c.key.clone()).unwrap_or_default()
}

/// Moves a picker's choice `by` along, wrapping.
fn cycle(s: &mut State, i: usize, by: i64) {
    let Some(f) = s.sheets.form_mut() else { return };
    let Some(field) = f.fields.get(i).filter(|x| picker(x) && answerable(x)) else { return };
    let n = field.choices.len() as i64;
    let at = field.choices.iter().position(|c| c.key == f.chosen[i]).unwrap_or(0) as i64;
    f.chosen[i] = field.choices[(at + by).rem_euclid(n) as usize].key.clone();
    changed(s, i);
}

fn choose(s: &mut State, i: usize, key: String, tx: &mut Tx) {
    let Some(f) = s.sheets.form_mut() else { return };
    if !f.fields.get(i).is_some_and(answerable) {
        return;
    }
    if f.focus(i) {
        tx.blur();
    }
    if f.chosen[i] != key {
        f.chosen[i] = key;
        changed(s, i);
    }
}

/// The keys while a sheet is up: nothing behind it takes any. A form moves between its fields
/// with Tab and the arrows, turns a picker with Left, Right and Space, submits with Enter and is
/// dismissed with Esc; a question moves its pick and answers with Enter, and Esc leaves it
/// (`dismissal`: a yes or no answered no, a pick of one declined).
pub fn key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool {
    if ev.key == Key::Escape {
        dismiss(s);
        return true;
    }
    if let Some(f) = s.sheets.form_mut() {
        let at = f.at();
        let on_picker = f.fields.get(at).is_some_and(picker);
        let on_picker_now = match ev.key {
            Key::Tab if ev.mods.shift => f.step(-1),
            Key::Tab | Key::Down => f.step(1),
            Key::Up => f.step(-1),
            _ => false,
        };
        if on_picker_now {
            tx.blur();
        }
        match ev.key {
            Key::Enter => submit(s, false),
            Key::Left if on_picker => cycle(s, at, -1),
            Key::Right if on_picker => cycle(s, at, 1),
            Key::Text(ref t) if on_picker && t == " " => cycle(s, at, 1),
            _ => {}
        }
        return true;
    }
    if s.sheets.edit_sheet().is_some() {
        if ev.key == Key::Enter {
            finish_edit(s);
        }
        return true;
    }
    if let Some(q) = s.sheets.question_mut() {
        let n = if q.question.choices.is_empty() { 2 } else { q.question.choices.len() };
        match ev.key {
            Key::Up | Key::Left => q.picked = (q.picked + n - 1) % n,
            Key::Down | Key::Right | Key::Tab => q.picked = (q.picked + 1) % n,
            Key::Enter => {
                let key = picked_key(q);
                answer(s, key);
            }
            _ => {}
        }
    }
    true
}

/// A typed field's own keys: the ones that move between fields, submit or dismiss are the
/// sheet's; the rest are the text's.
fn field_key(s: &mut State, ev: &KeyEvent, tx: &mut Tx) -> bool {
    let sheet = matches!(ev.key, Key::Tab | Key::Up | Key::Down | Key::Enter | Key::Escape);
    sheet && key(s, ev, tx)
}

// ---------------------------------------------------------------------------------------------
// Drawing

/// A sheet's button: the default one filled, a destructive one in the error colour.
fn button(key: impl Into<String>, label: &str, default: bool, danger: bool, enabled: bool) -> El {
    let accent = if danger { theme::err() } else { theme::mark() };
    let (fill, ink) = match (default, enabled) {
        (true, true) => (accent.alpha(0.18), theme::text()),
        (_, false) => (Color::TRANSPARENT, theme::dim()),
        _ => (Color::TRANSPARENT, if danger { theme::err() } else { theme::text() }),
    };
    let mut b = row().key(key.into()).h(28.0).px(14.0).center_y().radius(theme::r_row()).fill(fill)
        .border(if default { theme::stroke_focus() } else { theme::stroke_quiet() }, if default && enabled { accent } else { theme::line_bright() })
        .motion(theme::SNAPPY)
        .child(text(label.to_string()).font(if default { theme::UI_BOLD } else { theme::UI }).color(ink));
    if enabled {
        b = b.hover(theme::text().alpha(0.06)).press(theme::text().alpha(0.12)).cursor(rune::Cursor::Pointer);
    }
    b
}

/// A key and what it does, as a sheet's hint line says it.
fn hint(key: &str, what: &str) -> El {
    row().gap(4.0).center_y().child(view::keycap(keys::label(key))).child(text(what.to_string()).font(theme::UI_SMALL).color(theme::dim()))
}

/// The panel every sheet is: over the window, which is inert behind it, near its top.
fn panel(name: &str, width: f32, body: El) -> El {
    // A picker or button with the focus hands its keys to the sheet; a typed field's are its own
    // first (`field_key`).
    let card = col().key(name.to_string()).w(width).padding(18.0).gap(12.0).radius(theme::r_card()).on_key(key)
        .fill(theme::raised()).border(theme::stroke_focus(), theme::mark()).shadow(16.0, 48.0, Color::BLACK.alpha(0.55))
        .child(body);
    el().w_full().h_full().absolute(0.0, 0.0).fill(theme::bg().alpha(0.6))
        .modal(false, dismiss)
        .child(row().w_full().pt(90.0).items_start().child(spacer()).child(card.on_press(|_: &mut State| {})).child(spacer()))
}

const LABEL_W: f32 = 150.0;

/// How tall a form's fields grow before they scroll inside its sheet.
const FIELDS_MAX_H: f32 = 420.0;

/// One field: its label, then its value — typed, chosen, or why it takes none — and what the
/// core refused about it.
fn field_view(f: &FormSheet, i: usize, x: &Field, on: bool) -> El {
    let open = answerable(x);
    let label = text(x.label.clone()).font(if on { theme::UI_BOLD } else { theme::UI })
        .color(if !open { theme::dim() } else if on { theme::text() } else { theme::grey() }).w(LABEL_W).shrink();
    let value = if !open {
        // A masked field's value is never drawn, whether or not it takes an answer.
        let why = if !x.why.is_empty() || x.masked { x.why.clone() } else { x.value.clone() };
        text(why).font(theme::UI).color(theme::dim()).wrap().grow(1.0).shrink()
    } else if picker(x) {
        // A control of its own, which the keys go to while it is the field they are on: first in
        // the tab order, so the modal gives it focus once a field's caret lets go (`focus`).
        let mut choices = row().key(format!("sheet-field-{}", x.key)).grow(1.0).shrink().gap(6.0).flex_wrap().padding(2.0).radius(theme::r_row())
            .focusable(true).tab_index(if on { 1 } else { 0 }).no_focus_ring()
            .border(theme::stroke_quiet(), if on { theme::mark() } else { Color::TRANSPARENT });
        for c in &x.choices {
            let chosen = f.chosen[i] == c.key;
            let key = c.key.clone();
            choices = choices.child(row().key(format!("sheet-choice-{}-{}", x.key, c.key)).h(24.0).px(10.0).center_y().radius(theme::r_key())
                .fill(if chosen { theme::mark().alpha(0.16) } else { Color::TRANSPARENT })
                .border(theme::stroke_quiet(), if chosen { theme::mark() } else { theme::line() })
                .hover(theme::text().alpha(0.06)).cursor(rune::Cursor::Pointer).motion(theme::SNAPPY)
                .child(text(c.label.clone()).font(if chosen { theme::UI_BOLD } else { theme::UI }).color(if chosen { theme::text() } else { theme::grey() }))
                .on_press_tx(move |s: &mut State, tx: &mut Tx| choose(s, i, key.clone(), tx)));
        }
        choices
    } else {
        let field = text_input(&f.edits[i], theme::UI.lh(20.0)).key(format!("sheet-field-{}", x.key)).single_line().grow(1.0).color(theme::text())
            .placeholder(x.placeholder.clone(), theme::dim())
            .on_edit(move |s: &mut State| s.sheets.edit(i))
            .on_input_change(move |s: &mut State, _| changed(s, i))
            .on_input_key(field_key);
        let mut boxed = row().grow(1.0).shrink().h(30.0).px(8.0).gap(6.0).center_y().radius(theme::r_row()).fill(theme::sunken())
            .border(theme::stroke_quiet(), if on { theme::mark() } else { theme::line() })
            .child(field);
        if !x.suffix.is_empty() {
            boxed = boxed.child(text(x.suffix.clone()).font(theme::UI_SMALL).color(theme::dim()));
        }
        boxed
    };
    let mut out = col().key(row_key(&x.key)).w_full().gap(4.0).child(row().w_full().gap(12.0).center_y().child(label).child(value));
    if let Some(why) = f.error(&x.key) {
        out = out.child(row().w_full().child(el().w(LABEL_W + 12.0)).child(text(why.to_string()).font(theme::UI_SMALL).color(theme::err()).wrap().shrink()));
    }
    out.on_press_tx(move |s: &mut State, tx: &mut Tx| {
        if let Some(f) = s.sheets.form_mut()
            && f.fields.get(i).is_some_and(answerable)
            && f.focus(i)
        {
            tx.blur();
        }
    })
}

fn form_view(f: &FormSheet) -> El {
    let at = f.at();
    let mut body = col().w_full().gap(10.0).child(text(f.title.clone()).font(theme::UI_BOLD).color(theme::text()));
    for line in f.about.lines() {
        body = body.child(text(line.to_string()).font(theme::UI).color(theme::grey()).wrap().w_full());
    }
    // The fields scroll inside the sheet past a height, so its note and buttons stay on screen
    // however many rows a form has (the run options have a dozen, and grow).
    let mut rows = col().key("sheet-fields").w_full().gap(10.0).max_h(FIELDS_MAX_H).scroll(ScrollSpec { stick_bottom: false });
    let mut section = "";
    for (i, x) in f.fields.iter().enumerate() {
        if !x.section.is_empty() && x.section != section {
            rows = rows.child(col().w_full().gap(4.0).pt(4.0)
                .child(text(x.section.clone()).font(theme::UI_SMALL).color(theme::gold()))
                .child(el().h(1.0).w_full().fill(theme::line())));
        }
        section = &x.section;
        rows = rows.child(field_view(f, i, x, i == at));
    }
    body = body.child(rows);
    // Why the field the keys are on is asked: a picker's chosen option says its own, when it has one.
    let why = f.fields.get(at).map(|x| {
        let chosen = x.choices.iter().find(|c| c.key == f.chosen[at]).map(|c| c.hint.as_str()).unwrap_or("");
        if chosen.is_empty() { x.hint.clone() } else { chosen.to_string() }
    }).unwrap_or_default();
    if !why.is_empty() {
        body = body.child(text(why).font(theme::UI_SMALL).color(theme::grey()).wrap().w_full());
    }
    if let Some(e) = f.error("") {
        body = body.child(text(e.to_string()).font(theme::UI_SMALL).color(theme::err()).wrap().w_full());
    }
    // What the answers add up to: the command a create would run, which a shell reproduces.
    if !f.note.is_empty() {
        body = body.child(col().key("sheet-note").w_full().gap(4.0).padding(10.0).radius(theme::r_row()).fill(theme::sunken())
            .child(text("would run").font(theme::UI_SMALL).color(theme::dim()))
            .child(text(f.note.clone()).font(theme::UI_SMALL).color(theme::grey()).wrap().w_full()));
    }
    let mut buttons = row().w_full().gap(8.0).center_y()
        .child(hint("tab", "next")).child(hint("escape", "cancel")).child(spacer());
    if f.sent {
        let waiting = if f.action.is_empty() { "saving…" } else { "sent…" };
        buttons = buttons.child(text(waiting).font(theme::UI_SMALL).color(theme::dim()));
    }
    let action = if f.action.is_empty() { "Save" } else { f.action.as_str() };
    buttons = buttons
        .child(button("sheet-cancel", "Cancel", false, false, true).on_press(dismiss))
        .child(button("sheet-submit", action, true, false, !f.sent).default_button().on_press(|s: &mut State| submit(s, false)));
    panel("sheet", 600.0, body.child(buttons))
}

fn question_view(q: &QuestionSheet) -> El {
    let x = &q.question;
    let mut body = col().w_full().gap(12.0).child(text(x.title.clone()).font(theme::UI_BOLD).color(theme::text()));
    // What it is about, on a line of its own: the directory a create would carry.
    if !x.subject.is_empty() {
        body = body.child(text(x.subject.clone()).font(theme::UI_BOLD).color(theme::mark()).wrap().w_full());
    }
    if !x.body.is_empty() {
        body = body.child(text(x.body.clone()).font(theme::UI).color(theme::grey()).wrap().w_full());
    }
    // What the answer turns on, as counted so far, in the core's words: the directory a copy
    // would carry.
    if !x.count.is_empty() {
        body = body.child(text(x.count.clone()).key("sheet-count").font(theme::UI_BOLD).color(theme::text()).wrap().w_full());
    }
    if x.choices.is_empty() {
        // A yes or no, worded as the core asks it; a destruction (`defaultNo`) is never the
        // default, and its yes is drawn as one.
        let danger = x.default_no;
        let buttons = row().w_full().gap(8.0).center_y()
            .child(hint("escape", "no")).child(spacer())
            .child(button("sheet-no", "No", q.picked == 1, false, true)
                .on_press(|s: &mut State| answer(s, "no".into())))
            .child(button("sheet-yes", "Yes", q.picked == 0, danger, true)
                .on_press(|s: &mut State| answer(s, "yes".into())));
        return panel("sheet", 480.0, body.child(buttons));
    }
    let mut list = col().w_full().gap(2.0);
    for (i, c) in x.choices.iter().enumerate() {
        let on = i == q.picked;
        let key = c.key.clone();
        let mut line = col().key(format!("sheet-choice-{}", c.key)).w_full().px(10.0).py(5.0).gap(2.0).radius(theme::r_row())
            .fill(if on { theme::selection() } else { Color::TRANSPARENT }).motion(theme::SNAPPY).cursor(rune::Cursor::Pointer)
            .child(text(c.label.clone()).font(theme::UI).color(theme::text()));
        if !c.hint.is_empty() {
            line = line.child(text(c.hint.clone()).font(theme::UI_SMALL).color(theme::grey()).wrap());
        }
        list = list.child(line
            .on_hover(move |s: &mut State| if let Some(q) = s.sheets.question_mut() { q.picked = i })
            .on_press(move |s: &mut State| answer(s, key.clone())));
    }
    let hints = row().w_full().gap(14.0).center_y()
        .child(hint("up", "")).child(hint("down", "move")).child(hint("enter", "choose")).child(hint("escape", "cancel"));
    panel("sheet", 520.0, body.child(list).child(hints))
}

fn edit_view(e: &EditSheet) -> El {
    let what = match &e.opened {
        Ok(()) => "It is open in your editor. Save it there, then press Done: what you saved is read back. Cancel keeps it as it was.".to_string(),
        Err(why) => format!("{why}. Open it yourself, save it, then press Done; or Cancel to keep it as it was."),
    };
    let body = col().w_full().gap(12.0)
        .child(text(e.edit.title.clone()).font(theme::UI_BOLD).color(theme::text()))
        .child(text(what).font(theme::UI).color(if e.opened.is_ok() { theme::grey() } else { theme::warn() }).wrap().w_full())
        .child(text(e.edit.path.clone()).font(theme::UI_SMALL).color(theme::dim()).wrap().w_full());
    let buttons = row().w_full().gap(8.0).center_y()
        .child(hint("enter", "done")).child(hint("escape", "cancel")).child(spacer())
        .child(button("sheet-reopen", "Open again", false, false, true).on_press(reopen))
        .child(button("sheet-cancel", "Cancel", false, false, true).on_press(dismiss))
        .child(button("sheet-done", "Done", true, false, true).default_button().on_press(finish_edit));
    panel("sheet", 560.0, body.child(buttons))
}

/// The sheet on top, if any.
pub fn view(s: &State) -> Option<El> {
    match s.sheets.queue.first()? {
        Sheet::Form(f) => Some(form_view(f)),
        Sheet::Question(q) => Some(question_view(q)),
        Sheet::Edit(e) => Some(edit_view(e)),
    }
}
