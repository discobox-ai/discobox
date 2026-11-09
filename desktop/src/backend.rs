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
//! The wire's types are generated from the bridge's Go types, through its schema (`wire.rs`,
//! `go tool task generate`); none is written here.

use std::collections::HashMap;
use std::io::{Read, Write};
use std::sync::{Arc, Mutex};

use portable_pty::{CommandBuilder, MasterPty, PtySize, native_pty_system};
use rune::Waker;
use serde_json::Value;

use crate::wire::{
    Answer, Box as Sandbox, Command, Entry, Form, Hello, Message, Open, PROTOCOL, Progress, Question, ResizePayload,
    Result as CommandResult, SPEC_VERSION, Sessions, Spec, SpecCommand, SpecGesture, State, Status, Stream, StreamState,
    Submit, Subscribe, Tab, TabKind, Topic, Type, Welcome, kind,
};

impl Sandbox {
    pub fn running(&self) -> bool { self.state == State::Running }
    /// What the cursor row says in place of the name, as the TUI's row does.
    pub fn note(&self) -> Option<&str> {
        [&self.provisioning, &self.message].into_iter().find(|s| !s.is_empty()).map(|s| s.as_str())
    }
}

/// What the app sends, by message: the type and its body.
pub enum Out {
    Subscribe { sub: u32, topic: Topic, sandbox: String },
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
            Out::Subscribe { sub, topic, sandbox } => Message {
                r#type: Type::Subscribe,
                subscribe: Some(Subscribe { sub, topic, r#box: sandbox }),
                ..Default::default()
            },
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

impl Spec {
    /// The spec the CLI printed (`discobox desktop --print-spec`), for running without one.
    pub fn bundled() -> Spec { serde_json::from_str(include_str!("spec.json")).expect("spec.json") }

    pub fn command(&self, id: &str) -> Option<&SpecCommand> { self.commands.iter().find(|c| c.id == id) }

    pub fn gesture(&self, id: &str) -> Option<&SpecGesture> { self.gestures.iter().find(|g| g.id == id) }
}

/// What arrives for the window.
pub enum Event {
    Listing(Vec<Sandbox>),
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
// The bridge: frames on stdin/stdout. kind (1), stream (u32), length (u32), payload.

fn write_frame(w: &mut impl Write, kind: u8, stream: u32, payload: &[u8]) -> std::io::Result<()> {
    let mut head = [0u8; 9];
    head[0] = kind;
    head[1..5].copy_from_slice(&stream.to_be_bytes());
    head[5..9].copy_from_slice(&(payload.len() as u32).to_be_bytes());
    w.write_all(&head)?;
    w.write_all(payload)?;
    w.flush()
}

fn read_frame(r: &mut impl Read) -> std::io::Result<(u8, u32, Vec<u8>)> {
    let mut head = [0u8; 9];
    r.read_exact(&mut head)?;
    let stream = u32::from_be_bytes(head[1..5].try_into().unwrap());
    let len = u32::from_be_bytes(head[5..9].try_into().unwrap()) as usize;
    let mut payload = vec![0; len];
    r.read_exact(&mut payload)?;
    Ok((head[0], stream, payload))
}

/// Why this app and the CLI cannot talk: which side is older.
pub fn mismatch(cli: i64) -> String {
    let older = if cli < PROTOCOL { "the CLI is older" } else { "this desktop app is older" };
    format!("this desktop app speaks bridge protocol {PROTOCOL} and the CLI speaks {cli}: {older}; run the app built with this CLI")
}

/// A message as the window's event, or nothing for one the app does not read.
fn event(m: Message) -> Option<Event> {
    match m.r#type {
        Type::Snapshot => {
            let s = m.snapshot?;
            match s.topic {
                Topic::Listing => s.listing.map(|l| Event::Listing(l.boxes)),
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

/// A message the generated types could not read: never expected, since both sides are built
/// from one schema, so it is always said. What the CLI waits on still gets its way out, read
/// by its ID from the raw message: a result ends its command as failed, a question is
/// declined and a form dismissed (as the app answers every one it reads, until #808), and a
/// stream is failed, so its pane does not wait either.
fn unreadable(payload: &[u8], e: serde_json::Error) -> Option<Event> {
    let v: Value = serde_json::from_slice(payload).unwrap_or_default();
    let kind = v["type"].as_str().unwrap_or("JSON");
    eprintln!("desktop: a {kind} message from the CLI could not be read: {e}");
    let body = &v[kind];
    let why = format!("the desktop app could not read the CLI's {kind}: {e}; run the app built with this CLI");
    let title = || body["title"].as_str().unwrap_or("the CLI's message").to_string();
    match kind {
        "result" => {
            let id = body["id"].as_u64()?;
            Some(Event::Result(CommandResult { id, status: Status::Failed, why, ..Default::default() }))
        }
        "question" => Some(Event::Question(Question { id: body["id"].as_u64()?, title: title(), ..Default::default() })),
        "form" => Some(Event::Form(Form { id: body["id"].as_u64()?, title: title(), ..Default::default() })),
        "stream" => {
            let stream = u32::try_from(body["stream"].as_u64()?).ok()?;
            Some(Event::Stream(Stream { stream, state: StreamState::Failed, why }))
        }
        _ => None,
    }
}

pub struct Bridge {
    out: Arc<Mutex<std::io::Stdout>>,
}

impl Bridge {
    /// Says hello and waits for the welcome, then hands stdin to a reader thread.
    pub fn connect(inbox: Inbox) -> Result<(Bridge, Spec), String> {
        let out = Arc::new(Mutex::new(std::io::stdout()));
        let hello = Message { r#type: Type::Hello, hello: Some(Hello { protocol: PROTOCOL }), ..Default::default() };
        let hello = serde_json::to_vec(&hello).map_err(|e| e.to_string())?;
        write_frame(&mut *out.lock().unwrap(), kind::JSON, 0, &hello).map_err(|e| e.to_string())?;
        let mut input = std::io::stdin().lock();
        let spec = loop {
            let (k, _, payload) = read_frame(&mut input).map_err(|e| format!("the bridge closed: {e}"))?;
            if k != kind::JSON {
                continue;
            }
            // The protocol is read before anything else: another version's fields may not parse.
            let mut v: Value = serde_json::from_slice(&payload).map_err(|e| e.to_string())?;
            // Protocol 1 had no message types: a CLI speaking it answers this hello with an
            // error of its own shape.
            if v.get("type").is_none() {
                return Err(mismatch(1));
            }
            if v["type"] != "welcome" {
                continue;
            }
            let protocol = v["welcome"]["protocol"].as_i64().unwrap_or(0);
            if protocol != PROTOCOL {
                return Err(mismatch(protocol));
            }
            // So is the spec's version, for the same reason.
            let version = v["welcome"]["spec"]["version"].as_i64().unwrap_or(0);
            if version != SPEC_VERSION {
                return Err(format!(
                    "this desktop app reads console spec version {SPEC_VERSION}, and the CLI handed over version {version}: run the app built with this CLI"
                ));
            }
            let welcome: Welcome = serde_json::from_value(v["welcome"].take()).map_err(|e| e.to_string())?;
            break welcome.spec.ok_or("the CLI's welcome carried no spec")?;
        };
        drop(input);
        std::thread::spawn(move || {
            let mut input = std::io::stdin().lock();
            while let Ok((k, stream, payload)) = read_frame(&mut input) {
                match k {
                    kind::JSON => {
                        crate::debug(|| format!("← {}", String::from_utf8_lossy(&payload)));
                        match serde_json::from_slice::<Message>(&payload) {
                            Ok(m) => {
                                if let Some(e) = event(m) {
                                    inbox.push(e);
                                }
                            }
                            Err(e) => {
                                if let Some(e) = unreadable(&payload, e) {
                                    inbox.push(e);
                                }
                            }
                        }
                    }
                    kind::STDOUT | kind::STDERR => inbox.push(Event::Data(stream, payload)),
                    kind::EXIT => inbox.push(Event::Exited(stream, String::from_utf8_lossy(&payload).into())),
                    _ => {}
                }
            }
            // The CLI is gone: nothing more will arrive, and nothing can be sent.
            std::process::exit(0);
        });
        Ok((Bridge { out }, spec))
    }

    fn frame(&self, kind: u8, stream: u32, payload: &[u8]) {
        let _ = write_frame(&mut *self.out.lock().unwrap(), kind, stream, payload);
    }
}

impl Backend for Bridge {
    fn send(&mut self, m: Out) {
        let m = match serde_json::to_string(&m.message()) {
            Ok(m) => m,
            Err(e) => return eprintln!("desktop: a message could not be written: {e}"),
        };
        crate::debug(|| format!("→ {m}"));
        self.frame(kind::JSON, 0, m.as_bytes());
    }
    fn input(&mut self, stream: u32, bytes: &[u8]) { self.frame(kind::INPUT, stream, bytes); }
    fn resize(&mut self, stream: u32, cols: u16, rows: u16) {
        if let Ok(size) = serde_json::to_vec(&ResizePayload { cols, rows }) {
            self.frame(kind::RESIZE, stream, &size);
        }
    }
}

// ---------------------------------------------------------------------------------------------
// The mock: the same messages, answered here with the core's rules roughly. The CLI answers
// them from the core itself.

pub struct Mock {
    inbox: Inbox,
    boxes: Vec<Sandbox>,
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
                branch: "docs/attach".into(), commit: "41d9e2a".into(), parent: "sbx_p8c3n0vd6rgs2hkq".into(),
                ..mock_box("sbx_r5t1y7u3i9o0pzx2", "attach docs", State::Running, "done", "claude", "discobox")
            },
            Sandbox {
                provisioning: "provisioning browser, build, bun…".into(), cpu: 2,
                ..mock_box("sbx_v6svx9a5hndxswfp", "lucid_lalande", State::Starting, "", "shell", "")
            },
            mock_box("sbx_a1b2c3d4e5f6g7h8", "quiet_hopper", State::Stopped, "idle", "opencode", ""),
        ];
        Mock { inbox, boxes, extra: HashMap::new(), ptys: HashMap::new(), made: 0 }
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
                let actions = self.actions(&target);
                if let Some(a) = actions.iter().find(|a| a.command == c.command && !a.enabled) {
                    return fail(&format!("cannot {}: {}", a.label, a.why));
                }
                if let Some(b) = self.boxes.iter_mut().find(|b| b.key == target) {
                    match c.command.as_str() {
                        "stop" => b.state = State::Stopped,
                        "start" => b.state = State::Running,
                        "archive" => b.state = State::Archived,
                        _ => {}
                    }
                }
                self.inbox.push(Event::Listing(self.boxes.clone()));
                CommandResult { status: Status::Done, ..Default::default() }
            }
            "actions" => CommandResult { status: Status::Enter, r#box: target.clone(), entries: self.actions(&target), ..Default::default() },
            _ => enter(""),
        }
    }

    /// The registry's lifecycle commands, with the core's rules roughly.
    fn actions(&self, sandbox: &str) -> Vec<Entry> {
        let running = self.sandbox(sandbox).is_some_and(Sandbox::running);
        Spec::bundled().commands.iter().filter(|c| c.lifecycle).map(|c| {
            let (enabled, why) = match c.id.as_str() {
                "attach" | "shell" => (running, "the box has no container; repair rebuilds it"),
                "review config" | "stop" => (running, "not running"),
                "start" => (!running, "already on, or archived"),
                "upgrade" => (false, "already on the current image"),
                "unarchive" => (false, "not archived"),
                "purge" => (false, "archive it first — purge only takes archived boxes"),
                _ => (true, ""),
            };
            Entry { command: c.id.clone(), label: c.label.clone(), detail: c.detail.clone(), enabled, why: if enabled { String::new() } else { why.into() }, uses: 0 }
        }).collect()
    }
}

impl Backend for Mock {
    fn send(&mut self, m: Out) {
        match m {
            Out::Subscribe { topic: Topic::Listing, .. } => self.inbox.push(Event::Listing(self.boxes.clone())),
            Out::Subscribe { sub, topic: Topic::Sessions, sandbox } => {
                let running = self.sandbox(&sandbox).is_some_and(Sandbox::running);
                let mut sessions = vec![mock_exec("primary", TabKind::Primary, "")];
                sessions.extend(self.extra.get(&sandbox).into_iter().flatten().cloned());
                self.inbox.push(Event::Sessions(sub, Ok(Sessions { running, sessions, services: vec![] })));
            }
            Out::Subscribe { .. } => {}
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
    use crate::wire::{Args, Context};

    // src/spec.json is the CLI's own spec (`task generate`), so a CLI whose spec changed shape
    // fails this until the app reads the new one.
    #[test]
    fn the_bundled_spec_is_the_version_this_app_reads() {
        assert_eq!(Spec::bundled().version, SPEC_VERSION);
    }

    // A frame is kind, stream and length big-endian, then the payload, as
    // cli/internal/console/bridge writes and reads it.
    #[test]
    fn frames_round_trip() {
        let mut wire = Vec::new();
        write_frame(&mut wire, kind::STDOUT, 7, b"hello").unwrap();
        write_frame(&mut wire, kind::JSON, 0, b"").unwrap();
        assert_eq!(&wire[..9], &[1, 0, 0, 0, 7, 0, 0, 0, 5]);
        let mut r = wire.as_slice();
        assert_eq!(read_frame(&mut r).unwrap(), (kind::STDOUT, 7, b"hello".to_vec()));
        assert_eq!(read_frame(&mut r).unwrap(), (kind::JSON, 0, vec![]));
        assert!(read_frame(&mut r).is_err());
    }

    // A CLI of another protocol is refused, naming which side is older.
    #[test]
    fn a_mismatch_says_which_side_is_older() {
        assert!(mismatch(PROTOCOL - 1).contains("the CLI is older"));
        assert!(mismatch(PROTOCOL + 1).contains("this desktop app is older"));
    }

    // The CLI's messages as it writes them (cli/internal/console/bridge/wire.go) read as the
    // window's events.
    #[test]
    fn messages_read_as_events() {
        let read = |s: &str| event(serde_json::from_str(s).unwrap());
        let Some(Event::Listing(boxes)) = read(r#"{"type":"snapshot","snapshot":{"sub":1,"topic":"listing","listing":{"boxes":[{"key":"work/b1","id":"b1","server":"work","name":"one","state":"running","harness":"claude","source":"","tags":[],"created":"2026-11-09T12:00:00Z","attachable":true}],"unreachable":[],"waiting":[]}}}"#) else { panic!("listing") };
        assert_eq!((boxes[0].key.as_str(), boxes[0].id.as_str()), ("work/b1", "b1"));
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

    // What the types cannot read but something waits on still gets its way out: a result fails
    // its command, a question is declined, a form dismissed, a stream failed; the rest is only
    // said.
    #[test]
    fn an_unreadable_result_fails_its_command() {
        let unread = |s: &str| unreadable(s.as_bytes(), serde_json::from_str::<Message>(s).unwrap_err());
        let Some(Event::Result(r)) = unread(r#"{"type":"result","result":{"id":6,"status":"queued"}}"#) else { panic!("no result") };
        assert_eq!((r.id, r.status), (6, Status::Failed));
        assert!(r.why.contains("could not read"));
        let Some(Event::Question(q)) = unread(r#"{"type":"question","question":{"id":7,"command":1,"title":"Trust?","choices":[{"key":1}]}}"#) else { panic!("no question") };
        assert_eq!((q.id, q.title.as_str()), (7, "Trust?"));
        let Some(Event::Form(f)) = unread(r#"{"type":"form","form":{"id":8,"command":1,"title":"Rename","fields":null}}"#) else { panic!("no form") };
        assert_eq!(f.id, 8);
        let Some(Event::Stream(st)) = unread(r#"{"type":"stream","stream":{"stream":9,"state":"paused"}}"#) else { panic!("no stream") };
        assert_eq!((st.stream, st.state), (9, StreamState::Failed));
        assert!(unread(r#"{"type":"snapshot","snapshot":{"sub":1,"topic":"weather"}}"#).is_none());
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
        let open = json(Out::Open { stream: 4, sandbox: "b1".into(), exec: "primary".into(), cols: 80, rows: 24 });
        assert_eq!(open, json!({"type": "open", "open": {"stream": 4, "box": "b1", "exec": "primary", "cols": 80, "rows": 24}}));
    }
}
