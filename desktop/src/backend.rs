//! Where boxes and their terminal streams come from.
//!
//! `discobox desktop` runs this app with `--bridge` and serves the console's DataSource on
//! its stdin and stdout (`cli/internal/cli/desktop.go`). Every request is answered
//! asynchronously into the `Inbox`, with the stream output and the listing the bridge pushes,
//! and the window's poll drains it. Nothing here blocks the window but the opening `hello`.
//!
//! Without `--bridge` the `Mock` answers the same requests, with made-up boxes whose panes are
//! local shells on PTYs, for developing the app and for headless snapshots.

use std::collections::HashMap;
use std::io::{Read, Write};
use std::sync::{Arc, Mutex};

use portable_pty::{CommandBuilder, MasterPty, PtySize, native_pty_system};
use rune::Waker;
use serde::Deserialize;
use serde_json::{Value, json};

// ---------------------------------------------------------------------------------------------
// The wire's shapes

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(rename_all = "camelCase", default)]
pub struct Sandbox {
    pub id: String,
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
    /// What the cursor row says in place of the name, as the TUI's row does.
    pub fn note(&self) -> Option<&str> {
        [&self.provisioning, &self.message].into_iter().find(|s| !s.is_empty()).map(|s| s.as_str())
    }
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Exec {
    pub id: String,
    /// primary, terminal, shell or tool.
    pub kind: String,
    pub label: String,
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Action {
    pub command: String,
    pub key: String,
    pub label: String,
    pub detail: String,
    pub enabled: bool,
    pub why: String,
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Glyph {
    pub glyph: String,
    pub color: String,
    pub word: String,
}

#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Command {
    pub id: String,
    pub title: String,
    pub group: String,
    pub keys: HashMap<String, Vec<String>>,
}

/// The console's UX as the CLI hands it over (`tui.Spec`): colour tokens, state glyphs, and
/// the commands with each frontend's keys.
#[derive(Clone, Debug, Deserialize, Default)]
#[serde(default)]
pub struct Spec {
    pub version: u32,
    pub colors: HashMap<String, String>,
    pub states: HashMap<String, Glyph>,
    pub agents: HashMap<String, Glyph>,
    pub commands: Vec<Command>,
}

impl Spec {
    /// The spec the CLI printed (`discobox desktop --print-spec`), for running without one.
    pub fn bundled() -> Spec { serde_json::from_str(include_str!("spec.json")).expect("spec.json") }

    pub fn command(&self, id: &str) -> Option<&Command> { self.commands.iter().find(|c| c.id == id) }
}

/// What arrives for the window.
pub enum Event {
    Listing(Vec<Sandbox>),
    Reply(u64, Result<Value, String>),
    Data(u32, Vec<u8>),
    Exited(u32, String),
    StreamState(u32, String),
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
    /// Ask for something; the answer arrives as `Event::Reply` with the returned id.
    fn request(&mut self, op: &str, args: Value) -> u64;
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

pub struct Bridge {
    out: Arc<Mutex<std::io::Stdout>>,
    next: u64,
}

impl Bridge {
    /// Says hello and waits for the spec, then hands stdin to a reader thread.
    pub fn connect(inbox: Inbox) -> Result<(Bridge, Spec), String> {
        let out = Arc::new(Mutex::new(std::io::stdout()));
        let hello = json!({"id": 0, "op": "hello"}).to_string();
        write_frame(&mut *out.lock().unwrap(), JSON, 0, hello.as_bytes()).map_err(|e| e.to_string())?;
        let mut input = std::io::stdin().lock();
        let spec = loop {
            let (kind, _, payload) = read_frame(&mut input).map_err(|e| format!("the bridge closed: {e}"))?;
            if kind != JSON {
                continue;
            }
            let v: Value = serde_json::from_slice(&payload).map_err(|e| e.to_string())?;
            if v.get("id").and_then(Value::as_u64) == Some(0) {
                if let Some(err) = v.get("error") {
                    return Err(err.to_string());
                }
                break serde_json::from_value(v["result"]["spec"].clone()).map_err(|e| e.to_string())?;
            }
        };
        drop(input);
        std::thread::spawn(move || {
            let mut input = std::io::stdin().lock();
            while let Ok((kind, stream, payload)) = read_frame(&mut input) {
                match kind {
                    JSON => {
                        let Ok(v) = serde_json::from_slice::<Value>(&payload) else { continue };
                        match v.get("event").and_then(Value::as_str) {
                            Some("listing") => {
                                let boxes = serde_json::from_value(v["listing"].clone()).unwrap_or_default();
                                inbox.push(Event::Listing(boxes));
                            }
                            Some("stream") => inbox.push(Event::StreamState(
                                v["stream"].as_u64().unwrap_or(0) as u32,
                                v["state"].as_str().unwrap_or("").into(),
                            )),
                            _ => {
                                let id = v["id"].as_u64().unwrap_or(0);
                                let r = match v.get("error") {
                                    Some(e) => Err(e.as_str().unwrap_or("error").to_string()),
                                    None => Ok(v.get("result").cloned().unwrap_or(Value::Null)),
                                };
                                inbox.push(Event::Reply(id, r));
                            }
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
        Ok((Bridge { out, next: 1 }, spec))
    }

    fn frame(&self, kind: u8, stream: u32, payload: &[u8]) {
        let _ = write_frame(&mut *self.out.lock().unwrap(), kind, stream, payload);
    }
}

impl Backend for Bridge {
    fn request(&mut self, op: &str, mut args: Value) -> u64 {
        let id = self.next;
        self.next += 1;
        args["id"] = json!(id);
        args["op"] = json!(op);
        self.frame(JSON, 0, args.to_string().as_bytes());
        id
    }
    fn input(&mut self, stream: u32, bytes: &[u8]) { self.frame(INPUT, stream, bytes); }
    fn resize(&mut self, stream: u32, cols: u16, rows: u16) {
        self.frame(RESIZE, stream, json!({"cols": cols, "rows": rows}).to_string().as_bytes());
    }
}

// ---------------------------------------------------------------------------------------------
// The mock: the same requests, answered here.

pub struct Mock {
    inbox: Inbox,
    next: u64,
    boxes: Vec<Sandbox>,
    extra: HashMap<String, Vec<Exec>>,
    ptys: HashMap<u32, Pty>,
}

fn mock_box(v: Value) -> Sandbox { serde_json::from_value(v).unwrap() }

impl Mock {
    pub fn new(inbox: Inbox) -> Mock {
        let boxes = vec![
            mock_box(json!({"id": "sbx_k2m9q4tz7wbf1xay", "name": "rune console port", "state": "running", "agent": "working",
                "harness": "claude", "source": "discobox", "tags": ["wip", "tui"], "branch": "rune-console", "commit": "00541f9",
                "dirty": true, "added": 412, "deleted": 37, "cpu": 12, "memoryBytes": 2_254_857_830u64})),
            mock_box(json!({"id": "sbx_p8c3n0vd6rgs2hkq", "name": "reaper race", "state": "running", "agent": "blocked",
                "harness": "codex", "source": "discobox", "tags": ["ENG-12"], "branch": "fix/reaper-race", "commit": "8be21c0",
                "added": 58, "deleted": 12, "cpu": 3, "memoryBytes": 1_503_238_553u64})),
            mock_box(json!({"id": "sbx_r5t1y7u3i9o0pzx2", "name": "attach docs", "state": "running", "agent": "done",
                "harness": "claude", "source": "discobox", "branch": "docs/attach", "commit": "41d9e2a", "parent": "sbx_p8c3n0vd6rgs2hkq"})),
            mock_box(json!({"id": "sbx_v6svx9a5hndxswfp", "name": "lucid_lalande", "state": "starting", "harness": "shell",
                "source": "", "provisioning": "provisioning browser, build, bun…", "cpu": 2})),
            mock_box(json!({"id": "sbx_a1b2c3d4e5f6g7h8", "name": "quiet_hopper", "state": "stopped", "agent": "idle",
                "harness": "opencode", "source": ""})),
        ];
        inbox.push(Event::Listing(boxes.clone()));
        Mock { inbox, next: 1, boxes, extra: HashMap::new(), ptys: HashMap::new() }
    }

    fn spawn(&mut self, sandbox: &str, exec: &Exec, stream: u32, cols: u16, rows: u16) -> Result<(), String> {
        let sbx = self.boxes.iter().find(|b| b.id == sandbox).cloned().unwrap_or_default();
        let pty = Pty::spawn(&sbx, exec, stream, cols, rows, self.inbox.clone())?;
        self.ptys.insert(stream, pty);
        Ok(())
    }
}

impl Backend for Mock {
    fn request(&mut self, op: &str, args: Value) -> u64 {
        let id = self.next;
        self.next += 1;
        let sandbox = args["sandbox"].as_str().unwrap_or("").to_string();
        let stream = args["stream"].as_u64().unwrap_or(0) as u32;
        let (cols, rows) = (args["cols"].as_u64().unwrap_or(80) as u16, args["rows"].as_u64().unwrap_or(24) as u16);
        let result: Result<Value, String> = match op {
            "list" => Ok(serde_json::to_value(self.boxes.iter().map(|b| b.id.clone()).collect::<Vec<_>>()).unwrap()),
            "execs" => {
                let mut v = vec![json!({"id": "primary", "kind": "primary"})];
                for e in self.extra.get(&sandbox).into_iter().flatten() {
                    v.push(json!({"id": e.id, "kind": e.kind, "label": e.label}));
                }
                Ok(Value::Array(v))
            }
            "open" => {
                let exec = Exec { id: args["exec"].as_str().unwrap_or("").into(), kind: if args["exec"] == "primary" { "primary".into() } else { "shell".into() }, label: String::new() };
                self.spawn(&sandbox, &exec, stream, cols, rows).map(|_| json!({"stream": stream}))
            }
            "newShell" | "newTerminal" => {
                let shell = op == "newShell";
                let exec = Exec {
                    id: format!("exec_{id}"),
                    kind: if shell { "shell" } else { "terminal" }.into(),
                    label: if shell { "bash".into() } else { self.boxes.iter().find(|b| b.id == sandbox).map(|b| b.harness.clone()).unwrap_or_default() },
                };
                self.extra.entry(sandbox.clone()).or_default().push(exec.clone());
                self.spawn(&sandbox, &exec, stream, cols, rows).map(|_| json!({"id": exec.id, "kind": exec.kind, "label": exec.label}))
            }
            "endExec" => {
                if let Some(mut p) = self.ptys.remove(&stream) {
                    let _ = p.child.kill();
                }
                let exec = args["exec"].as_str().unwrap_or("");
                if let Some(v) = self.extra.get_mut(&sandbox) {
                    v.retain(|e| e.id != exec);
                }
                Ok(Value::Null)
            }
            "actions" => {
                let running = self.boxes.iter().any(|b| b.id == sandbox && b.running());
                Ok(json!([
                    {"command": "box.attach", "key": "a", "label": "attach", "detail": "join the harness terminal", "enabled": running, "why": "the box has no container; repair rebuilds it"},
                    {"command": "box.shell", "key": "s", "label": "shell", "detail": "open a shell in the box", "enabled": running},
                    {"command": "box.rename", "key": "e", "label": "rename", "detail": "type a new name for the box", "enabled": true},
                    {"command": "box.upgrade", "key": "u", "label": "upgrade", "detail": "re-pin to the current harness image", "enabled": false, "why": "already on the current image"},
                    {"command": "box.stop", "key": "t", "label": "stop", "detail": "power the box off, keeping its disk", "enabled": running, "why": "not running"},
                    {"command": "box.start", "key": "T", "label": "start", "detail": "power a stopped box back on", "enabled": !running, "why": "already on, or archived"},
                    {"command": "box.archive", "key": "x", "label": "archive", "detail": "put it away, disk and all, reversibly", "enabled": true},
                    {"command": "box.purge", "key": "P", "label": "purge", "detail": "destroy it and its data, for good", "enabled": false, "why": "archive it first — purge only takes archived boxes"},
                ]))
            }
            "do" => {
                let verb = args["verb"].as_str().unwrap_or("");
                if let Some(b) = self.boxes.iter_mut().find(|b| b.id == sandbox) {
                    match verb {
                        "stop" => b.state = "stopped".into(),
                        "start" => b.state = "running".into(),
                        "archive" => b.state = "archived".into(),
                        _ => {}
                    }
                }
                self.inbox.push(Event::Listing(self.boxes.clone()));
                Ok(json!({"note": ""}))
            }
            _ => Err(format!("unknown op {op}")),
        };
        self.inbox.push(Event::Reply(id, result));
        id
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
