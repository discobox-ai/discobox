//! Where boxes and their terminal streams come from.
//!
//! `discobox desktop` runs this app with `--bridge` and serves it the console core on its stdin
//! and stdout (`cli/internal/console/bridge`, ADR 26-11-07-289): the app subscribes to what it
//! draws, sends every registry command as a `command` for the core to rule on and run, and gets
//! a `result` back — `enter` for a flow that is the app's own (focus, a menu, a workspace).
//! Everything arrives asynchronously into the `Inbox`, with the stream output, and the window's
//! poll drains it. Nothing here blocks the window but the opening `hello`.
//!
//! Without `--bridge` the `Mock` answers the same messages, with made-up boxes whose panes are
//! local shells on PTYs, for developing the app and for headless snapshots.
//!
//! The bridge's client and the wire's types are the `discobox_bridge` crate (`bridge/`); the
//! types are generated from the bridge's Go types, through its schema (`go tool task generate`),
//! and none is written here.

use std::collections::HashMap;
use std::io::{Read, Write};
use std::sync::{Arc, Mutex};

use discobox_bridge::{Client, Incoming};
use portable_pty::{CommandBuilder, MasterPty, PtySize, native_pty_system};
use rune::Waker;

use crate::wire::{
    Answer, Box as Sandbox, ChoiceGroup, Close, Command, Commands, Choice, Entry, Field, FieldError, FieldErrors, Filter, FilterChoice,
    Form, GitTone, Header, Link, Listing, Message, Open, Palette, PaletteCommit, PaletteQuery, PaletteRow, PortGroup, Progress, Question,
    Remark, Result as CommandResult, Row, Sessions, Spec, SpecTarget, State, Status, Stream, StreamState, Submit, Subscribe, Tab, TabEnd,
    TabKind, Topic, Type, Unsubscribe,
};

/// What the app sends, by message: the type and its body.
pub enum Out {
    /// A topic followed: the listing's narrowed to its filter (none: what the last window left),
    /// a box's topic about its box, the commands and the palette asked from its context.
    Subscribe(Subscribe),
    Unsubscribe(u32),
    Command(Command),
    Open { stream: u32, sandbox: String, exec: String, cols: u16, rows: u16 },
    /// A stream let go: the app shows it no more, and the session goes on in the box.
    Close(u32),
    /// A form filled, drafted or dismissed.
    Submit(Submit),
    /// A question answered, or declined.
    Answer(Answer),
}

impl Out {
    pub fn message(self) -> Message {
        match self {
            Out::Subscribe(s) => Message { r#type: Type::Subscribe, subscribe: Some(s), ..Default::default() },
            Out::Unsubscribe(sub) => Message { r#type: Type::Unsubscribe, unsubscribe: Some(Unsubscribe { sub }), ..Default::default() },
            Out::Command(c) => Message { r#type: Type::Command, command: Some(c), ..Default::default() },
            Out::Open { stream, sandbox, exec, cols, rows } => Message {
                r#type: Type::Open,
                open: Some(Open { stream, r#box: sandbox, exec, cols: cols.into(), rows: rows.into() }),
                ..Default::default()
            },
            Out::Close(stream) => Message { r#type: Type::Close, close: Some(Close { stream }), ..Default::default() },
            Out::Answer(a) => Message { r#type: Type::Answer, answer: Some(a), ..Default::default() },
            Out::Submit(f) => Message { r#type: Type::Submit, submit: Some(f), ..Default::default() },
        }
    }
}

/// The spec the CLI printed (`discobox desktop --print-spec`), for running without one.
pub fn bundled_spec() -> Spec { serde_json::from_str(include_str!("spec.json")).expect("spec.json") }

/// What arrives for the window.
pub enum Event {
    /// A listing subscription's snapshot, or why it could not be read.
    Listing(u32, Result<Listing, String>),
    /// A sessions subscription's snapshot, or why it could not be read.
    Sessions(u32, Result<Sessions, String>),
    /// The commands as they stand where a menu was opened, or why they could not be read.
    Commands(u32, Result<Commands, String>),
    /// What the palette's query found, or why it could not be read.
    Palette(u32, Result<Palette, String>),
    /// A header subscription's snapshot, or why it could not be read.
    Header(u32, Result<Header, String>),
    Result(CommandResult),
    Stream(Stream),
    Question(Question),
    Form(Form),
    /// Why what was submitted to a form was refused; the form stays up.
    FieldErrors(FieldErrors),
    Progress(Progress),
    Data(u32, Vec<u8>),
    Exited(u32, String),
}

#[derive(Clone, Default)]
pub struct Inbox(pub Arc<Mutex<Vec<Event>>>, pub Arc<Mutex<Option<Waker>>>);

impl Inbox {
    pub fn push(&self, e: Event) {
        self.0.lock().unwrap().push(e);
        if let Some(w) = self.1.lock().unwrap().as_ref() {
            w();
        }
    }
    pub fn drain(&self) -> Vec<Event> { std::mem::take(&mut *self.0.lock().unwrap()) }
    pub fn set_waker(&self, w: Waker) { *self.1.lock().unwrap() = Some(w); }
}

pub trait Backend {
    /// Send one message; what answers it arrives in the inbox.
    fn send(&mut self, m: Out);
    fn input(&mut self, stream: u32, bytes: &[u8]);
    fn resize(&mut self, stream: u32, cols: u16, rows: u16);
}

// ---------------------------------------------------------------------------------------------
// The bridge: the client (`discobox_bridge`) on stdin and stdout.

/// A message as the window's event, or nothing for one the app does not read.
fn event(m: Message) -> Option<Event> {
    match m.r#type {
        Type::Snapshot => {
            let s = m.snapshot?;
            match s.topic {
                Topic::Listing => Some(Event::Listing(s.sub, match s.listing {
                    Some(l) if s.error.is_empty() => Ok(l),
                    _ => Err(s.error),
                })),
                Topic::Sessions => Some(Event::Sessions(s.sub, match s.sessions {
                    Some(v) if s.error.is_empty() => Ok(v),
                    _ => Err(s.error),
                })),
                Topic::Header => Some(Event::Header(s.sub, match s.header {
                    Some(h) if s.error.is_empty() => Ok(h),
                    _ => Err(s.error),
                })),
                Topic::Commands => Some(Event::Commands(s.sub, match s.commands {
                    Some(v) if s.error.is_empty() => Ok(v),
                    _ => Err(s.error),
                })),
                Topic::Palette => Some(Event::Palette(s.sub, match s.palette {
                    Some(v) if s.error.is_empty() => Ok(v),
                    _ => Err(s.error),
                })),
                _ => None,
            }
        }
        Type::Result => m.result.map(Event::Result),
        Type::Stream => m.stream.map(Event::Stream),
        Type::Question => m.question.map(Event::Question),
        Type::Form => m.form.map(Event::Form),
        Type::FieldErrors => m.field_errors.map(Event::FieldErrors),
        Type::Progress => m.progress.map(Event::Progress),
        _ => None,
    }
}

pub struct Bridge {
    client: Client,
}

impl Bridge {
    /// Says hello and waits for the welcome; then what arrives goes to the inbox, until the CLI
    /// is gone, when the app is too.
    pub fn connect(inbox: Inbox) -> Result<(Bridge, Spec), String> {
        let (client, welcome) = Client::connect(std::io::stdin(), std::io::stdout(), move |incoming| match incoming {
            Incoming::Message(m) => {
                if let Some(e) = event(m) {
                    inbox.push(e);
                }
            }
            Incoming::Output(stream, bytes) => inbox.push(Event::Data(stream, bytes)),
            Incoming::Exited(stream, why) => inbox.push(Event::Exited(stream, why)),
            // Nothing more will arrive, and nothing can be sent.
            Incoming::Closed => std::process::exit(0),
        })?;
        Ok((Bridge { client }, welcome.spec.ok_or("the CLI's welcome carried no spec")?))
    }
}

impl Backend for Bridge {
    // A write fails only once the CLI is gone, and then the reader ends the app.
    fn send(&mut self, m: Out) {
        if let Err(e) = self.client.send(&m.message()) {
            eprintln!("desktop: a message could not be written: {e}");
        }
    }
    fn input(&mut self, stream: u32, bytes: &[u8]) { let _ = self.client.input(stream, bytes); }
    fn resize(&mut self, stream: u32, cols: u16, rows: u16) { let _ = self.client.resize(stream, cols, rows); }
}

// ---------------------------------------------------------------------------------------------
// The mock: the same messages, answered here with the core's rules roughly. The CLI answers
// them from the core itself.

pub struct Mock {
    inbox: Inbox,
    boxes: Vec<Sandbox>,
    /// Which box made which, by key: the core's to nest by, which the wire does not carry.
    parents: HashMap<String, String>,
    /// The listing subscription, and what it is narrowed to.
    list_sub: u32,
    filter: Filter,
    extra: HashMap<String, Vec<Tab>>,
    ptys: HashMap<u32, Pty>,
    made: u64,
    /// The forms and questions put to the app, by their ID: what each is for, and the command
    /// that waits on it.
    asks: HashMap<u64, (Command, Ask)>,
    next_ask: u64,
}

/// What a mock form or question is for.
#[derive(Clone)]
enum Ask {
    Rename(String),
    Settings(String),
    Purge(Vec<String>),
}

fn mock_exec(exec: &str, kind: TabKind, title: &str) -> Tab {
    Tab { exec: exec.into(), kind, title: title.into(), live: true, argv: vec![] }
}

/// A made-up box, as the CLI would list it: one server, so its key is its ID.
#[allow(clippy::too_many_arguments)]
fn mock_box(id: &str, name: &str, state: State, agent: &str, harness: &str, source: &str) -> Sandbox {
    Sandbox {
        key: id.into(), id: id.into(), name: name.into(), state, agent: agent.into(),
        harness: harness.into(), source: source.into(), attachable: state == State::Running,
        created: "2026-11-09T12:00:00Z".into(), ..Default::default()
    }
}

/// A mock box's folder: its source, or this machine for none.
fn mock_folder(b: &Sandbox) -> String { if b.source.is_empty() { "this machine".into() } else { b.source.clone() } }

/// What a mock box's row says in place of its name, as the core's `Sandbox.Remark` would.
fn mock_remark(b: &Sandbox) -> Option<Remark> {
    let text = [&b.message, &b.agent_msg, &b.provisioning].into_iter().find(|t| !t.is_empty())?;
    Some(Remark { text: text.clone(), ..Default::default() })
}

impl Mock {
    pub fn new(inbox: Inbox) -> Mock {
        let boxes = vec![
            Sandbox {
                tags: vec!["wip".into(), "tui".into()], branch: "rune-console".into(), commit: "00541f9".into(),
                dirty: true, added: 412, deleted: 37, cpu: 12, memory_bytes: 2_254_857_830,
                ..mock_box("sbx_k2m9q4tz7wbf1xay", "rune console port", State::Running, "working", "claude", "discobox")
            },
            Sandbox {
                tags: vec!["ENG-12".into()], branch: "fix/reaper-race".into(), commit: "8be21c0".into(),
                added: 58, deleted: 12, cpu: 3, memory_bytes: 1_503_238_553,
                ..mock_box("sbx_p8c3n0vd6rgs2hkq", "reaper race", State::Running, "blocked", "codex", "discobox")
            },
            Sandbox {
                branch: "docs/attach".into(), commit: "41d9e2a".into(),
                ..mock_box("sbx_r5t1y7u3i9o0pzx2", "attach docs", State::Running, "done", "claude", "discobox")
            },
            Sandbox {
                provisioning: "provisioning browser, build, bun…".into(), cpu: 2,
                ..mock_box("sbx_v6svx9a5hndxswfp", "lucid_lalande", State::Starting, "", "shell", "")
            },
            mock_box("sbx_a1b2c3d4e5f6g7h8", "quiet_hopper", State::Stopped, "idle", "opencode", ""),
            Sandbox { tags: vec!["wip".into()], ..mock_box("sbx_z0x9c8v7b6n5m4l3", "old experiment", State::Archived, "", "claude", "discobox") },
        ];
        let parents = HashMap::from([("sbx_r5t1y7u3i9o0pzx2".to_string(), "sbx_p8c3n0vd6rgs2hkq".to_string())]);
        let filter = Filter { tags: vec![], ..Default::default() };
        Mock { inbox, boxes, parents, list_sub: 0, filter, extra: HashMap::new(), ptys: HashMap::new(), made: 0, asks: HashMap::new(), next_ask: 1 }
    }

    /// The rows the core would draw for a filter, roughly: archived boxes only when asked for,
    /// the tags a box must carry, folders by source as sections, and a box under the one that
    /// made it.
    fn rows(&self, f: &Filter) -> Vec<Row> {
        let tagged = |b: &Sandbox| f.tags.iter().all(|t| b.tags.contains(t));
        let shown = |b: &Sandbox| (f.archived || b.state != State::Archived) && tagged(b);
        let in_folder = |b: &Sandbox| f.folder_key.is_empty() || mock_folder(b) == f.folder_key;
        let parent_shown = |b: &Sandbox| {
            self.parents.get(&b.key).and_then(|p| self.sandbox(p)).is_some_and(|p| shown(p) && in_folder(p))
        };
        let mut rows = vec![];
        for sec in self.folders() {
            for b in self.boxes.iter().filter(|b| mock_folder(b) == sec && shown(b) && in_folder(b) && !parent_shown(b)) {
                rows.push(Row { r#box: b.key.clone(), section: sec.clone(), section_title: sec.clone(), remark: mock_remark(b), ..Default::default() });
                let kids: Vec<&Sandbox> = self.boxes.iter().filter(|c| self.parents.get(&c.key) == Some(&b.key) && shown(c)).collect();
                for (i, c) in kids.iter().enumerate() {
                    rows.push(Row {
                        r#box: c.key.clone(), section: sec.clone(), section_title: sec.clone(), depth: 1, last: i + 1 == kids.len(),
                        remark: mock_remark(c), ..Default::default()
                    });
                }
            }
        }
        rows
    }

    /// Every folder a box is in, in the order the boxes are.
    fn folders(&self) -> Vec<String> {
        let mut out: Vec<String> = vec![];
        for b in &self.boxes {
            if !out.contains(&mock_folder(b)) {
                out.push(mock_folder(b));
            }
        }
        out
    }

    /// The listing as the core would give it, roughly: the rows, and the filter's choices each
    /// counted against the rest of it.
    fn listing(&self) -> Listing {
        let f = &self.filter;
        let count = |g: &Filter| {
            let n = self.rows(g).len();
            format!("{n} box{}", if n == 1 { "" } else { "es" })
        };
        let mut choices = vec![];
        for key in self.folders().into_iter().map(Some).chain([None]) {
            let k = key.clone().unwrap_or_default();
            let mark = Filter { folder_key: k.clone(), folder_label: k.clone(), ..f.clone() };
            choices.push(FilterChoice {
                group: ChoiceGroup::Folder, label: key.unwrap_or_else(|| "all folders".into()), detail: count(&mark),
                marked: f.folder_key == k, unmark: mark.clone(), mark, ..Default::default()
            });
        }
        let mut tags: Vec<String> = self.boxes.iter().filter(|b| f.archived || b.state != State::Archived).flat_map(|b| b.tags.clone()).collect();
        tags.sort();
        tags.dedup();
        if !tags.is_empty() {
            let none = Filter { tags: vec![], ..f.clone() };
            choices.push(FilterChoice {
                group: ChoiceGroup::Tag, label: "all tags".into(), detail: count(&none), marked: f.tags.is_empty(), several: true,
                mark: none.clone(), unmark: none,
            });
            for t in tags {
                let on = f.tags.contains(&t);
                let mut mark = f.clone();
                if !on {
                    mark.tags.push(t.clone());
                    mark.tags.sort();
                }
                let unmark = if on { Filter { tags: f.tags.iter().filter(|x| **x != t).cloned().collect(), ..f.clone() } } else { mark.clone() };
                choices.push(FilterChoice { group: ChoiceGroup::Tag, label: format!("#{t}"), detail: count(&mark), marked: on, several: true, mark, unmark });
            }
        }
        let mut parts = vec![];
        if !f.folder_key.is_empty() {
            parts.push(f.folder_label.clone());
        }
        parts.extend(f.tags.iter().map(|t| format!("#{t}")));
        let rows = self.rows(f);
        let archived = if f.archived { 0 } else { self.rows(&Filter { archived: true, ..f.clone() }).len() - rows.len() };
        Listing {
            boxes: self.boxes.clone(), grouped: f.folder_key.is_empty(), rows, notes: vec![], filter: f.clone(),
            narrowed: !f.folder_key.is_empty() || !f.tags.is_empty(),
            label: if parts.is_empty() { "all discoboxes".into() } else { parts.join(" · ") },
            choices, archived: archived as i64, unreachable: vec![], waiting: vec![],
        }
    }

    fn relist(&self) { self.inbox.push(Event::Listing(self.list_sub, Ok(self.listing()))); }

    /// A box's header as the core would say it, roughly: its git, its diff, its tags, and for
    /// the first box a dev server forwarded to this machine beside a database that is not a page.
    fn header(&self, key: &str) -> Header {
        let b = self.sandbox(key).cloned().unwrap_or_default();
        let base = if b.branch.is_empty() { String::new() } else { format!("{}@{}{}", b.branch, b.commit, if b.dirty { "*" } else { "" }) };
        let ports = if self.boxes.first().is_some_and(|f| f.key == key) {
            vec![
                PortGroup { protocol: "http".into(), ports: vec![Link { label: "5173".into(), url: "http://localhost:5173".into() }] },
                PortGroup { protocol: "tcp".into(), ports: vec![Link { label: "5432".into(), url: String::new() }] },
            ]
        } else {
            vec![]
        };
        Header {
            id: b.id.clone(), work: vec![], blocked: if b.agent == "blocked" { "blocked".into() } else { String::new() }, changes: if b.agent == "blocked" || base.is_empty() { String::new() } else if b.dirty { "dirty".into() } else { "clean".into() },
            base, tone: if b.dirty { GitTone::Dirty } else { GitTone::Quiet }, diff: b.added + b.deleted > 0, added: b.added, deleted: b.deleted,
            ports, tags: b.tags.iter().map(|t| format!("#{t}")).collect(), ..Default::default()
        }
    }

    fn sandbox(&self, key: &str) -> Option<&Sandbox> { self.boxes.iter().find(|b| b.key == key) }

    fn spawn(&mut self, sandbox: &str, exec: &Tab, stream: u32, cols: u16, rows: u16) -> Result<(), String> {
        let sbx = self.sandbox(sandbox).cloned().unwrap_or_default();
        let pty = Pty::spawn(&sbx, exec, stream, cols, rows, self.inbox.clone())?;
        self.ptys.insert(stream, pty);
        Ok(())
    }

    fn command(&mut self, c: Command) -> CommandResult {
        let target = c.targets.first().cloned().unwrap_or_default();
        let workspace = c.context.workspace.clone();
        let on_pane = c.context.pane_focused && c.context.workspace_shown;
        let fail = |why: &str| CommandResult { status: Status::Failed, why: why.into(), ..Default::default() };
        let enter = |sandbox: &str| CommandResult { status: Status::Enter, r#box: sandbox.into(), ..Default::default() };
        let running = self.sandbox(&target).is_some_and(Sandbox::running);
        match c.command.as_str() {
            "attach" if running => enter(&target),
            "attach" | "shell" if !running => fail("the box has no container; repair rebuilds it"),
            "terminal" | "end" | "maximize" | "detach" if !on_pane => fail("requires a workspace pane"),
            "shell" | "terminal" => {
                let sandbox = if c.command == "shell" { target } else { workspace };
                self.made += 1;
                let exec = if c.command == "shell" {
                    mock_exec(&format!("exec_{}", self.made), TabKind::Shell, "bash")
                } else {
                    let harness = self.sandbox(&sandbox).map(|b| b.harness.clone()).unwrap_or_default();
                    mock_exec(&format!("exec_{}", self.made), TabKind::Terminal, &harness)
                };
                let size = |n: i64| u16::try_from(n.max(1)).unwrap_or(u16::MAX);
                if let Err(e) = self.spawn(&sandbox, &exec, c.args.stream, size(c.args.cols), size(c.args.rows)) {
                    return fail(&e);
                }
                self.extra.entry(sandbox.clone()).or_default().push(exec.clone());
                CommandResult { status: Status::Done, r#box: sandbox, session: Some(exec), ..Default::default() }
            }
            "end" if c.args.exec == "primary" => fail("the primary session ends with its box; stop the box instead"),
            "end" => {
                if let Some(mut p) = self.ptys.remove(&c.args.stream) {
                    let _ = p.child.kill();
                }
                if let Some(v) = self.extra.get_mut(&workspace) {
                    v.retain(|e| e.exec != c.args.exec);
                }
                CommandResult { status: Status::Done, ..Default::default() }
            }
            "stop" | "start" | "upgrade" | "repair" | "archive" | "unarchive" => {
                // Every target, as the core sends a batch; one that cannot take it is refused alone.
                let mut failures = vec![];
                for t in &c.targets {
                    let actions = self.actions(t);
                    if let Some(a) = actions.iter().find(|a| a.command == c.command && !a.enabled) {
                        failures.push(format!("cannot {}: {}", a.label, a.why));
                        continue;
                    }
                    if let Some(b) = self.boxes.iter_mut().find(|b| b.key == *t) {
                        b.state = match c.command.as_str() {
                            "stop" => State::Stopped,
                            "start" => State::Running,
                            "archive" => State::Archived,
                            "unarchive" => State::Stopped,
                            _ => b.state,
                        };
                        b.attachable = b.state == State::Running;
                    }
                }
                self.relist();
                if failures.len() == c.targets.len() && !failures.is_empty() {
                    return fail(&failures.join("; "));
                }
                CommandResult { status: Status::Done, why: failures.join("; "), ..Default::default() }
            }
            "purge" => fail("cannot purge: archive it first — purge only takes archived boxes"),
            "refresh" => {
                self.relist();
                CommandResult { status: Status::Done, ..Default::default() }
            }
            "actions" => CommandResult { status: Status::Enter, r#box: target.clone(), entries: self.actions(&target), ..Default::default() },
            _ => enter(""),
        }
    }

    /// Puts a command's form or question to the app, as the core does for rename, settings and
    /// purge; the command waits on the answer (`answered`). False for any other command.
    fn ask(&mut self, c: &Command) -> bool {
        let id = self.next_ask;
        let Some((ask, event)) = self.put(id, c) else { return false };
        self.next_ask += 1;
        self.asks.insert(id, (c.clone(), ask));
        self.inbox.push(event);
        true
    }

    /// A command's form or question under an ID, and what it is for.
    fn put(&self, id: u64, c: &Command) -> Option<(Ask, Event)> {
        let target = c.targets.first().cloned().unwrap_or_default();
        let field = |key: &str, label: &str, value: &str| Field { key: key.into(), label: label.into(), value: value.into(), applies: true, ..Default::default() };
        let choice = |key: &str, label: &str, hint: &str| Choice { key: key.into(), label: label.into(), hint: hint.into() };
        let (ask, event) = match c.command.as_str() {
            "rename" => {
                let b = self.sandbox(&target)?;
                let name = Field { hint: "What should this discobox be called?".into(), placeholder: "name".into(), ..field("name", "name", &b.name) };
                (Ask::Rename(target), Event::Form(Form { id, command: c.id, title: "Rename".into(), fields: vec![name] }))
            }
            "settings" => {
                let b = self.sandbox(&target)?;
                let stopped = b.state == State::Stopped;
                let harness = Field {
                    choices: vec![choice(&b.harness, &b.harness, "what the box has now"), choice("codex", "codex", "Codex")],
                    applies: stopped, why: "stop the box to change its harness".into(),
                    hint: "the new harness provisions on the next start".into(), ..field("harness", "harness", &b.harness)
                };
                let restrict = Field {
                    section: "Egress".into(),
                    choices: vec![choice("no", "no", "this level restricts nothing"), choice("yes", "deny by default", "reach only the allow entries")],
                    ..field("deny", "restrict", "no")
                };
                let allow = Field { section: "Egress".into(), placeholder: "hosts, *.wildcards, IPs or CIDRs, comma-separated".into(), ..field("allow", "allow", "") };
                let api = Field {
                    section: "Access".into(),
                    choices: vec![choice("no", "no", "the box cannot call the discobox API"), choice("yes", "yes", "the box may call the discobox API")],
                    ..field("api", "discobox API", "no")
                };
                let name = Field { required: true, hint: "the handle it was created with".into(), ..field("name", "name", &b.name) };
                let title = format!("Settings · {}", b.name);
                (Ask::Settings(target), Event::Form(Form { id, command: c.id, title, fields: vec![name, harness, restrict, allow, api] }))
            }
            // The core refuses a purge none of its boxes can take before it asks anything.
            "purge" if !c.targets.iter().any(|t| self.sandbox(t).is_some_and(|b| b.state == State::Archived)) => return None,
            "purge" => {
                let name = match c.targets.as_slice() {
                    [one] => self.sandbox(one).map(|b| b.name.clone()).unwrap_or_default(),
                    many => format!("{} boxes selected", many.len()),
                };
                let body = format!("Purge {name}? The disk and everything on it goes, and unarchive cannot bring it back.");
                (Ask::Purge(c.targets.clone()), Event::Question(Question { id, command: c.id, title: "Purge".into(), body, choices: vec![], default_no: true }))
            }
            _ => return None,
        };
        Some((ask, event))
    }

    /// What a form's submit or a question's answer does: the command it ends, as the core would
    /// carry it out, or the form again — after a draft, or refused.
    fn answered(&mut self, id: u64, answers: Option<&HashMap<String, String>>, draft: bool) {
        let Some((c, ask)) = self.asks.get(&id).cloned() else { return };
        let command = c.id;
        let done = |note: String| CommandResult { id: command, status: Status::Done, note, ..Default::default() };
        let canceled = CommandResult { id: command, status: Status::Canceled, ..Default::default() };
        let Some(answers) = answers else {
            self.asks.remove(&id);
            self.inbox.push(Event::Result(canceled));
            return;
        };
        // What the app sent over what each field opened holding, as the bridge merges them.
        let opening: HashMap<String, String> = match self.put(id, &c) {
            Some((_, Event::Form(f))) => f.fields.into_iter().map(|x| (x.key, x.value)).collect(),
            _ => HashMap::new(),
        };
        let get = |k: &str| answers.get(k).or(opening.get(k)).map(|v| v.trim().to_string()).unwrap_or_default();
        let result = match ask {
            Ask::Rename(_) | Ask::Settings(_) if draft => {
                // The form again under its ID, as the core sends it after a draft.
                if let Some((_, form)) = self.put(id, &c) {
                    self.inbox.push(form);
                }
                return;
            }
            Ask::Settings(_) if get("name").is_empty() => {
                let errors = vec![FieldError { field: "name".into(), why: "a discobox needs a configured name".into() }];
                self.inbox.push(Event::FieldErrors(FieldErrors { form: id, errors }));
                return;
            }
            Ask::Rename(key) | Ask::Settings(key) => {
                let name = get("name");
                let renamed = self.boxes.iter_mut().find(|b| b.key == key).filter(|b| !name.is_empty() && b.name != name).map(|b| b.name = name.clone()).is_some();
                self.relist();
                match (c.command.as_str(), renamed) {
                    ("rename", true) => done(format!("renamed to {name}")),
                    ("rename", false) => done("name unchanged".into()),
                    _ => done("settings saved".into()),
                }
            }
            Ask::Purge(_) if get("key") != "yes" => canceled,
            Ask::Purge(keys) => {
                self.boxes.retain(|b| !keys.contains(&b.key) || b.state != State::Archived);
                self.relist();
                done(format!("purge: {}", keys.len()))
            }
        };
        self.asks.remove(&id);
        self.inbox.push(Event::Result(result));
    }

    /// The registry's lifecycle commands, with the core's rules roughly.
    fn actions(&self, sandbox: &str) -> Vec<Entry> {
        let running = self.sandbox(sandbox).is_some_and(Sandbox::running);
        let archived = self.sandbox(sandbox).is_some_and(|b| b.state == State::Archived);
        bundled_spec().commands.iter().filter(|c| c.lifecycle).map(|c| {
            let (enabled, why) = match c.id.as_str() {
                "attach" | "shell" => (running, "the box has no container; repair rebuilds it"),
                "review config" | "stop" => (running, "not running"),
                "start" => (!running && !archived, "already on, or archived"),
                "archive" => (!archived, "already archived"),
                "upgrade" => (false, "already on the current image"),
                "unarchive" => (archived, "not archived"),
                "purge" => (archived, "archive it first — purge only takes archived boxes"),
                _ => (true, ""),
            };
            Entry { command: c.id.clone(), label: c.label.clone(), detail: c.detail.clone(), enabled, why: if enabled { String::new() } else { why.into() }, uses: 0 }
        }).collect()
    }

    /// Every registry command for a box (or none), roughly: a lifecycle command by `actions`,
    /// any other able to act.
    fn entries(&self, sandbox: Option<&str>) -> Vec<Entry> {
        let lifecycle = sandbox.map(|b| self.actions(b)).unwrap_or_default();
        bundled_spec().commands.iter().map(|c| {
            lifecycle.iter().find(|e| e.command == c.id).cloned().unwrap_or_else(|| Entry {
                command: c.id.clone(), label: c.label.clone(), detail: c.detail.clone(), enabled: true, why: String::new(), uses: 0,
            })
        }).collect()
    }

    /// What the palette finds, with the core's rules roughly: a box command's alias and a space
    /// commit it; boxes and commands by subsequence; a chip's rows evaluated for it.
    fn palette(&self, mut q: PaletteQuery) -> Palette {
        let spec = bundled_spec();
        let mut commit = None;
        if q.command.is_empty() && q.r#box.is_empty()
            && let Some((first, rest)) = q.text.split_once(' ')
            && let Some(c) = spec.commands.iter().find(|c| c.target == SpecTarget::Box && c.aliases.iter().any(|a| !a.contains(' ') && a.eq_ignore_ascii_case(first)))
        {
            commit = Some(PaletteCommit { command: c.id.clone(), text: rest.into() });
            q.command = c.id.clone();
            q.text = rest.into();
        }
        let archived = q.archived || q.command == "purge" || q.command == "unarchive";
        let query = q.text.to_lowercase();
        let fits = |t: &str| {
            let t = t.to_lowercase();
            let mut it = t.chars();
            query.chars().filter(|c| *c != ' ').all(|q| it.any(|c| c == q))
        };
        let mut rows = vec![];
        if q.r#box.is_empty() {
            for b in self.boxes.iter().filter(|b| (archived || b.state != State::Archived) && (fits(&b.name) || fits(&b.id))) {
                let why = if q.command.is_empty() {
                    String::new()
                } else {
                    self.entries(Some(&b.key)).into_iter().find(|e| e.command == q.command && !e.enabled).map(|e| e.why).unwrap_or_default()
                };
                rows.push(PaletteRow { r#box: b.key.clone(), command: None, why });
            }
        }
        if q.command.is_empty() {
            let target = (!q.r#box.is_empty()).then_some(q.r#box.as_str());
            for (c, e) in spec.commands.iter().zip(self.entries(target)) {
                if (target.is_some() && c.target != SpecTarget::Box) || !c.aliases.iter().any(|a| fits(a)) {
                    continue;
                }
                let why = if e.enabled { String::new() } else { e.why.clone() };
                rows.push(PaletteRow { r#box: String::new(), command: Some(e), why });
            }
        }
        if rows.iter().any(|r| r.why.is_empty()) {
            rows.retain(|r| r.why.is_empty());
        }
        Palette { rows, archived, commit, partial: vec![], stale: vec![] }
    }
}

impl Backend for Mock {
    fn send(&mut self, m: Out) {
        match m {
            Out::Subscribe(Subscribe { sub, topic: Topic::Listing, filter, reveal, .. }) => {
                self.list_sub = sub;
                // A box revealed shows whatever the filter hid, as the core's `Revealing` does.
                if let Some(b) = self.sandbox(&reveal) {
                    self.filter = Filter { tags: vec![], archived: b.state == State::Archived, ..Default::default() };
                } else if let Some(f) = filter {
                    self.filter = f;
                }
                self.relist();
            }
            Out::Subscribe(Subscribe { sub, topic: Topic::Sessions, r#box: sandbox, .. }) => {
                let running = self.sandbox(&sandbox).is_some_and(Sandbox::running);
                let mut sessions = vec![mock_exec("primary", TabKind::Primary, "")];
                sessions.extend(self.extra.get(&sandbox).into_iter().flatten().cloned());
                self.inbox.push(Event::Sessions(sub, Ok(Sessions { running, sessions, services: vec![] })));
            }
            Out::Subscribe(Subscribe { sub, topic: Topic::Commands, targets, .. }) => {
                let entries = self.entries(targets.first().map(String::as_str));
                self.inbox.push(Event::Commands(sub, Ok(Commands { entries })));
            }
            Out::Subscribe(Subscribe { sub, topic: Topic::Palette, palette, .. }) => {
                let p = self.palette(palette.unwrap_or_default());
                self.inbox.push(Event::Palette(sub, Ok(p)));
            }
            Out::Subscribe(Subscribe { sub, topic: Topic::Header, r#box, .. }) => {
                let h = self.header(&r#box);
                self.inbox.push(Event::Header(sub, Ok(h)));
            }
            Out::Subscribe(_) | Out::Unsubscribe(_) => {}
            Out::Command(c) if self.ask(&c) => {}
            // A stream let go is a session left running in the box: the mock's is a local shell,
            // which nothing would show again, so it goes.
            Out::Close(stream) => {
                if let Some(mut p) = self.ptys.remove(&stream) {
                    let _ = p.child.kill();
                }
            }
            Out::Command(c) => {
                let mut r = self.command(c.clone());
                r.id = c.id;
                self.inbox.push(Event::Result(r));
            }
            Out::Open { stream, sandbox, exec, cols, rows } => {
                let kind = if exec == "primary" { TabKind::Primary } else { TabKind::Shell };
                let state = match self.spawn(&sandbox, &mock_exec(&exec, kind, ""), stream, cols, rows) {
                    Ok(()) => Stream { stream, state: StreamState::Attached, ..Default::default() },
                    Err(why) => Stream { stream, state: StreamState::Failed, why, end: Some(if kind == TabKind::Primary { TabEnd::Workspace } else { TabEnd::Tab }), ..Default::default() },
                };
                self.inbox.push(Event::Stream(state));
            }
            Out::Submit(f) => {
                let answers = (!f.cancel).then_some(&f.answers);
                self.answered(f.form, answers, f.draft);
            }
            Out::Answer(a) => {
                let answers = HashMap::from([("key".to_string(), a.key)]);
                self.answered(a.question, (!a.declined).then_some(&answers), false);
            }
        }
    }

    fn input(&mut self, stream: u32, bytes: &[u8]) {
        if let Some(p) = self.ptys.get_mut(&stream) {
            let _ = p.writer.write_all(bytes);
            let _ = p.writer.flush();
        }
    }

    fn resize(&mut self, stream: u32, cols: u16, rows: u16) {
        if let Some(p) = self.ptys.get(&stream) {
            let _ = p.master.resize(PtySize { rows, cols, pixel_width: 0, pixel_height: 0 });
        }
    }
}

struct Pty {
    master: Box<dyn MasterPty + Send>,
    writer: Box<dyn Write + Send>,
    child: Box<dyn portable_pty::Child + Send + Sync>,
}

impl Pty {
    fn spawn(sandbox: &Sandbox, exec: &Tab, stream: u32, cols: u16, rows: u16, inbox: Inbox) -> Result<Pty, String> {
        let pair = native_pty_system()
            .openpty(PtySize { rows, cols, pixel_width: 0, pixel_height: 0 })
            .map_err(|e| e.to_string())?;
        let mut cmd = CommandBuilder::new("bash");
        cmd.args(["--noprofile", "--norc", "-i"]);
        let host = sandbox.id.replace('_', "-");
        let prompt = if matches!(exec.kind, TabKind::Primary | TabKind::Terminal) {
            format!("\\[\\e[38;5;207m\\]{}\\[\\e[0m\\] \\[\\e[2m\\]{host}\\[\\e[0m\\] ❯ ", sandbox.harness)
        } else {
            format!("\\[\\e[32m\\]darren@{host}\\[\\e[0m\\]:\\[\\e[34m\\]\\w\\[\\e[0m\\]$ ")
        };
        cmd.env("PS1", prompt);
        cmd.env("TERM", "xterm-256color");
        cmd.env("COLORTERM", "truecolor");
        if let Ok(home) = std::env::var("HOME") {
            cmd.cwd(home);
        }
        let child = pair.slave.spawn_command(cmd).map_err(|e| e.to_string())?;
        drop(pair.slave);
        let mut reader = pair.master.try_clone_reader().map_err(|e| e.to_string())?;
        let writer = pair.master.take_writer().map_err(|e| e.to_string())?;
        if matches!(exec.kind, TabKind::Primary | TabKind::Terminal) {
            // A stand-in for the harness's own banner.
            let banner = format!(
                "\x1b[1;38;5;207m{}\x1b[0m  \x1b[2m{} · mock session on a local PTY\x1b[0m\r\n\r\n",
                sandbox.name, sandbox.harness
            );
            inbox.push(Event::Data(stream, banner.into_bytes()));
        }
        let primary = exec.kind == TabKind::Primary;
        std::thread::spawn(move || {
            let mut buf = [0u8; 16 * 1024];
            loop {
                match reader.read(&mut buf) {
                    Ok(0) | Err(_) => {
                        // What the ending does, as the core's Tab.End says it: the primary takes the
                        // workspace with it, anything else that finished is held.
                        let ended = if primary {
                            Stream { stream, state: StreamState::Ended, end: Some(TabEnd::Workspace), ..Default::default() }
                        } else {
                            Stream { stream, state: StreamState::Ended, end: Some(TabEnd::Held), status: "finished".into(), ..Default::default() }
                        };
                        inbox.push(Event::Stream(ended));
                        inbox.push(Event::Exited(stream, String::new()));
                        break;
                    }
                    Ok(n) => inbox.push(Event::Data(stream, buf[..n].to_vec())),
                }
            }
        });
        Ok(Pty { master: pair.master, writer, child })
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;
    use crate::wire::{Args, Context, SPEC_VERSION};

    // src/spec.json is the CLI's own spec (`task generate`), so a CLI whose spec changed shape
    // fails this until the app reads the new one.
    #[test]
    fn the_bundled_spec_is_the_version_this_app_reads() {
        assert_eq!(bundled_spec().version, SPEC_VERSION);
    }

    // The CLI's messages as it writes them (cli/internal/console/bridge/wire.go) read as the
    // window's events.
    #[test]
    fn messages_read_as_events() {
        let read = |s: &str| event(serde_json::from_str(s).unwrap());
        let Some(Event::Listing(1, Ok(l))) = read(r##"{"type":"snapshot","snapshot":{"sub":1,"topic":"listing","listing":{"boxes":[{"key":"work/b1","id":"b1","server":"work","name":"one","state":"running","harness":"claude","source":"","tags":[],"created":"2026-11-09T12:00:00Z","attachable":true}],"rows":[{"box":"work/b1","section":"k","sectionTitle":"repo","depth":1,"last":true,"rules":[false],"remark":{"text":"harness changes","command":"review config","verb":"review"}}],"grouped":true,"notes":[{"server":"work","title":"server work","detail":"still listing"}],"filter":{"tags":["wip"],"archived":true},"label":"#wip","choices":[{"group":"tag","label":"#wip","detail":"1 box","marked":true,"several":true,"mark":{"tags":["wip"]},"unmark":{"tags":[]}}],"archived":0,"unreachable":[],"waiting":["work"]}}}"##) else { panic!("listing") };
        assert_eq!((l.boxes[0].key.as_str(), l.boxes[0].id.as_str()), ("work/b1", "b1"));
        assert_eq!((l.rows[0].depth, l.rows[0].last, l.rows[0].remark.as_ref().map(|r| r.command.as_str())), (1, true, Some("review config")));
        assert_eq!((l.filter.tags.as_slice(), l.filter.archived, l.choices[0].group), (["wip".to_string()].as_slice(), true, ChoiceGroup::Tag));
        // A listing that could not be read says why, rather than staying at "listing…".
        let Some(Event::Listing(1, Err(e))) = read(r#"{"type":"snapshot","snapshot":{"sub":1,"topic":"listing","error":"connection refused"}}"#) else { panic!("a listing's error") };
        assert_eq!(e, "connection refused");
        let Some(Event::Sessions(2, Ok(s))) = read(r#"{"type":"snapshot","snapshot":{"sub":2,"topic":"sessions","sessions":{"running":true,"sessions":[{"exec":"primary","kind":"primary","title":"","live":true,"argv":[]}],"services":[]}}}"#) else { panic!("sessions") };
        assert_eq!(s.sessions[0].kind, TabKind::Primary);
        let Some(Event::Sessions(3, Err(e))) = read(r#"{"type":"snapshot","snapshot":{"sub":3,"topic":"sessions","error":"no discobox x is listed"}}"#) else { panic!("an error") };
        assert!(e.contains("x"));
        let Some(Event::Result(r)) = read(r#"{"type":"result","result":{"id":4,"status":"done","box":"b1","session":{"exec":"sh1","kind":"shell","title":"bash","live":true,"argv":["bash"]}}}"#) else { panic!("result") };
        assert_eq!((r.id, r.status, r.r#box.as_str(), r.session.unwrap().title.as_str()), (4, Status::Done, "b1", "bash"));
        let Some(Event::Stream(st)) = read(r#"{"type":"stream","stream":{"stream":5,"state":"failed","why":"gone"}}"#) else { panic!("stream") };
        assert_eq!((st.stream, st.state), (5, StreamState::Failed));
        let Some(Event::Palette(6, Ok(p))) = read(r#"{"type":"snapshot","snapshot":{"sub":6,"topic":"palette","palette":{"rows":[{"box":"b1"},{"command":{"command":"stop","label":"stop","detail":"power it off","enabled":false,"why":"not running"},"why":"not running"}],"archived":true,"commit":{"command":"stop","text":"b"},"partial":[],"stale":[]}}}"#) else { panic!("palette") };
        assert_eq!((p.rows[0].r#box.as_str(), p.rows[1].why.as_str(), p.archived, p.commit.map(|c| c.command)), ("b1", "not running", true, Some("stop".into())));
        let Some(Event::Commands(7, Ok(c))) = read(r#"{"type":"snapshot","snapshot":{"sub":7,"topic":"commands","commands":{"entries":[{"command":"help","label":"help","detail":"show the keys","enabled":true}]}}}"#) else { panic!("commands") };
        assert_eq!(c.entries[0].command, "help");
        let Some(Event::Form(f)) = read(r#"{"type":"form","form":{"id":8,"command":4,"title":"Rename","fields":[{"key":"name","label":"name","choices":[],"value":"one","applies":true}]}}"#) else { panic!("form") };
        assert_eq!((f.id, f.command, f.fields[0].key.as_str(), f.fields[0].applies), (8, 4, "name", true));
        let Some(Event::FieldErrors(e)) = read(r#"{"type":"fieldErrors","fieldErrors":{"form":8,"errors":[{"field":"name","why":"a discobox needs a configured name"}]}}"#) else { panic!("fieldErrors") };
        assert_eq!((e.form, e.errors[0].field.as_str()), (8, "name"));
        let Some(Event::Question(q)) = read(r#"{"type":"question","question":{"id":9,"command":4,"title":"Purge","body":"Purge one?","choices":[],"defaultNo":true}}"#) else { panic!("question") };
        assert!(q.default_no && q.choices.is_empty());
    }

    // What the app sends is what the CLI reads.
    #[test]
    fn a_command_is_written_as_the_cli_reads_it() {
        let c = Command { id: 3, command: "shell".into(), targets: vec!["b1".into()], context: Context { pane_focused: true, ..Default::default() }, args: Args { stream: 7, ..Default::default() } };
        let json = |o: Out| serde_json::to_value(o.message()).unwrap();
        let v = json(Out::Command(c));
        assert_eq!(v["type"], "command");
        assert_eq!(v["command"]["command"], "shell");
        assert_eq!(v["command"]["context"]["paneFocused"], true);
        assert_eq!(v["command"]["args"], json!({"stream": 7}));
        assert_eq!(json(Out::Answer(Answer { question: 9, declined: true, ..Default::default() })), json!({"type": "answer", "answer": {"question": 9, "declined": true}}));
        assert_eq!(json(Out::Answer(Answer { question: 9, key: "yes".into(), ..Default::default() })), json!({"type": "answer", "answer": {"question": 9, "key": "yes"}}));
        assert_eq!(json(Out::Submit(Submit { form: 2, cancel: true, ..Default::default() })), json!({"type": "submit", "submit": {"form": 2, "answers": {}, "cancel": true}}));
        let answers = std::collections::HashMap::from([("name".to_string(), "two".to_string())]);
        assert_eq!(json(Out::Submit(Submit { form: 2, answers, draft: true, ..Default::default() })), json!({"type": "submit", "submit": {"form": 2, "answers": {"name": "two"}, "draft": true}}));
        let sub = json(Out::Subscribe(Subscribe { sub: 1, topic: Topic::Listing, filter: Some(Filter { archived: true, tags: vec![], ..Default::default() }), ..Default::default() }));
        assert_eq!(sub, json!({"type": "subscribe", "subscribe": {"sub": 1, "topic": "listing", "filter": {"tags": [], "archived": true}}}));
        let q = PaletteQuery { text: "stop ".into(), recent: vec!["b1".into()], ..Default::default() };
        let sub = json(Out::Subscribe(Subscribe { sub: 2, topic: Topic::Palette, context: Some(Context::default()), palette: Some(q), ..Default::default() }));
        assert_eq!(sub, json!({"type": "subscribe", "subscribe": {"sub": 2, "topic": "palette", "context": {}, "palette": {"text": "stop ", "recent": ["b1"]}}}));
        assert_eq!(json(Out::Unsubscribe(1)), json!({"type": "unsubscribe", "unsubscribe": {"sub": 1}}));
        let open = json(Out::Open { stream: 4, sandbox: "b1".into(), exec: "primary".into(), cols: 80, rows: 24 });
        assert_eq!(open, json!({"type": "open", "open": {"stream": 4, "box": "b1", "exec": "primary", "cols": 80, "rows": 24}}));
    }
}
