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
//! The wire types below mirror `cli/internal/console/bridge/wire.go` by hand until they are
//! generated from it (#799).

use std::collections::HashMap;
use std::io::{Read, Write};
use std::sync::{Arc, Mutex};

use portable_pty::{CommandBuilder, MasterPty, PtySize, native_pty_system};
use rune::Waker;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

// ---------------------------------------------------------------------------------------------
// The wire's shapes

/// The bridge protocol this app speaks (`bridge.Protocol`). The CLI pins the app it runs, so a
/// CLI that speaks another is a development override, and the app says which side is older.
pub const PROTOCOL: u32 = 2;

/// One box as its row is drawn.
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(rename_all = "camelCase", default)]
pub struct Sandbox {
    /// The key the CLI knows this box by, server and ID together: opaque here.
    #[serde(rename = "key")]
    pub id: String,
    /// The box's ID on its server, which is what is shown.
    #[serde(rename = "id")]
    pub sandbox_id: String,
    pub name: String,
    pub state: String,
    pub agent: String,
    pub agent_msg: String,
    pub harness: String,
    pub source: String,
    pub tags: Vec<String>,
    pub branch: String,
    pub commit: String,
    pub dirty: bool,
    pub added: u32,
    pub deleted: u32,
    pub cpu: u32,
    pub memory_bytes: u64,
    pub created: String,
    pub parent: String,
    pub message: String,
    pub provisioning: String,
}

impl Sandbox {
    pub fn running(&self) -> bool { self.state == "running" }
    /// The ID to show: the box's own on its server (the mock has only `id`).
    pub fn shown_id(&self) -> &str { if self.sandbox_id.is_empty() { &self.id } else { &self.sandbox_id } }
    /// What the cursor row says in place of the name, as the TUI's row does.
    pub fn note(&self) -> Option<&str> {
        [&self.provisioning, &self.message].into_iter().find(|s| !s.is_empty()).map(|s| s.as_str())
    }
}

/// One session as its tab is drawn (`bridge.Tab`).
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Exec {
    #[serde(rename = "exec")]
    pub id: String,
    /// primary, terminal, shell, tool or service.
    pub kind: String,
    #[serde(rename = "title")]
    pub label: String,
}

/// One command as it stands for what it was asked about (`bridge.Entry`).
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Action {
    pub command: String,
    pub label: String,
    pub detail: String,
    pub enabled: bool,
    pub why: String,
}

/// Where a command is asked from: the app's navigation, which the core's rules read.
#[derive(Clone, Debug, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase", default)]
pub struct Context {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub workspace: String,
    pub has_workspace: bool,
    pub workspace_shown: bool,
    pub pane_focused: bool,
    pub list_has_keys: bool,
    pub tree: bool,
    pub tree_shown: bool,
    pub tree_fits: bool,
    pub page_shown: bool,
}

/// What a command takes beyond its targets: the stream a session it makes attaches to, and
/// the session it acts on.
#[derive(Clone, Debug, Serialize, Deserialize, Default)]
#[serde(default)]
pub struct Args {
    #[serde(skip_serializing_if = "is_zero")]
    pub stream: u32,
    #[serde(skip_serializing_if = "is_zero16")]
    pub cols: u16,
    #[serde(skip_serializing_if = "is_zero16")]
    pub rows: u16,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub exec: String,
}

fn is_zero(n: &u32) -> bool { *n == 0 }
fn is_zero16(n: &u16) -> bool { *n == 0 }

/// A registry command for the core to rule on and run (`bridge.Command`).
#[derive(Clone, Debug, Serialize, Deserialize, Default)]
#[serde(default)]
pub struct CommandMsg {
    pub id: u64,
    pub command: String,
    pub targets: Vec<String>,
    pub context: Context,
    pub args: Args,
}

/// How a command ended (`bridge.Result`).
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct CommandResult {
    pub id: u64,
    /// done, failed, canceled, or enter: the app's own flow, which the core has ruled can run.
    pub status: String,
    pub why: String,
    pub note: String,
    #[serde(rename = "box")]
    pub sandbox: String,
    pub session: Option<Exec>,
    pub entries: Vec<Action>,
}

/// A topic's value, whole. Only the topics the app draws are read.
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Snapshot {
    pub sub: u32,
    pub topic: String,
    pub error: String,
    pub listing: Option<Listing>,
    pub sessions: Option<Sessions>,
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Listing {
    pub boxes: Vec<Sandbox>,
}

/// What a box's workspace opens, in the core's order.
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Sessions {
    pub running: bool,
    pub sessions: Vec<Exec>,
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Question {
    pub id: u64,
    pub command: u64,
    pub title: String,
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Form {
    pub id: u64,
    pub command: u64,
    pub title: String,
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Progress {
    pub command: u64,
    pub text: String,
    pub warning: bool,
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Stream {
    pub stream: u32,
    /// attached, failed, reconnecting or reconnected.
    pub state: String,
    pub why: String,
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Welcome {
    pub protocol: u32,
    pub spec: Option<Spec>,
}

/// One JSON message on the bridge: its type, and that type's body under the same name.
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Message {
    #[serde(rename = "type")]
    pub kind: String,
    pub welcome: Option<Welcome>,
    pub snapshot: Option<Snapshot>,
    pub result: Option<CommandResult>,
    pub form: Option<Form>,
    pub question: Option<Question>,
    pub progress: Option<Progress>,
    pub stream: Option<Stream>,
}

/// What the app sends, by message: the type and its body.
pub enum Out {
    Subscribe { sub: u32, topic: &'static str, sandbox: String },
    Command(CommandMsg),
    Open { stream: u32, sandbox: String, exec: String, cols: u16, rows: u16 },
    /// A question left unanswered: the app draws none yet (#808).
    Decline(u64),
    /// A form dismissed: the app draws none yet (#808).
    Dismiss(u64),
}

impl Out {
    fn message(&self) -> Value {
        match self {
            Out::Subscribe { sub, topic, sandbox } => {
                json!({"type": "subscribe", "subscribe": {"sub": sub, "topic": topic, "box": sandbox}})
            }
            Out::Command(c) => json!({"type": "command", "command": c}),
            Out::Open { stream, sandbox, exec, cols, rows } => {
                json!({"type": "open", "open": {"stream": stream, "box": sandbox, "exec": exec, "cols": cols, "rows": rows}})
            }
            Out::Decline(q) => json!({"type": "answer", "answer": {"question": q, "declined": true}}),
            Out::Dismiss(f) => json!({"type": "submit", "submit": {"form": f, "answers": {}, "cancel": true}}),
        }
    }
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Glyph {
    pub glyph: String,
    /// The colour token it is drawn in.
    pub color: String,
    pub word: String,
}

/// A colour token in both palettes, as `#rrggbb`.
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct ColorToken {
    pub dark: String,
    pub light: String,
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct DesktopBinding {
    /// Chords, where `primary` is Cmd on macOS and Ctrl+Shift elsewhere.
    pub keys: Vec<String>,
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Bindings {
    pub desktop: DesktopBinding,
}

/// One gesture of the registry: a binding that is not a command (moving a cursor, a pane by
/// its number), which the app acts on itself and never runs as a command.
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Gesture {
    pub id: String,
    pub label: String,
    pub bindings: Bindings,
}

/// One command of the console core's registry.
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Command {
    pub id: String,
    pub label: String,
    pub detail: String,
    pub aliases: Vec<String>,
    /// `box` for a command that takes a box, `window` for one that takes none.
    pub target: String,
    pub lifecycle: bool,
    pub bindings: Bindings,
}

/// The console's spec as the CLI hands it over (`console.NewSpec`, in
/// `cli/internal/console/spec.go`): the colour tokens in both palettes, the state and agent
/// glyphs, and the registry's commands with each frontend's bindings.
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Spec {
    pub version: u32,
    pub colors: HashMap<String, ColorToken>,
    pub states: HashMap<String, Glyph>,
    pub agents: HashMap<String, Glyph>,
    pub commands: Vec<Command>,
    /// Every binding that is not a command's: with the commands', every shortcut and list key
    /// the app takes (an open menu or palette keeps its own).
    pub gestures: Vec<Gesture>,
}

/// The spec's shape this app reads (`console.SpecVersion`). A CLI that hands over another is
/// refused at `hello`, rather than read into empty labels and bindings.
pub const SPEC_VERSION: u32 = 3;

impl Spec {
    /// The spec the CLI printed (`discobox desktop --print-spec`), for running without one.
    pub fn bundled() -> Spec { serde_json::from_str(include_str!("spec.json")).expect("spec.json") }

    pub fn command(&self, id: &str) -> Option<&Command> { self.commands.iter().find(|c| c.id == id) }

    pub fn gesture(&self, id: &str) -> Option<&Gesture> { self.gestures.iter().find(|g| g.id == id) }
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

const JSON: u8 = 255;
const INPUT: u8 = 0;
const STDOUT: u8 = 1;
const STDERR: u8 = 2;
const RESIZE: u8 = 3;
const EXIT: u8 = 6;

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
pub fn mismatch(cli: u32) -> String {
    let older = if cli < PROTOCOL { "the CLI is older" } else { "this desktop app is older" };
    format!("this desktop app speaks bridge protocol {PROTOCOL} and the CLI speaks {cli}: {older}; run the app built with this CLI")
}

/// A message as the window's event, or nothing for one the app does not read.
fn event(m: Message) -> Option<Event> {
    match m.kind.as_str() {
        "snapshot" => {
            let s = m.snapshot?;
            match s.topic.as_str() {
                "listing" => s.listing.map(|l| Event::Listing(l.boxes)),
                "sessions" => Some(Event::Sessions(s.sub, match s.sessions {
                    Some(v) if s.error.is_empty() => Ok(v),
                    _ => Err(s.error),
                })),
                _ => None,
            }
        }
        "result" => m.result.map(Event::Result),
        "stream" => m.stream.map(Event::Stream),
        "question" => m.question.map(Event::Question),
        "form" => m.form.map(Event::Form),
        "progress" => m.progress.map(Event::Progress),
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
        let hello = json!({"type": "hello", "hello": {"protocol": PROTOCOL}}).to_string();
        write_frame(&mut *out.lock().unwrap(), JSON, 0, hello.as_bytes()).map_err(|e| e.to_string())?;
        let mut input = std::io::stdin().lock();
        let spec = loop {
            let (kind, _, payload) = read_frame(&mut input).map_err(|e| format!("the bridge closed: {e}"))?;
            if kind != JSON {
                continue;
            }
            // The protocol is read before anything else: another version's fields may not parse.
            let v: Value = serde_json::from_slice(&payload).map_err(|e| e.to_string())?;
            // Protocol 1 had no message types: a CLI speaking it answers this hello with an
            // error of its own shape.
            if v.get("type").is_none() {
                return Err(mismatch(1));
            }
            if v["type"] != "welcome" {
                continue;
            }
            let protocol = v["welcome"]["protocol"].as_u64().unwrap_or(0) as u32;
            if protocol != PROTOCOL {
                return Err(mismatch(protocol));
            }
            let spec = &v["welcome"]["spec"];
            let version = spec["version"].as_u64().unwrap_or(0);
            if version != SPEC_VERSION as u64 {
                return Err(format!(
                    "this desktop app reads console spec version {SPEC_VERSION}, and the CLI handed over version {version}: run the app built with this CLI"
                ));
            }
            break serde_json::from_value(spec.clone()).map_err(|e| e.to_string())?;
        };
        drop(input);
        std::thread::spawn(move || {
            let mut input = std::io::stdin().lock();
            while let Ok((kind, stream, payload)) = read_frame(&mut input) {
                match kind {
                    JSON => {
                        crate::debug(|| format!("← {}", String::from_utf8_lossy(&payload)));
                        if let Some(e) = serde_json::from_slice::<Message>(&payload).ok().and_then(event) {
                            inbox.push(e);
                        }
                    }
                    STDOUT | STDERR => inbox.push(Event::Data(stream, payload)),
                    EXIT => inbox.push(Event::Exited(stream, String::from_utf8_lossy(&payload).into())),
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
        let m = m.message().to_string();
        crate::debug(|| format!("→ {m}"));
        self.frame(JSON, 0, m.as_bytes());
    }
    fn input(&mut self, stream: u32, bytes: &[u8]) { self.frame(INPUT, stream, bytes); }
    fn resize(&mut self, stream: u32, cols: u16, rows: u16) {
        self.frame(RESIZE, stream, json!({"cols": cols, "rows": rows}).to_string().as_bytes());
    }
}

// ---------------------------------------------------------------------------------------------
// The mock: the same messages, answered here with the core's rules roughly. The CLI answers
// them from the core itself.

pub struct Mock {
    inbox: Inbox,
    boxes: Vec<Sandbox>,
    extra: HashMap<String, Vec<Exec>>,
    ptys: HashMap<u32, Pty>,
    made: u64,
}

fn mock_box(v: Value) -> Sandbox { serde_json::from_value(v).unwrap() }

fn mock_exec(id: &str, kind: &str, label: &str) -> Exec {
    Exec { id: id.into(), kind: kind.into(), label: label.into() }
}

impl Mock {
    pub fn new(inbox: Inbox) -> Mock {
        let boxes = vec![
            mock_box(json!({"key": "sbx_k2m9q4tz7wbf1xay", "name": "rune console port", "state": "running", "agent": "working",
                "harness": "claude", "source": "discobox", "tags": ["wip", "tui"], "branch": "rune-console", "commit": "00541f9",
                "dirty": true, "added": 412, "deleted": 37, "cpu": 12, "memoryBytes": 2_254_857_830u64})),
            mock_box(json!({"key": "sbx_p8c3n0vd6rgs2hkq", "name": "reaper race", "state": "running", "agent": "blocked",
                "harness": "codex", "source": "discobox", "tags": ["ENG-12"], "branch": "fix/reaper-race", "commit": "8be21c0",
                "added": 58, "deleted": 12, "cpu": 3, "memoryBytes": 1_503_238_553u64})),
            mock_box(json!({"key": "sbx_r5t1y7u3i9o0pzx2", "name": "attach docs", "state": "running", "agent": "done",
                "harness": "claude", "source": "discobox", "branch": "docs/attach", "commit": "41d9e2a", "parent": "sbx_p8c3n0vd6rgs2hkq"})),
            mock_box(json!({"key": "sbx_v6svx9a5hndxswfp", "name": "lucid_lalande", "state": "starting", "harness": "shell",
                "source": "", "provisioning": "provisioning browser, build, bun…", "cpu": 2})),
            mock_box(json!({"key": "sbx_a1b2c3d4e5f6g7h8", "name": "quiet_hopper", "state": "stopped", "agent": "idle",
                "harness": "opencode", "source": ""})),
        ];
        Mock { inbox, boxes, extra: HashMap::new(), ptys: HashMap::new(), made: 0 }
    }

    fn sandbox(&self, key: &str) -> Option<&Sandbox> { self.boxes.iter().find(|b| b.id == key) }

    fn spawn(&mut self, sandbox: &str, exec: &Exec, stream: u32, cols: u16, rows: u16) -> Result<(), String> {
        let sbx = self.sandbox(sandbox).cloned().unwrap_or_default();
        let pty = Pty::spawn(&sbx, exec, stream, cols, rows, self.inbox.clone())?;
        self.ptys.insert(stream, pty);
        Ok(())
    }

    fn command(&mut self, c: CommandMsg) -> CommandResult {
        let target = c.targets.first().cloned().unwrap_or_default();
        let workspace = c.context.workspace.clone();
        let on_pane = c.context.pane_focused && c.context.workspace_shown;
        let fail = |why: &str| CommandResult { status: "failed".into(), why: why.into(), ..Default::default() };
        let enter = |sandbox: &str| CommandResult { status: "enter".into(), sandbox: sandbox.into(), ..Default::default() };
        let running = self.sandbox(&target).is_some_and(Sandbox::running);
        match c.command.as_str() {
            "attach" if running => enter(&target),
            "attach" | "shell" if !running => fail("the box has no container; repair rebuilds it"),
            "terminal" | "end" | "maximize" | "detach" if !on_pane => fail("requires a workspace pane"),
            "shell" | "terminal" => {
                let sandbox = if c.command == "shell" { target } else { workspace };
                self.made += 1;
                let exec = if c.command == "shell" {
                    mock_exec(&format!("exec_{}", self.made), "shell", "bash")
                } else {
                    let harness = self.sandbox(&sandbox).map(|b| b.harness.clone()).unwrap_or_default();
                    mock_exec(&format!("exec_{}", self.made), "terminal", &harness)
                };
                if let Err(e) = self.spawn(&sandbox, &exec, c.args.stream, c.args.cols.max(1), c.args.rows.max(1)) {
                    return fail(&e);
                }
                self.extra.entry(sandbox.clone()).or_default().push(exec.clone());
                CommandResult { status: "done".into(), sandbox, session: Some(exec), ..Default::default() }
            }
            "end" if c.args.exec == "primary" => fail("the primary session ends with its box; stop the box instead"),
            "end" => {
                if let Some(mut p) = self.ptys.remove(&c.args.stream) {
                    let _ = p.child.kill();
                }
                if let Some(v) = self.extra.get_mut(&workspace) {
                    v.retain(|e| e.id != c.args.exec);
                }
                CommandResult { status: "done".into(), ..Default::default() }
            }
            "stop" | "start" | "upgrade" | "repair" | "archive" | "unarchive" => {
                let actions = self.actions(&target);
                if let Some(a) = actions.iter().find(|a| a.command == c.command && !a.enabled) {
                    return fail(&format!("cannot {}: {}", a.label, a.why));
                }
                if let Some(b) = self.boxes.iter_mut().find(|b| b.id == target) {
                    match c.command.as_str() {
                        "stop" => b.state = "stopped".into(),
                        "start" => b.state = "running".into(),
                        "archive" => b.state = "archived".into(),
                        _ => {}
                    }
                }
                self.inbox.push(Event::Listing(self.boxes.clone()));
                CommandResult { status: "done".into(), ..Default::default() }
            }
            "actions" => CommandResult { status: "enter".into(), sandbox: target.clone(), entries: self.actions(&target), ..Default::default() },
            _ => enter(""),
        }
    }

    /// The registry's lifecycle commands, with the core's rules roughly.
    fn actions(&self, sandbox: &str) -> Vec<Action> {
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
            Action { command: c.id.clone(), label: c.label.clone(), detail: c.detail.clone(), enabled, why: if enabled { String::new() } else { why.into() } }
        }).collect()
    }
}

impl Backend for Mock {
    fn send(&mut self, m: Out) {
        match m {
            Out::Subscribe { topic: "listing", .. } => self.inbox.push(Event::Listing(self.boxes.clone())),
            Out::Subscribe { sub, topic: "sessions", sandbox } => {
                let running = self.sandbox(&sandbox).is_some_and(Sandbox::running);
                let mut sessions = vec![mock_exec("primary", "primary", "")];
                sessions.extend(self.extra.get(&sandbox).into_iter().flatten().cloned());
                self.inbox.push(Event::Sessions(sub, Ok(Sessions { running, sessions })));
            }
            Out::Subscribe { .. } => {}
            Out::Command(c) => {
                let mut r = self.command(c.clone());
                r.id = c.id;
                self.inbox.push(Event::Result(r));
            }
            Out::Open { stream, sandbox, exec, cols, rows } => {
                let kind = if exec == "primary" { "primary" } else { "shell" };
                let state = match self.spawn(&sandbox, &mock_exec(&exec, kind, ""), stream, cols, rows) {
                    Ok(()) => Stream { stream, state: "attached".into(), why: String::new() },
                    Err(why) => Stream { stream, state: "failed".into(), why },
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
    fn spawn(sandbox: &Sandbox, exec: &Exec, stream: u32, cols: u16, rows: u16, inbox: Inbox) -> Result<Pty, String> {
        let pair = native_pty_system()
            .openpty(PtySize { rows, cols, pixel_width: 0, pixel_height: 0 })
            .map_err(|e| e.to_string())?;
        let mut cmd = CommandBuilder::new("bash");
        cmd.args(["--noprofile", "--norc", "-i"]);
        let host = sandbox.id.replace('_', "-");
        let prompt = if matches!(exec.kind.as_str(), "primary" | "terminal") {
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
        if matches!(exec.kind.as_str(), "primary" | "terminal") {
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
    use super::*;

    // src/spec.json is the CLI's own spec (`task generate`), so a CLI whose spec changed shape
    // fails this until the app reads the new one.
    #[test]
    fn the_bundled_spec_is_the_version_this_app_reads() {
        assert_eq!(Spec::bundled().version, SPEC_VERSION);
    }

    // The frame kinds are execstream's (pkg/execstream/frame/frame.go), which the CLI's side
    // of the bridge passes through unchanged.
    #[test]
    fn frame_kinds_are_execstreams() {
        assert_eq!((INPUT, STDOUT, STDERR, RESIZE, EXIT, JSON), (0, 1, 2, 3, 6, 255));
    }

    // A frame is kind, stream and length big-endian, then the payload, as
    // cli/internal/console/bridge writes and reads it.
    #[test]
    fn frames_round_trip() {
        let mut wire = Vec::new();
        write_frame(&mut wire, STDOUT, 7, b"hello").unwrap();
        write_frame(&mut wire, JSON, 0, b"").unwrap();
        assert_eq!(&wire[..9], &[1, 0, 0, 0, 7, 0, 0, 0, 5]);
        let mut r = wire.as_slice();
        assert_eq!(read_frame(&mut r).unwrap(), (STDOUT, 7, b"hello".to_vec()));
        assert_eq!(read_frame(&mut r).unwrap(), (JSON, 0, vec![]));
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
        let Some(Event::Listing(boxes)) = read(r#"{"type":"snapshot","snapshot":{"sub":1,"topic":"listing","listing":{"boxes":[{"key":"work/b1","id":"b1","name":"one","state":"running","tags":[]}],"unreachable":[],"waiting":[]}}}"#) else { panic!("listing") };
        assert_eq!((boxes[0].id.as_str(), boxes[0].shown_id()), ("work/b1", "b1"));
        let Some(Event::Sessions(2, Ok(s))) = read(r#"{"type":"snapshot","snapshot":{"sub":2,"topic":"sessions","sessions":{"running":true,"sessions":[{"exec":"primary","kind":"primary","title":"","live":true,"argv":[]}],"services":[]}}}"#) else { panic!("sessions") };
        assert_eq!(s.sessions[0].kind, "primary");
        let Some(Event::Sessions(3, Err(e))) = read(r#"{"type":"snapshot","snapshot":{"sub":3,"topic":"sessions","error":"no discobox x is listed"}}"#) else { panic!("an error") };
        assert!(e.contains("x"));
        let Some(Event::Result(r)) = read(r#"{"type":"result","result":{"id":4,"status":"done","box":"b1","session":{"exec":"sh1","kind":"shell","title":"bash","live":true,"argv":["bash"]}}}"#) else { panic!("result") };
        assert_eq!((r.id, r.status.as_str(), r.sandbox.as_str(), r.session.unwrap().label.as_str()), (4, "done", "b1", "bash"));
        let Some(Event::Stream(st)) = read(r#"{"type":"stream","stream":{"stream":5,"state":"failed","why":"gone"}}"#) else { panic!("stream") };
        assert_eq!((st.stream, st.state.as_str()), (5, "failed"));
        assert!(read(r#"{"type":"fieldErrors","fieldErrors":{"form":1,"errors":[]}}"#).is_none());
    }

    // What the app sends is what the CLI reads.
    #[test]
    fn a_command_is_written_as_the_cli_reads_it() {
        let c = CommandMsg { id: 3, command: "shell".into(), targets: vec!["b1".into()], context: Context { pane_focused: true, ..Default::default() }, args: Args { stream: 7, ..Default::default() } };
        let v = Out::Command(c).message();
        assert_eq!(v["type"], "command");
        assert_eq!(v["command"]["command"], "shell");
        assert_eq!(v["command"]["context"]["paneFocused"], true);
        assert_eq!(v["command"]["args"], json!({"stream": 7}));
        assert_eq!(Out::Decline(9).message(), json!({"type": "answer", "answer": {"question": 9, "declined": true}}));
    }
}
