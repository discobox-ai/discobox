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
    Answer, Box as Sandbox, ChoiceGroup, Command, Entry, Filter, FilterChoice, Form, Listing, Message, Open, Progress,
    Question, Remark, Result as CommandResult, Row, Sessions, Spec, State, Status, Stream, StreamState, Submit, Subscribe, Tab,
    TabKind, Topic, Type, Unsubscribe,
};

/// What the app sends, by message: the type and its body.
pub enum Out {
    /// A topic followed; the listing's narrowed to `filter` (none: what the last window left).
    Subscribe { sub: u32, topic: Topic, sandbox: String, filter: Option<Filter> },
    Unsubscribe(u32),
    Command(Command),
    Open { stream: u32, sandbox: String, exec: String, cols: u16, rows: u16 },
    /// A question left unanswered: the app draws none yet (#808).
    Decline(u64),
    /// A form dismissed: the app draws none yet (#808).
    Dismiss(u64),
}

impl Out {
    fn message(self) -> Message {
        match self {
            Out::Subscribe { sub, topic, sandbox, filter } => Message {
                r#type: Type::Subscribe,
                subscribe: Some(Subscribe { sub, topic, r#box: sandbox, filter }),
                ..Default::default()
            },
            Out::Unsubscribe(sub) => Message { r#type: Type::Unsubscribe, unsubscribe: Some(Unsubscribe { sub }), ..Default::default() },
            Out::Command(c) => Message { r#type: Type::Command, command: Some(c), ..Default::default() },
            Out::Open { stream, sandbox, exec, cols, rows } => Message {
                r#type: Type::Open,
                open: Some(Open { stream, r#box: sandbox, exec, cols: cols.into(), rows: rows.into() }),
                ..Default::default()
            },
            Out::Decline(question) => Message {
                r#type: Type::Answer,
                answer: Some(Answer { question, declined: true, ..Default::default() }),
                ..Default::default()
            },
            Out::Dismiss(form) => Message {
                r#type: Type::Submit,
                submit: Some(Submit { form, cancel: true, ..Default::default() }),
                ..Default::default()
            },
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
    Result(CommandResult),
    Stream(Stream),
    Question(Question),
    Form(Form),
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
                _ => None,
            }
        }
        Type::Result => m.result.map(Event::Result),
        Type::Stream => m.stream.map(Event::Stream),
        Type::Question => m.question.map(Event::Question),
        Type::Form => m.form.map(Event::Form),
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
        Mock { inbox, boxes, parents, list_sub: 0, filter, extra: HashMap::new(), ptys: HashMap::new(), made: 0 }
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
            label: if parts.is_empty() { "all discoboxes".into() } else { parts.join(" · ") },
            choices, archived: archived as i64, unreachable: vec![], waiting: vec![],
        }
    }

    fn relist(&self) { self.inbox.push(Event::Listing(self.list_sub, Ok(self.listing()))); }

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
            "refresh" => {
                self.relist();
                CommandResult { status: Status::Done, ..Default::default() }
            }
            "all" => fail("available in a wide attach window"),
            "actions" => CommandResult { status: Status::Enter, r#box: target.clone(), entries: self.actions(&target), ..Default::default() },
            _ => enter(""),
        }
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
}

impl Backend for Mock {
    fn send(&mut self, m: Out) {
        match m {
            Out::Subscribe { sub, topic: Topic::Listing, filter, .. } => {
                self.list_sub = sub;
                if let Some(f) = filter {
                    self.filter = f;
                }
                self.relist();
            }
            Out::Subscribe { sub, topic: Topic::Sessions, sandbox, .. } => {
                let running = self.sandbox(&sandbox).is_some_and(Sandbox::running);
                let mut sessions = vec![mock_exec("primary", TabKind::Primary, "")];
                sessions.extend(self.extra.get(&sandbox).into_iter().flatten().cloned());
                self.inbox.push(Event::Sessions(sub, Ok(Sessions { running, sessions, services: vec![] })));
            }
            Out::Subscribe { .. } | Out::Unsubscribe(_) => {}
            Out::Command(c) => {
                let mut r = self.command(c.clone());
                r.id = c.id;
                self.inbox.push(Event::Result(r));
            }
            Out::Open { stream, sandbox, exec, cols, rows } => {
                let kind = if exec == "primary" { TabKind::Primary } else { TabKind::Shell };
                let state = match self.spawn(&sandbox, &mock_exec(&exec, kind, ""), stream, cols, rows) {
                    Ok(()) => Stream { stream, state: StreamState::Attached, why: String::new() },
                    Err(why) => Stream { stream, state: StreamState::Failed, why },
                };
                self.inbox.push(Event::Stream(state));
            }
            Out::Decline(_) | Out::Dismiss(_) => {}
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
        std::thread::spawn(move || {
            let mut buf = [0u8; 16 * 1024];
            loop {
                match reader.read(&mut buf) {
                    Ok(0) | Err(_) => {
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
        assert!(read(r#"{"type":"fieldErrors","fieldErrors":{"form":1,"errors":[]}}"#).is_none());
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
        assert_eq!(json(Out::Decline(9)), json!({"type": "answer", "answer": {"question": 9, "declined": true}}));
        assert_eq!(json(Out::Dismiss(2)), json!({"type": "submit", "submit": {"form": 2, "answers": {}, "cancel": true}}));
        let sub = json(Out::Subscribe { sub: 1, topic: Topic::Listing, sandbox: String::new(), filter: Some(Filter { archived: true, tags: vec![], ..Default::default() }) });
        assert_eq!(sub, json!({"type": "subscribe", "subscribe": {"sub": 1, "topic": "listing", "filter": {"tags": [], "archived": true}}}));
        assert_eq!(json(Out::Unsubscribe(1)), json!({"type": "unsubscribe", "unsubscribe": {"sub": 1}}));
        let open = json(Out::Open { stream: 4, sandbox: "b1".into(), exec: "primary".into(), cols: 80, rows: 24 });
        assert_eq!(open, json!({"type": "open", "open": {"stream": 4, "box": "b1", "exec": "primary", "cols": 80, "rows": 24}}));
    }
}
