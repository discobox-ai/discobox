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
    Answer, Box as Sandbox, Command, Entry, Form, Message, Open, Progress, Question, Result as CommandResult, Sessions,
    Spec, State, Status, Stream, StreamState, Submit, Subscribe, Tab, TabKind, Topic, Type,
};

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

/// The spec the CLI printed (`discobox desktop --print-spec`), for running without one.
pub fn bundled_spec() -> Spec { serde_json::from_str(include_str!("spec.json")).expect("spec.json") }

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
// The bridge: the client (`discobox_bridge`) on stdin and stdout.

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
        bundled_spec().commands.iter().filter(|c| c.lifecycle).map(|c| {
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
