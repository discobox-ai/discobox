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
    Answer, Box as Sandbox, ChoiceGroup, Chip, Close, Command, Commands, Choice, Composer, Draft, Entry, Field, FieldError, FieldErrors, Filter, FilterChoice,
    Form, GitTone, Header, Link, Listing, Message, Open, Palette, Provisioning, PaletteCommit, PaletteQuery, PaletteRow, PortGroup, Progress, Question,
    Remark, Result as CommandResult, Row, Service, ServiceAction, ServiceVerb, Sessions, Spec, SpecTarget, State, Status, Stream, StreamState,
    Submit, Subscribe, Tab, TabEnd, TabKind, Topic, Type, Unsubscribe, Edit, Saved, Harness, HarnessAction, HarnessOffer, HarnessFile,
    HarnessChanges, ChangedFile, ChangeOffer, HarnessChange, CardRow, CardSpan, CardTone,
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
    /// An edit finished with, or given up.
    Saved(Saved),
    /// The prompt in the composer, kept for the next window.
    Draft(String),
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
            Out::Saved(v) => Message { r#type: Type::Saved, saved: Some(v), ..Default::default() },
            Out::Draft(prompt) => Message { r#type: Type::Draft, draft: Some(Draft { prompt }), ..Default::default() },
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
    /// What a create from the composer would be made with, as the line under it says it.
    Composer(u32, Result<Composer, String>),
    /// What a box that is not usable yet is being made to do.
    Provisioning(u32, Result<Provisioning, String>),
    Result(CommandResult),
    Stream(Stream),
    Question(Question),
    Form(Form),
    /// Why what was submitted to a form was refused; the form stays up.
    FieldErrors(FieldErrors),
    Progress(Progress),
    /// A file a command needs edited in the person's editor.
    Edit(Edit),
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
                Topic::Composer => Some(Event::Composer(s.sub, match s.composer {
                    Some(v) if s.error.is_empty() => Ok(v),
                    _ => Err(s.error),
                })),
                Topic::Provisioning => Some(Event::Provisioning(s.sub, match s.provisioning {
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
        Type::Edit => m.edit.map(Event::Edit),
        _ => None,
    }
}

pub struct Bridge {
    client: Client,
}

impl Bridge {
    /// Says hello and waits for the welcome; then what arrives goes to the inbox, until the CLI
    /// is gone, when the app is too. Beside the spec, the prompt the last window left.
    pub fn connect(inbox: Inbox) -> Result<(Bridge, Spec, String), String> {
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
        let draft = welcome.session.as_ref().map(|s| s.draft.clone()).unwrap_or_default();
        Ok((Bridge { client }, welcome.spec.ok_or("the CLI's welcome carried no spec")?, draft))
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
    /// Each box's declared services, by its key, and the sessions subscriptions following each box:
    /// a service verb sends them its sessions again.
    services: HashMap<String, Vec<MockService>>,
    following: HashMap<u32, String>,
    /// The project's harnesses, and each one's files' contents by its ID and path.
    harnesses: Vec<Harness>,
    files: HashMap<(String, String), String>,
    /// Each box's harness changes, by its key: the first box has some to save.
    changes: HashMap<String, HarnessChanges>,
    /// The run options a create is made with, and the composer topic that says them.
    run: MockRun,
    composer_sub: u32,
    /// What the app sent that only the CLI would act on, for tests to read.
    record: Arc<Mutex<MockRecord>>,
}

/// A mock harness, its actions as the core offers them (`console.Harness.Actions`).
fn mock_harness(id: &str, name: &str, default: bool, built_in: bool, variant: bool, files: &[(&str, &str, &[&str])]) -> Harness {
    let files: Vec<HarnessFile> = files.iter().map(|(path, content, notes)| HarnessFile {
        path: path.to_string(), size: content.len() as i64, notes: notes.iter().map(|n| n.to_string()).collect(),
    }).collect();
    let mut h = Harness {
        id: id.into(), name: name.into(), default, built_in, bundle: format!("ghcr.io/discobox-ai/harness-{id}:1"), age: "3d ago".into(),
        secrets: if built_in { 1 } else { 0 }, files, actions: vec![],
    };
    h.actions = mock_offers(&h, variant);
    h
}

fn mock_offers(h: &Harness, variant: bool) -> Vec<HarnessOffer> {
    let offer = |action, label: &str, detail: &str, enabled: bool, why: &str| HarnessOffer {
        action, label: label.into(), detail: detail.into(), enabled, why: if enabled { String::new() } else { why.into() },
    };
    vec![
        offer(HarnessAction::Variant, "create variant", "follow this bundle with an independent account", !variant, "a variant cannot be a parent; select its standalone parent instead"),
        offer(HarnessAction::Default, "default", "run it when a discobox says no harness", !h.default, "already the default"),
        offer(HarnessAction::Config, "config", "everything the harness is set to", true, ""),
        offer(HarnessAction::Files, "files", "edit one of its files in your editor", !h.files.is_empty(), "it carries no files"),
        offer(HarnessAction::Reset, "reset", "forget saved settings and saved logins, back to the bundle's defaults", true, ""),
    ]
}

/// The mock's harnesses, the first the default, with files saved from a box and one of yours.
fn mock_harnesses() -> (Vec<Harness>, HashMap<(String, String), String>) {
    let claude_json = "{\n  \"theme\": \"dark\",\n  \"hasCompletedOnboarding\": true\n}\n";
    let settings = "{\n  \"permissions\": { \"allow\": [\"Bash(go test:*)\"] }\n}\n";
    let harnesses = vec![
        mock_harness("hc_claude", "Claude", true, true, false, &[
            (".claude.json", claude_json, &["saved from a discobox", "over the image's"]),
            (".claude/settings.json", settings, &["yours"]),
        ]),
        mock_harness("hc_codex", "Codex", false, true, false, &[]),
        mock_harness("hc_opencode", "OpenCode", false, false, false, &[]),
    ];
    let files = HashMap::from([
        (("hc_claude".to_string(), ".claude.json".to_string()), claude_json.to_string()),
        (("hc_claude".to_string(), ".claude/settings.json".to_string()), settings.to_string()),
    ]);
    (harnesses, files)
}

/// What a box captured of its harness's settings and login, as the core reviews it.
fn mock_changes() -> HarnessChanges {
    let offer = |change, label: &str| ChangeOffer { change, label: label.into(), enabled: true, why: String::new() };
    HarnessChanges {
        candidate: "hcc_7f3a".into(), summary: "State: pending · revision 2".into(), nothing: String::new(),
        saving: "Save updates shared harness defaults and credentials. Discard leaves local files unchanged.".into(),
        files: vec![ChangedFile {
            path: ".claude/settings.json".into(), label: "update .claude/settings.json — shadowed by your override, which boxes keep getting".into(),
            diff: "@@ -1,3 +1,4 @@\n {\n-  \"permissions\": { \"allow\": [\"Bash(go test:*)\"] }\n+  \"permissions\": { \"allow\": [\"Bash(go test:*)\", \"Bash(git status)\"] },\n+  \"model\": \"opus\"\n }\n".into(),
        }],
        credentials: vec!["Claude login (claude.ai)".into()], diagnostics: vec![],
        offers: vec![offer(HarnessChange::Save, "Save reviewed changes"), offer(HarnessChange::Discard, "Discard candidate")],
    }
}

/// The mock's run options: the rows the core's forms ask, roughly.
#[derive(Clone)]
struct MockRun {
    harness: String,
    dirty: String,
    detach: bool,
    env: Vec<String>,
    skills: Vec<String>,
    include: Vec<String>,
    /// Where the last create was cut from: where the next opens.
    source: String,
}

impl Default for MockRun {
    fn default() -> MockRun {
        MockRun { harness: "claude".into(), dirty: "auto".into(), detach: false, env: vec![], skills: vec![], include: vec![], source: MOCK_DIR.into() }
    }
}

/// What the mock was sent that a test reads back: every draft, and every create's prompt.
#[derive(Default, Debug)]
pub struct MockRecord {
    pub drafts: Vec<String>,
    pub created: Vec<String>,
}

/// Where the mock's window runs, which a create is cut from unless told otherwise.
const MOCK_DIR: &str = "/src/discobox";
/// The source field's choices that are not a directory, as the core keys them.
const SOURCE_NONE: &str = "\u{0}no-source";
const SOURCE_ENTER: &str = "\u{0}enter";

/// A mock box's declared service: what it is doing, what its runs have written, and how many
/// times it has started — its run, which moves on a start.
#[derive(Clone, Debug)]
pub struct MockService {
    pub id: String,
    pub status: String,
    pub logs: String,
    runs: u32,
}

impl MockService {
    pub fn new(id: &str, status: &str, logs: &str) -> MockService {
        MockService { id: id.into(), status: status.into(), logs: logs.into(), runs: 1 }
    }

    fn live(&self) -> bool { matches!(self.status.as_str(), "running" | "starting") }

    /// Whether it has a tab, as the core's `Service.PaneWorthy` says: running, or ended on its own.
    fn tabbed(&self) -> bool { self.live() || matches!(self.status.as_str(), "exited" | "failed") }

    /// Its tab, as the core's `ServiceTab` makes it.
    fn tab(&self) -> Tab {
        let mark = match self.status.as_str() {
            "failed" => " ✗",
            "exited" => " ·",
            _ => "",
        };
        Tab {
            exec: format!("service:{}", self.id), kind: TabKind::Service, title: format!("{}{mark}", self.id), live: self.live(), argv: vec![],
            service: self.id.clone(), run: format!("{}|{}", self.runs, self.status), status: if self.live() { String::new() } else { self.status.clone() },
            tool: String::new(),
        }
    }

    /// As the services menu lists it, its verbs as the core offers them.
    fn wire(&self) -> Service {
        let action = |verb, label: &str, detail: &str| ServiceAction { verb, label: label.into(), detail: detail.into(), enabled: true, why: String::new() };
        Service {
            id: self.id.clone(), name: self.id.clone(), status: self.status.clone(), detail: self.status.clone(),
            actions: vec![
                action(ServiceVerb::Start, "start", "run it, or leave it running"),
                action(ServiceVerb::Stop, "stop", "end its run and keep what it printed"),
                action(ServiceVerb::Restart, "restart", "stop it if it is running, then start it"),
            ],
        }
    }

    /// What its tab draws: what it has written, and for one not running the card over it, as the
    /// core's `OpenService` does. What it wrote is sent as it wrote it, bare line feeds and all, as
    /// the bridge sends a service's output: the pane is what draws them as new lines.
    fn screen(&self) -> String {
        if self.live() {
            return self.logs.clone();
        }
        format!("{}\r\n{}\r\n\r\n{}\r\n\r\n── last output ──\r\n\r\n{}", self.id, "─".repeat(self.id.chars().count()), self.status, self.logs)
    }
}

/// What a mock form or question is for.
#[derive(Clone)]
enum Ask {
    Rename(String),
    Settings(String),
    Purge(Vec<String>),
    /// A harness's reset, by its ID, and whether it carries a person's own files.
    Reset(String, bool),
    /// A variant of a harness, by its ID.
    Variant(String),
    /// A harness's file, by its ID and path, written out to a file of this machine to edit.
    Edit(String, String, std::path::PathBuf),
    /// The create card, for the prompt the composer held.
    Create(String),
    Options,
}

fn mock_exec(exec: &str, kind: TabKind, title: &str) -> Tab {
    Tab { exec: exec.into(), kind, title: title.into(), live: true, argv: vec![], ..Default::default() }
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
    /// The mock, its first box declaring a dev server that is running, a worker stopped and a
    /// migration that ran.
    pub fn new(inbox: Inbox) -> Mock {
        Mock::with_services(inbox, vec![
            MockService::new("web", "running", "  VITE v6.0.3  ready in 412 ms\n\n  ➜  Local:   http://localhost:5173/\n"),
            MockService::new("worker", "stopped", ""),
            MockService::new("migrate", "exited", "applying 0007_sessions.sql\nmigrated 3 tables\n"),
        ])
    }

    /// The mock, its first box declaring `services`.
    pub fn with_services(inbox: Inbox, services: Vec<MockService>) -> Mock {
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
        let services = HashMap::from([(boxes[0].key.clone(), services)]);
        let (harnesses, files) = mock_harnesses();
        let changes = HashMap::from([(boxes[0].key.clone(), mock_changes())]);
        Mock {
            inbox, boxes, parents, list_sub: 0, filter, extra: HashMap::new(), ptys: HashMap::new(), made: 0, asks: HashMap::new(), next_ask: 1,
            services, following: HashMap::new(), harnesses, files, changes, run: MockRun::default(), composer_sub: 0, record: Arc::default(),
        }
    }

    /// What the app sent the mock that only the CLI would act on.
    #[cfg(test)]
    pub fn record(&self) -> Arc<Mutex<MockRecord>> { self.record.clone() }

    /// The line under the composer, as the core's `RunOptions.Summary` says it: the harness, and
    /// every other row somebody chose.
    fn chips(&self) -> Composer {
        let chosen = |text: String| Chip { text, chosen: true };
        let mut chips = vec![Chip { text: self.run.harness.clone(), chosen: self.run.harness != "claude" }];
        match self.run.dirty.as_str() {
            "include" => chips.push(chosen("+dirty".into())),
            "exclude" => chips.push(chosen("clean".into())),
            _ => {}
        }
        if self.run.detach {
            chips.push(chosen("detached".into()));
        }
        if !self.run.env.is_empty() {
            chips.push(chosen(format!("{} env", self.run.env.len())));
        }
        if self.run.source != MOCK_DIR {
            chips.push(chosen(if self.run.source == SOURCE_NONE { "no source".into() } else { self.run.source.clone() }));
        }
        Composer { chips }
    }

    fn send_chips(&self) {
        if self.composer_sub != 0 {
            self.inbox.push(Event::Composer(self.composer_sub, Ok(self.chips())));
        }
    }

    /// The create card or the run options as the core's forms, roughly, for the answers so far:
    /// a repeated row grows a field once its last is filled, and the note is the command.
    fn run_form(&self, id: u64, c: &Command, card: bool, answers: &HashMap<String, String>) -> Form {
        let get = |k: &str, opening: &str| answers.get(k).cloned().unwrap_or_else(|| opening.to_string());
        let choice = |key: &str, label: &str| Choice { key: key.into(), label: label.into(), ..Default::default() };
        let picker = |key: &str, label: &str, value: String, choices: Vec<Choice>| Field {
            key: key.into(), label: label.into(), section: "Options".into(), value, choices, applies: true, ..Default::default()
        };
        let mut fields = vec![];
        let source = get("source", &self.run.source);
        if card {
            fields.push(Field {
                section: "Source".into(), hint: "SOURCE · where the discobox is cut from".into(),
                ..picker("source", "cut from", source.clone(), vec![
                    choice(MOCK_DIR, "~/src/discobox @ main"), choice(SOURCE_NONE, "no source"), choice(SOURCE_ENTER, "another…"),
                ])
            });
            fields.push(Field {
                key: "source:typed".into(), label: "source".into(), section: "Source".into(), value: get("source:typed", ""),
                placeholder: "~/src/discobox".into(), applies: source == SOURCE_ENTER, why: "choose another… to type one".into(), ..Default::default()
            });
        }
        let auto = if card { "auto (claude)" } else { "claude" };
        fields.push(picker("harness", "harness", get("harness", &self.run.harness), vec![choice("claude", auto), choice("codex", "codex"), choice("opencode", "opencode")]));
        if !card {
            fields.push(picker("dirty", "uncommitted changes", get("dirty", &self.run.dirty), vec![choice("auto", "auto"), choice("include", "include"), choice("exclude", "exclude")]));
            fields.push(picker("detach", "detach", get("detach", if self.run.detach { "on" } else { "off" }), vec![choice("off", "no"), choice("on", "yes")]));
        }
        let lists: Vec<(&str, &str, &Vec<String>)> = if card {
            vec![("skills", "Skills", &self.run.skills), ("include", "Additional sources", &self.run.include)]
        } else {
            vec![("env", "Env", &self.run.env)]
        };
        for (key, label, items) in lists {
            let mut n = items.len() + 1;
            for i in 0.. {
                let Some(v) = answers.get(&format!("{key}:{i}")) else { break };
                if !v.trim().is_empty() {
                    n = n.max(i + 2);
                }
            }
            for i in 0..n {
                fields.push(Field {
                    key: format!("{key}:{i}"), label: if i == 0 { label.to_lowercase() } else { "and".into() }, section: label.into(),
                    value: get(&format!("{key}:{i}"), items.get(i).map(String::as_str).unwrap_or("")), applies: true, ..Default::default()
                });
            }
        }
        let prompt = match self.asks.get(&id) {
            Some((_, Ask::Create(p))) => p.clone(),
            _ => c.args.prompt.clone(),
        };
        let mut note = format!("discobox run {}", if source == MOCK_DIR { "." } else if source == SOURCE_NONE { "--no-source" } else { source.as_str() });
        if !prompt.is_empty() {
            note += &format!(" -p '{prompt}'");
        }
        let harness = get("harness", &self.run.harness);
        if harness != "claude" {
            note += &format!(" --harness {harness}");
        }
        let (title, action) = if card { ("New discobox", "Create") } else { ("Run options", "") };
        Form { id, command: c.id, title: title.into(), fields, note, action: action.into() }
    }

    /// A create answered: what it carries, then the box it made — taken the moment it exists,
    /// and entered unless it was to be left detached.
    fn create(&mut self, c: &Command, prompt: String, answers: &HashMap<String, String>) -> CommandResult {
        let get = |k: &str| answers.get(k).cloned().unwrap_or_default();
        if !get("harness").is_empty() {
            self.run.harness = get("harness");
        }
        let source = match get("source").as_str() {
            "" => self.run.source.clone(),
            SOURCE_ENTER => get("source:typed").trim().to_string(),
            s => s.to_string(),
        };
        self.run.source = source.clone();
        self.made += 1;
        let key = format!("sbx_new{}", self.made);
        let name = if prompt.trim().is_empty() { format!("new box {}", self.made) } else { prompt.trim().to_string() };
        let sandbox = Sandbox {
            provisioning: "pulling the image".into(),
            ..mock_box(&key, &name, State::Starting, "", &self.run.harness, if source == SOURCE_NONE { "" } else { "discobox" })
        };
        self.inbox.push(Event::Progress(Progress { command: c.id, text: "pushing the source".into(), ..Default::default() }));
        if self.run.dirty == "exclude" {
            self.inbox.push(Event::Progress(Progress { command: c.id, text: "uncommitted changes left out".into(), warning: true, ..Default::default() }));
        }
        self.inbox.push(Event::Progress(Progress { command: c.id, accepted: Some(sandbox.clone()), ..Default::default() }));
        self.boxes.insert(0, sandbox);
        self.record.lock().unwrap().created.push(prompt);
        self.relist();
        self.send_chips();
        if self.run.detach {
            return CommandResult { id: c.id, status: Status::Done, r#box: key, note: format!("created {name}, detached"), ..Default::default() };
        }
        CommandResult { id: c.id, status: Status::Enter, r#box: key, ..Default::default() }
    }

    /// A box's sessions as the core lists them: its services' tabs, the primary, and the sessions
    /// made in it; only the primary for a box not running.
    fn sessions(&self, sandbox: &str) -> Sessions {
        let running = self.sandbox(sandbox).is_some_and(Sandbox::running);
        let mut sessions = vec![];
        if running {
            sessions.extend(self.services.get(sandbox).into_iter().flatten().filter(|v| v.tabbed()).map(MockService::tab));
        }
        sessions.push(mock_exec("primary", TabKind::Primary, ""));
        sessions.extend(self.extra.get(sandbox).into_iter().flatten().cloned());
        Sessions { running, sessions }
    }

    /// The services command: the box's services for its menu, or a verb run on one of them, after
    /// which the box's sessions say what it is doing.
    fn services(&mut self, c: &Command, sandbox: String) -> CommandResult {
        let Some(verb) = c.args.verb.filter(|_| !c.args.service.is_empty()) else {
            let services: Vec<Service> = self.services.get(&sandbox).into_iter().flatten().map(MockService::wire).collect();
            let note = if services.is_empty() { "this discobox declares no services — add one under .discobox/services".into() } else { String::new() };
            return CommandResult { status: Status::Enter, r#box: sandbox, services, note, ..Default::default() };
        };
        let Some(v) = self.services.get_mut(&sandbox).and_then(|v| v.iter_mut().find(|v| v.id == c.args.service)) else {
            return CommandResult { status: Status::Failed, why: format!("this discobox declares no service {:?}", c.args.service), ..Default::default() };
        };
        let done = match verb {
            ServiceVerb::Start if v.live() => "started",
            ServiceVerb::Start | ServiceVerb::Restart => {
                v.runs += 1;
                v.status = "running".into();
                if verb == ServiceVerb::Start { "started" } else { "restarted" }
            }
            ServiceVerb::Stop => {
                v.status = "stopped".into();
                "stopped"
            }
        };
        let note = format!("{done} {}", v.id);
        let snapshot = self.sessions(&sandbox);
        for (&sub, _) in self.following.iter().filter(|(_, b)| **b == sandbox) {
            self.inbox.push(Event::Sessions(sub, Ok(snapshot.clone())));
        }
        CommandResult { status: Status::Done, r#box: sandbox, note, ..Default::default() }
    }

    /// A harnesses result carrying the harnesses as they stand, about one of them.
    fn harnessed(&self, status: Status, about: &str, note: String) -> CommandResult {
        CommandResult { status, harnesses: self.harnesses.clone(), harness: about.into(), note, ..Default::default() }
    }

    /// Recomputes what each harness offers, after the default moved or a variant was made.
    fn reoffer(&mut self) {
        for h in self.harnesses.iter_mut() {
            let variant = h.name.contains("(variant of");
            h.actions = mock_offers(h, variant);
        }
    }

    /// The harnesses command: the page's harnesses, or an action done to one of them, as the core
    /// does it. None for one that waits on the app: a reset's question, a variant's form, an edit.
    fn harness(&mut self, c: &Command) -> Option<CommandResult> {
        let fail = |why: String| Some(CommandResult { status: Status::Failed, why, ..Default::default() });
        let Some(action) = c.args.action.filter(|_| !c.args.harness.is_empty()) else {
            return Some(self.harnessed(Status::Enter, "", String::new()));
        };
        let Some(h) = self.harnesses.iter().find(|h| h.id == c.args.harness).cloned() else {
            return fail(format!("no harness {} is listed", c.args.harness));
        };
        let offer = h.actions.iter().find(|a| a.action == action).cloned().unwrap_or_default();
        if !offer.enabled {
            return fail(format!("cannot {} {}: {}", offer.label, h.name, offer.why));
        }
        let id = self.next_ask;
        match action {
            HarnessAction::Config => {
                let plain = |t: &str| CardSpan { text: t.into(), tone: CardTone::Plain };
                let quiet = |t: &str| CardSpan { text: t.into(), tone: CardTone::Quiet };
                let mut card = vec![];
                if h.default {
                    card.push(CardRow { label: "Role".into(), spans: vec![CardSpan { text: "★ default".into(), tone: CardTone::Standout }], ..Default::default() });
                }
                card.push(CardRow { label: "ID".into(), spans: vec![plain(&h.id)], ..Default::default() });
                card.push(CardRow { label: "Bundle".into(), spans: vec![plain(&h.bundle)], ..Default::default() });
                card.push(CardRow { label: "Run".into(), spans: vec![CardSpan { text: h.name.to_lowercase(), tone: CardTone::Command }], ..Default::default() });
                for (i, f) in h.files.iter().enumerate() {
                    let content = self.files.get(&(h.id.clone(), f.path.clone())).cloned().unwrap_or_default();
                    card.push(CardRow {
                        label: if i == 0 { "Files".into() } else { String::new() },
                        spans: vec![plain(&f.path), quiet(&format!("  ({})", f.notes.join(" · ")))],
                        lines: content.lines().map(String::from).collect(), more: 0,
                    });
                }
                Some(CommandResult { status: Status::Done, harness: h.id.clone(), card, ..Default::default() })
            }
            HarnessAction::Default => {
                for x in self.harnesses.iter_mut() {
                    x.default = x.id == h.id;
                }
                self.reoffer();
                Some(self.harnessed(Status::Done, &h.id, format!("{} is now the default", h.name)))
            }
            HarnessAction::Reset => {
                // The core's question: a yes or no, or, for a harness carrying a person's own
                // files, a pick of one keeping them first, its keys the verbs.
                let mut body = format!("Reset {} to its bundle's defaults? The settings and logins saved to it from discoboxes are deleted.", h.name);
                let overrides = h.files.iter().any(|f| f.notes.iter().any(|n| n == "yours"));
                let choices = if overrides {
                    body += "\n\nIt has files you added or overrode yourself.";
                    let choice = |key: &str, label: &str, hint: &str| Choice { key: key.into(), label: label.into(), hint: hint.into() };
                    vec![
                        choice("reset", "Reset, keep your files", "the files you added or overrode stay"),
                        choice("reset with your files", "Reset your files too", "nothing but the bundle's files is left"),
                    ]
                } else {
                    vec![]
                };
                self.next_ask += 1;
                self.asks.insert(id, (c.clone(), Ask::Reset(h.id.clone(), overrides)));
                self.inbox.push(Event::Question(Question { id, command: c.id, title: "Reset".into(), body, choices, default_no: false, ..Default::default() }));
                None
            }
            HarnessAction::Variant => {
                let field = |key: &str, label: &str, placeholder: String| Field {
                    key: key.into(), label: label.into(), placeholder, required: true, applies: true, ..Default::default()
                };
                let fields = vec![field("name", "Name", format!("{} (work)", h.name)), field("slug", "Slug", format!("{}-work", h.name.to_lowercase()))];
                self.next_ask += 1;
                self.asks.insert(id, (c.clone(), Ask::Variant(h.id.clone())));
                self.inbox.push(Event::Form(Form { id, command: c.id, title: format!("Create variant of {}", h.name), fields, ..Default::default() }));
                None
            }
            HarnessAction::Files => {
                let Some(content) = self.files.get(&(h.id.clone(), c.args.path.clone())).cloned() else {
                    return fail(format!("{} carries no file {:?}", h.name, c.args.path));
                };
                let ext = std::path::Path::new(&c.args.path).extension().map(|e| format!(".{}", e.to_string_lossy())).unwrap_or_default();
                let file = std::env::temp_dir().join(format!("discobox-mock-edit-{}-{id}{ext}", std::process::id()));
                if let Err(e) = std::fs::write(&file, &content) {
                    return fail(format!("edit {}: {e}", c.args.path));
                }
                self.next_ask += 1;
                self.asks.insert(id, (c.clone(), Ask::Edit(h.id.clone(), c.args.path.clone(), file.clone())));
                let title = format!("Editing {} — {}", c.args.path, h.name);
                self.inbox.push(Event::Edit(Edit { id, command: c.id, path: file.to_string_lossy().into_owned(), title }));
                None
            }
        }
    }

    /// An edit finished with, or given up: what was saved is read back, as the core does.
    fn saved(&mut self, v: Saved) {
        let Some((c, Ask::Edit(harness, path, file))) = self.asks.remove(&v.edit) else { return };
        let result = if v.canceled {
            CommandResult { status: Status::Canceled, ..Default::default() }
        } else {
            let after = std::fs::read_to_string(&file).unwrap_or_default();
            let key = (harness.clone(), path.clone());
            let changed = self.files.get(&key) != Some(&after);
            if changed {
                self.files.insert(key, after);
            }
            self.harnessed(Status::Done, &harness, if changed { format!("updated {path}") } else { format!("{path} unchanged") })
        };
        let _ = std::fs::remove_file(&file);
        self.inbox.push(Event::Result(CommandResult { id: c.id, ..result }));
    }

    /// The review config command: a box's harness changes, or the candidate saved or discarded.
    fn review(&mut self, c: &Command, sandbox: String) -> CommandResult {
        let Some(change) = c.args.change else {
            let changes = self.changes.get(&sandbox).cloned().unwrap_or_else(|| HarnessChanges {
                summary: "State: none · revision 0".into(), nothing: "No saveable settings or credentials.".into(), saving: mock_changes().saving,
                offers: mock_changes().offers.into_iter().map(|o| ChangeOffer { enabled: false, why: "no saveable candidate".into(), ..o }).collect(),
                ..Default::default()
            });
            return CommandResult { status: Status::Enter, r#box: sandbox, changes: Some(changes), ..Default::default() };
        };
        if self.changes.get(&sandbox).is_none_or(|x| x.candidate != c.args.candidate) {
            return CommandResult { status: Status::Failed, why: "harness changes: that candidate is no longer pending".into(), ..Default::default() };
        }
        self.changes.remove(&sandbox);
        let state = if change == HarnessChange::Save { "saved" } else { "discarded" };
        CommandResult { status: Status::Done, r#box: sandbox, note: format!("harness changes: {state}"), ..Default::default() }
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
            ports, tags: b.tags.iter().map(|t| format!("#{t}")).collect(),
            // The mock's poll: its local shells report nothing of their own.
            agent: b.agent.clone(), agent_msg: b.agent_msg.clone(), ..Default::default()
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
            "terminal" | "end" | "maximize" | "detach" | "mouse" if !on_pane => fail("requires a workspace pane"),
            "repaint" if c.args.stream == 0 => fail("no pane to repaint"),
            // A local shell has no far end to ask: its screen is already the one drawn.
            "repaint" => CommandResult { status: Status::Done, ..Default::default() },
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
            "services" if !running => fail("the box has no container; repair rebuilds it"),
            "services" => self.services(&c, target),
            "review config" if !running => fail("cannot review config: takes one running box"),
            "review config" => self.review(&c, target),
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
                (Ask::Rename(target), Event::Form(Form { id, command: c.id, title: "Rename".into(), fields: vec![name], ..Default::default() }))
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
                (Ask::Settings(target), Event::Form(Form { id, command: c.id, title, fields: vec![name, harness, restrict, allow, api], ..Default::default() }))
            }
            // The core refuses a purge none of its boxes can take before it asks anything.
            "purge" if !c.targets.iter().any(|t| self.sandbox(t).is_some_and(|b| b.state == State::Archived)) => return None,
            "purge" => {
                let name = match c.targets.as_slice() {
                    [one] => self.sandbox(one).map(|b| b.name.clone()).unwrap_or_default(),
                    many => format!("{} boxes selected", many.len()),
                };
                let body = format!("Purge {name}? The disk and everything on it goes, and unarchive cannot bring it back.");
                (Ask::Purge(c.targets.clone()), Event::Question(Question { id, command: c.id, title: "Purge".into(), body, choices: vec![], default_no: true, ..Default::default() }))
            }
            // A harness's own way out is the window's, as the core's `LeavesWindow` says: no card.
            "new" if matches!(c.args.prompt.trim(), "/exit" | "/quit") => return None,
            "new" => (Ask::Create(c.args.prompt.clone()), Event::Form(self.run_form(id, c, true, &HashMap::new()))),
            "options" => (Ask::Options, Event::Form(self.run_form(id, c, false, &HashMap::new()))),
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
            Ask::Create(_) | Ask::Options if draft => {
                self.inbox.push(Event::Form(self.run_form(id, &c, matches!(ask, Ask::Create(_)), answers)));
                return;
            }
            Ask::Create(_) if answers.get("source").is_some_and(|v| v == SOURCE_ENTER) && get("source:typed") == "nowhere" => {
                let errors = vec![FieldError { field: "source:typed".into(), why: "nowhere is not a directory".into() }];
                self.inbox.push(Event::FieldErrors(FieldErrors { form: id, errors }));
                return;
            }
            Ask::Create(prompt) => {
                self.asks.remove(&id);
                let r = self.create(&c, prompt, answers);
                self.inbox.push(Event::Result(r));
                return;
            }
            Ask::Options => {
                let items = |key: &str| (0..).map_while(|i| answers.get(&format!("{key}:{i}")).cloned()).filter(|v| !v.trim().is_empty()).collect::<Vec<_>>();
                self.run.harness = get("harness");
                self.run.dirty = get("dirty");
                self.run.detach = get("detach") == "on";
                self.run.env = items("env");
                self.send_chips();
                done("run options saved".into())
            }
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
            Ask::Reset(harness, overrides) => {
                let name = self.harnesses.iter().find(|h| h.id == harness).map(|h| h.name.clone()).unwrap_or_default();
                let note = match get("key").as_str() {
                    "yes" if !overrides => format!("reset {name} to its bundle's defaults, keeping your files"),
                    "reset" if overrides => format!("reset {name} to its bundle's defaults, keeping your files"),
                    "reset with your files" if overrides => format!("reset {name} to its bundle's files alone"),
                    _ => {
                        self.asks.remove(&id);
                        self.inbox.push(Event::Result(canceled));
                        return;
                    }
                };
                CommandResult { id: command, ..self.harnessed(Status::Done, &harness, note) }
            }
            Ask::Variant(_) if draft => return,
            Ask::Variant(_) if get("name").is_empty() || get("slug").is_empty() => {
                let field = if get("name").is_empty() { "name" } else { "slug" };
                let errors = vec![FieldError { field: field.into(), why: format!("give the variant a {field}") }];
                self.inbox.push(Event::FieldErrors(FieldErrors { form: id, errors }));
                return;
            }
            Ask::Variant(parent) => {
                let (name, slug) = (get("name"), get("slug"));
                let mut h = mock_harness(&format!("hc_{slug}"), &format!("{name} (variant of {parent})"), false, false, true, &[]);
                h.age = "now".into();
                self.harnesses.push(h.clone());
                CommandResult { id: command, ..self.harnessed(Status::Done, &h.id, format!("created variant {name:?}")) }
            }
            Ask::Edit(..) => return,
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
                self.inbox.push(Event::Sessions(sub, Ok(self.sessions(&sandbox))));
                self.following.insert(sub, sandbox);
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
            Out::Subscribe(Subscribe { sub, topic: Topic::Composer, .. }) => {
                self.composer_sub = sub;
                self.send_chips();
            }
            Out::Subscribe(Subscribe { sub, topic: Topic::Provisioning, r#box, .. }) => {
                let step = self.sandbox(&r#box).map(|b| b.provisioning.clone()).unwrap_or_default();
                self.inbox.push(Event::Provisioning(sub, Ok(Provisioning { step })));
            }
            Out::Draft(prompt) => self.record.lock().unwrap().drafts.push(prompt),
            Out::Unsubscribe(sub) => {
                self.following.remove(&sub);
            }
            Out::Subscribe(_) => {}
            Out::Command(c) if c.command == "harnesses" => {
                if let Some(r) = self.harness(&c) {
                    self.inbox.push(Event::Result(CommandResult { id: c.id, ..r }));
                }
            }
            Out::Saved(v) => self.saved(v),
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
            // A service's tab draws what it has written, and its card when it is not running; it
            // reads nothing, so it needs no shell.
            Out::Open { stream, sandbox, exec, .. } if exec.starts_with("service:") => {
                let screen = self.services.get(&sandbox).into_iter().flatten().find(|v| v.tab().exec == exec).map(MockService::screen);
                let Some(screen) = screen else {
                    let why = "the service is no longer declared".into();
                    self.inbox.push(Event::Stream(Stream { stream, state: StreamState::Failed, why, end: Some(TabEnd::Tab), ..Default::default() }));
                    return;
                };
                self.inbox.push(Event::Stream(Stream { stream, state: StreamState::Attached, ..Default::default() }));
                self.inbox.push(Event::Data(stream, screen.into_bytes()));
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
        let Some(Event::Sessions(2, Ok(s))) = read(r#"{"type":"snapshot","snapshot":{"sub":2,"topic":"sessions","sessions":{"running":true,"sessions":[{"exec":"primary","kind":"primary","title":"","live":true,"argv":[]}]}}}"#) else { panic!("sessions") };
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
