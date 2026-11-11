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
    Answer, Appearance, AuditBelow, Box as Sandbox, ChoiceGroup, Chip, Close, Command, Commands, Choice, Composer, Draft, Entry, Field, FieldError, FieldErrors, Filter, FilterChoice, Follow,
    Form, GitTone, Header, Link, Listing, Message, Open, Palette, Provisioning, PaletteCommit, PaletteQuery, PaletteRow, PortGroup, Progress, Push, Question,
    Remark, Result as CommandResult, Row, Service, ServiceAction, ServiceVerb, Session, Sessions, Spec, SpecTarget, State, Status, Stream, StreamState,
    Submit, Subscribe, Tab, TabEnd, TabKind, Topic, Type, Unsubscribe, Edit, Saved, Harness, HarnessAction, HarnessOffer, HarnessFile,
    HarnessChanges, ChangedFile, ChangeOffer, HarnessChange, CardRow, CardSpan, CardTone, Tool, ToolFile, Address, Upload, Secret, SecretAction, SecretBinding, SecretStanding,
    Decision, DetailSection, DetailField, DetailLine, SecretWords, Requests, Arrival, Request, RequestKind, Notice, NoticeKind, FormAction, CardAction,
    Audit, AuditPart, AuditRecord, AuditRecording, AuditRun, AuditText,
    Issue, IssueAct, IssueItem, IssueItemKind, IssueMark, IssueRef, IssueState, IssueTone, Label, PullRequest, Merge, Reviewer, Checks, Check,
    Thread, ThreadComment,
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
    /// A link pressed in a pane, for the core to say where it goes.
    Follow(Follow),
    /// Images for one of a box's sessions: the clipboard's, or files dropped on its pane.
    Upload(Upload),
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
            Out::Follow(f) => Message { r#type: Type::Follow, follow: Some(f), ..Default::default() },
            Out::Upload(u) => Message { r#type: Type::Upload, upload: Some(u), ..Default::default() },
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
    /// A box's audit timeline, whole, or why its trails could not be read.
    Audit(u32, Result<Audit, String>),
    /// A box's issue or pull request as its tab draws it, or why it could not be read.
    Issue(u32, Result<Issue, String>),
    /// What waits on a person: the requests, the refused credentials, each box's notice, the
    /// requests just arrived, and what renewals the window allowed said.
    Requests(u32, Result<Requests, String>),
    /// What a workspace's automatic push said: what moved, or what was refused.
    Push(u32, Push),
    /// A notification of a request pressed: the request to answer.
    Answer(Arrival),
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
    /// Asks for a frame, and so a poll, with nothing arrived: a system appearance the view saw
    /// change, which the poll draws on its spring (`theme::see_system`).
    pub fn wake(&self) {
        if let Some(w) = self.1.lock().unwrap().as_ref() {
            w();
        }
    }
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
                Topic::Audit => Some(Event::Audit(s.sub, match s.audit {
                    Some(v) if s.error.is_empty() => Ok(v),
                    _ => Err(s.error),
                })),
                Topic::Issue => Some(Event::Issue(s.sub, match s.issue {
                    Some(v) if s.error.is_empty() => Ok(v),
                    _ => Err(s.error),
                })),
                Topic::Requests => Some(Event::Requests(s.sub, match s.requests {
                    Some(v) if s.error.is_empty() => Ok(v),
                    _ => Err(s.error),
                })),
                // The push says nothing of a look that failed as a whole; a snapshot without one
                // is nothing to say.
                Topic::Push => s.push.map(|p| Event::Push(s.sub, p)),
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
    inbox: Inbox,
}

impl Bridge {
    /// Says hello and waits for the welcome; then what arrives goes to the inbox, until the CLI
    /// is gone, when the app is too. Beside the spec, the prompt the last window left.
    pub fn connect(inbox: Inbox) -> Result<(Bridge, Spec, Session), String> {
        let reader = inbox.clone();
        let (client, welcome) = Client::connect(std::io::stdin(), std::io::stdout(), move |incoming| match incoming {
            Incoming::Message(m) => {
                if let Some(e) = event(m) {
                    reader.push(e);
                }
            }
            Incoming::Output(stream, bytes) => reader.push(Event::Data(stream, bytes)),
            Incoming::Exited(stream, why) => reader.push(Event::Exited(stream, why)),
            // Nothing more will arrive, and nothing can be sent.
            Incoming::Closed => std::process::exit(0),
        })?;
        let session = welcome.session.clone().unwrap_or_default();
        Ok((Bridge { client, inbox }, welcome.spec.ok_or("the CLI's welcome carried no spec")?, session))
    }
}

impl Backend for Bridge {
    // A write fails once the CLI is gone, and then the reader ends the app; or for a message past
    // the bridge's bound, which is refused whole and the bridge goes on. What waits on a result
    // for it (a command, an upload holding its pane's input, a link followed) gets one, failed
    // with why.
    fn send(&mut self, m: Out) {
        let m = m.message();
        if let Err(e) = self.client.send(&m) {
            eprintln!("desktop: a message could not be written: {e}");
            let id = m.command.as_ref().map(|c| c.id).or(m.upload.as_ref().map(|u| u.id)).or(m.follow.as_ref().map(|f| f.id));
            if let Some(id) = id {
                self.inbox.push(Event::Result(CommandResult { id, status: Status::Failed, why: format!("could not send it to the CLI: {e}"), ..Default::default() }));
            }
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
    /// The header subscriptions, by the box each follows: a box's forward, which a link in its
    /// panes is pointed through, is held only while one follows it, as the bridge's is.
    headers: HashMap<u32, String>,
    /// The project's harnesses, and each one's files' contents by its ID and path.
    harnesses: Vec<Harness>,
    files: HashMap<(String, String), String>,
    /// Each box's harness changes, by its key: the first box has some to save.
    changes: HashMap<String, HarnessChanges>,
    /// The project's secrets, as the core lists them.
    secrets: Vec<Secret>,
    /// The requests waiting on a person, the requests subscription that says them with each
    /// box's notice, and the boxes whose notice was dismissed.
    requests: Vec<Request>,
    requests_sub: u32,
    dismissed: Vec<String>,
    /// Whether the project owes the harness changes explainer: the requests topic names the
    /// first box with changes to save while it does, and asking it pays it.
    explain: bool,
    /// The action each form was submitted with, by its ID, for the one that answers it.
    actions_taken: HashMap<u64, CardAction>,
    /// The run options a create is made with, and the composer topic that says them.
    run: MockRun,
    composer_sub: u32,
    /// What the app sent that only the CLI would act on, for tests to read.
    record: Arc<Mutex<MockRecord>>,
    /// The theme the window was last picked to draw in, which its question opens on a change from.
    appearance: Appearance,
    /// Each box's issue and pull request as read, by its key and whether it is the pull request,
    /// and the issue subscriptions following each: an act sends them the issue again.
    issues: HashMap<(String, bool), Issue>,
    issue_subs: HashMap<u32, (String, bool)>,
    /// The audit subscriptions following a box's timeline, by box key: the bridge reads one box's
    /// trails once for all of them, so one made while another follows is answered at once.
    audit_subs: HashMap<u32, String>,
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
    /// Every theme picked, as the CLI would keep it for the next window.
    pub appearances: Vec<Appearance>,
    pub created: Vec<String>,
    /// Every upload, as sent.
    pub uploads: Vec<Upload>,
    /// The push subscriptions the app holds, by subscription: the box each pushes.
    pub pushing: HashMap<u32, String>,
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
            ..Default::default()
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
    /// A tool's file, by the tool's ID and the file's name, written out to edit.
    ToolEdit(String, String, std::path::PathBuf),
    /// The create card, for the prompt the composer held.
    Create(String),
    Options,
    /// A close or reopen of a box's issue or pull request, by its key and whether it is the pull
    /// request; and a pull request's merge.
    IssueState(String, bool),
    Merge(String),
    /// A secret's new one, edit, binding, delete or unbind, by the secret's ID (and the
    /// binding's, for an unbind).
    Secret(SecretAction, String, String),
    /// A request's card, by the request's ID; from the secrets page, answered with the page.
    Request(String, bool),
    /// The harness changes explainer, about a box by its key.
    Explain(String),
    /// The theme command's question.
    Theme,
}

/// A field of a card the core describes, in its tone.
fn mock_field(label: &str, value: &str, tone: CardTone) -> DetailField { DetailField { label: label.into(), value: value.into(), tone } }

/// A mock secret, as the core lists and reads it: its row, its card, its live bindings, each as
/// (ID, variable or "given on", what it is on), and whether an upstream refuses it.
fn mock_secret(id: &str, name: &str, kind: &str, routes: &[&str], bindings: &[(&str, &str, &str)], refused: &str) -> Secret {
    let mut fields = vec![mock_field("kind", kind, CardTone::Plain), mock_field("binding limit", "none — bindings of it may last forever", CardTone::Plain)];
    if routes.is_empty() {
        fields.push(mock_field("may be sent to", "nowhere — it has no routes", CardTone::Alert));
    }
    for (i, r) in routes.iter().enumerate() {
        fields.push(mock_field(if i == 0 { "route" } else { "or route" }, &format!("{r} · allowed"), CardTone::Quiet));
    }
    let bindings: Vec<SecretBinding> = bindings.iter().map(|(bid, var, target)| {
        let card = vec![DetailSection { label: "the binding".into(), fields: vec![
            mock_field("secret", name, CardTone::Standout), mock_field("bound to", target, CardTone::Plain),
            mock_field("as variable", var, CardTone::Standout), mock_field("may go to", "where the secret's routes send it", CardTone::Quiet),
            mock_field("expires", "never", CardTone::Plain),
        ], lines: vec![] }];
        SecretBinding { id: bid.to_string(), label: var.to_string(), detail: format!("{target} · never expires"), card }
    }).collect();
    let mut s = Secret {
        id: id.into(), name: name.into(), r#where: if routes.is_empty() { "sent nowhere".into() } else { routes.join("; ") }, nowhere: routes.is_empty(),
        age: "2d ago".into(), refused: refused.into(), card: vec![DetailSection { label: "the credential".into(), fields, lines: vec![] }], bindings,
        ..Default::default()
    };
    mock_stand(&mut s);
    s
}

/// What a mock secret's row says stands on it, as the core's `SecretStandingOf` says it.
fn mock_stand(s: &mut Secret) {
    let n = s.bindings.len();
    (s.standing_text, s.standing) = match n {
        _ if !s.refused.is_empty() => ("refused".into(), SecretStanding::Refused),
        0 => ("—".into(), SecretStanding::Unbound),
        1 => ("1 binding".into(), SecretStanding::Bound),
        n => (format!("{n} bindings"), SecretStanding::Bound),
    };
    s.answer = match n {
        0 => "nothing is bound to it".into(),
        1 => "1 binding stands on it".into(),
        n => format!("{n} bindings stand on it"),
    };
}

/// The mock project's secrets: one bound twice, an oauth login bound to a harness, one sent
/// nowhere and bound to nothing, and one an upstream refuses.
fn mock_secrets() -> Vec<Secret> {
    vec![
        mock_secret("sec_gh", "github", "token", &["api.github.com", "github.com/discobox-ai/*"], &[
            ("bind_gh1", "GH_TOKEN", "discobox rune console port"), ("bind_gh2", "GITHUB_TOKEN", "every discobox in the project"),
        ], ""),
        mock_secret("sec_claude", "claude-login", "oauth", &["api.anthropic.com"], &[("bind_cl1", "CLAUDE_CODE_OAUTH_TOKEN", "harness Claude")], ""),
        mock_secret("sec_npm", "npm", "token", &[], &[], ""),
        mock_secret("sec_openai", "openai", "token", &["api.openai.com"], &[("bind_oa1", "OPENAI_API_KEY", "harness Codex")], "refused at api.openai.com · nothing to renew"),
    ]
}

/// What the secrets page says beside its secrets, as the core's words.
fn mock_words(waiting: usize) -> SecretWords {
    SecretWords {
        empty: "no secrets in this project yet".into(), bind: "Bind this secret…".into(), bind_detail: "hand it over before anything asks".into(),
        history_title: "Binding history and answered requests".into(), history_note: "Latest 100 changes".into(),
        requests: "Requests".into(), waiting: format!("{waiting} waiting"), nothing_waiting: "nothing is waiting".into(),
    }
}

/// The request the mock's second box's agent made: github, to use in GH_TOKEN.
fn mock_request(sandbox: &str) -> Request {
    Request {
        id: "req_gh".into(), r#box: sandbox.into(), kind: RequestKind::Credential, name: "github".into(), env_var: "GH_TOKEN".into(),
        justification: "push the fix branch".into(), subject: "github".into(), r#where: "api.github.com".into(),
        requester: "agent · p8c3n0vd".into(), age: "2m ago".into(), ..Default::default()
    }
}

/// A request's card, as the core says its use part as it is answered (`console.RequestApproval`).
fn mock_request_card(r: &Request) -> Vec<DetailSection> {
    vec![
        DetailSection { label: String::new(), fields: vec![
            mock_field("Box", &r.r#box, CardTone::Standout), mock_field("Reason (box's words)", &r.justification, CardTone::Plain),
            mock_field("Variable", &r.env_var, CardTone::Plain),
        ], lines: vec![] },
        DetailSection { label: String::new(), fields: vec![], lines: vec![DetailLine { text: "This box may use the credential.".into(), tone: CardTone::Plain, ..Default::default() }] },
        DetailSection { label: "Grant restriction · Requested routes".into(), fields: vec![], lines: vec![
            DetailLine { text: format!("HTTP {}", r.r#where), tone: CardTone::Standout, ..Default::default() },
            DetailLine { text: "  METHOD  PATH".into(), tone: CardTone::Quiet, ..Default::default() },
            DetailLine { text: "  *       *".into(), tone: CardTone::Standout, ..Default::default() },
        ] },
    ]
}

/// A secret's binding history, as the core reads each decision.
fn mock_history() -> Vec<Decision> {
    let decision = |id: &str, at: &str, event: &str, actor: &str, var: &str| Decision {
        id: id.into(), label: format!("{at} · {event} · {actor}"),
        card: vec![DetailSection { label: "the decision".into(), fields: vec![
            mock_field("event", event, CardTone::Plain), mock_field("event actor", actor, CardTone::Plain), mock_field("event at", at, CardTone::Plain),
            mock_field("variable", var, CardTone::Plain), mock_field("expires", "forever", CardTone::Plain),
            mock_field("effective limits", "not recorded for this historical change", CardTone::Plain),
        ], lines: vec![] }],
        delegation: vec![],
    };
    vec![
        decision("ev_2", "2026-11-09T14:02:11Z", "binding.created", "darren", "GITHUB_TOKEN"),
        decision("ev_1", "2026-11-08T09:30:00Z", "request.approved", "darren", "GH_TOKEN"),
    ]
}

/// The tools every mock box declares, as the core lists them for one up or not: a diff viewer, an
/// editor carrying its config, an IDE on this machine, and a declaration that cannot run.
fn mock_tools(running: bool) -> Vec<Tool> {
    let down = "the box has no container; repair rebuilds it";
    let tool = |id: &str, label: &str, detail: &str, host: bool, files: Vec<ToolFile>| Tool {
        id: id.into(), label: label.into(), detail: detail.into(), host, enabled: running, why: if running { String::new() } else { down.into() }, files,
    };
    let file = |name: &str, home: &str| ToolFile { name: name.into(), detail: format!("→ ~/{home}  ·  ~/.config/discobox/tools/fresh/{name}") };
    vec![
        tool("diff", "diff", "review the working tree's changes", false, vec![]),
        tool("fresh", "fresh", "a terminal editor", false, vec![file("config.json", ".config/fresh/config.json"), file("keys.json", ".config/fresh/keys.json")]),
        tool("vscode", "VS Code", "open the box in VS Code on this machine", true, vec![]),
        Tool { enabled: false, why: "its command is empty".into(), ..tool("broken", "broken", "", false, vec![]) },
    ]
}

/// Every mock box's audit timeline, as the core sends it: an agent paging an API (three exchanges
/// of one group, folded into one run), a credential lent, a command run, and a last exchange.
pub fn mock_audit() -> Audit {
    let record = |id: &str, clock: &str, source: &str, summary: &str, group: &str| AuditRecord {
        id: id.into(), clock: clock.into(), source: source.into(), summary: summary.into(), group: group.into(),
        group_summary: if group.is_empty() { String::new() } else { "GET 200 https://api.github.com/repos/discobox-ai/discobox/issues".into() },
        ..Default::default()
    };
    let page = |id: &str, clock: &str, n: u32| {
        let summary = format!("GET 200 https://api.github.com/repos/discobox-ai/discobox/issues?page={n}");
        record(id, clock, "http", &summary, "GET 200 api.github.com")
    };
    let records = vec![
        record("creds-1", "14:02:11", "creds", "github lent to gh as GH_TOKEN", ""),
        page("http-1", "14:02:12", 1),
        page("http-2", "14:02:12", 2),
        page("http-3", "14:02:13", 3),
        record("execs-1", "14:02:20", "execs", "go test ./... (exit 0, 41s)", ""),
        record("http-4", "14:03:02", "http", "POST 201 https://api.github.com/repos/discobox-ai/discobox/pulls", ""),
    ];
    let runs = vec![
        AuditRun { first: 0, last: 0, summary: String::new(), ..Default::default() },
        AuditRun { first: 1, last: 3, summary: "GET 200 https://api.github.com/repos/discobox-ai/discobox/issues (count 3)".into(), ..Default::default() },
        AuditRun { first: 4, last: 4, summary: String::new(), ..Default::default() },
        AuditRun { first: 5, last: 5, summary: String::new(), ..Default::default() },
    ];
    Audit { records, runs, missing: vec![], below: below(false), ..Default::default() }
}

/// What the core says below a held row (`console.AuditBelowWords`), as the mock sends it.
fn below(searching: bool) -> AuditBelow {
    let (one, many) = if searching { ("newer match below", "newer matches below") } else { ("newer record below", "newer records below") };
    AuditBelow { one: one.into(), many: many.into() }
}

/// The mock's timeline under a search, in the shape the core sends one: the records whose line or
/// ID holds the query, lit where their line does, the runs folded over them, and the tally. The
/// match is a plain stand-in for the core's (`console.AuditSearch`), which the mock does not
/// reproduce: it is enough to tell what the view does with what it is sent.
pub fn mock_audit_search(query: &str) -> Audit {
    let all = mock_audit();
    let q = query.to_lowercase();
    let at = |text: &str| text.to_lowercase().find(&q).map(|b| {
        let start = text[..b].chars().count() as i64;
        (start..start + q.chars().count() as i64).collect::<Vec<_>>()
    });
    let mut a = Audit { missing: vec![], ..Default::default() };
    for r in &all.records {
        let (source, summary) = (at(&r.source), at(&r.summary));
        if source.is_some() || summary.is_some() || r.id.to_lowercase().contains(&q) {
            a.records.push(AuditRecord { source_lit: source.unwrap_or_default(), summary_lit: summary.unwrap_or_default(), ..r.clone() });
        }
    }
    let mut i = 0;
    while i < a.records.len() {
        let mut j = i;
        while !a.records[i].group.is_empty() && j + 1 < a.records.len() && a.records[j + 1].group == a.records[i].group {
            j += 1;
        }
        let mut run = AuditRun { first: i as i64, last: j as i64, ..Default::default() };
        if j > i {
            run.summary = format!("{} (count {})", a.records[j].group_summary, j - i + 1);
            run.summary_lit = at(&run.summary).unwrap_or_default();
        }
        a.runs.push(run);
        i = j + 1;
    }
    if a.records.is_empty() {
        a.note = "no records match".into();
    }
    a.tally = format!("{} of {}", a.records.len(), all.records.len());
    a.below = below(true);
    a
}

/// The audit command, as the core answers it for the mock's timeline: the view, a record in full
/// with the recordings it offers, or one of them.
fn mock_audit_command(c: &Command) -> CommandResult {
    let sandbox = c.targets.first().cloned().unwrap_or_default();
    let a = &c.args;
    if a.record.is_empty() {
        return CommandResult { status: Status::Enter, r#box: sandbox, ..Default::default() };
    }
    let Some(r) = mock_audit().records.into_iter().find(|r| r.id == a.record) else {
        return CommandResult { status: Status::Failed, why: format!("{}: no such record", a.record), ..Default::default() };
    };
    let http = r.source == "http";
    if let Some(part) = a.part {
        let (title, text) = match part {
            AuditPart::Response => ("Response body", "{\n  \"number\": 816,\n  \"title\": \"desktop: audit\",\n  \"state\": \"open\"\n}\n"),
            AuditPart::Request => ("Request body", "{\"title\": \"desktop: audit\"}\n"),
            AuditPart::Stream => return CommandResult { status: Status::Failed, why: "stream: not recorded".into(), ..Default::default() },
        };
        let text = AuditText { record: r.id, part: Some(part), title: title.into(), text: text.into(), recordings: vec![] };
        return CommandResult { status: Status::Done, r#box: sandbox, audit: Some(text), ..Default::default() };
    }
    let text = if http {
        format!("ID:       {}\nTime:     {}\nSource:   http\n\n{}\n\nRequest headers:\n\tAccept: application/vnd.github+json\n\tAuthorization: [lent: github]\n", r.id, r.clock, r.summary)
    } else {
        format!("ID:       {}\nTime:     {}\nSource:   {}\n\n{}\n", r.id, r.clock, r.source, r.summary)
    };
    let recordings = if http {
        vec![
            AuditRecording { part: AuditPart::Response, label: "response body".into() },
            AuditRecording { part: AuditPart::Request, label: "request body".into() },
        ]
    } else {
        vec![]
    };
    let text = AuditText { record: r.id, part: None, title: "Audit record".into(), text, recordings };
    CommandResult { status: Status::Done, r#box: sandbox, audit: Some(text), ..Default::default() }
}

/// The number a mock box's `issue=` or `pr=` tag gives its work, as the core's
/// `Sandbox.WorkNumber` reads it.
fn work_number(b: &Sandbox, pull: bool) -> Option<i64> {
    let prefix = if pull { "pr=" } else { "issue=" };
    b.tags.iter().find_map(|t| t.strip_prefix(prefix)?.parse().ok())
}

fn mark(filled: bool, tone: IssueTone) -> IssueMark { IssueMark { filled, tone } }

fn mock_item(kind: IssueItemKind, actor: &str, ago: &str, text: &str, body: &str) -> IssueItem {
    IssueItem { kind, actor: actor.into(), ago: ago.into(), text: text.into(), body: body.into(), threads: vec![], ..Default::default() }
}

/// A mock box's issue or pull request, as the bridge would send it: every rule's answer already
/// the core's.
pub fn mock_issue(number: i64, pull: bool) -> Issue {
    let repo = "https://github.com/discobox-ai/discobox";
    if !pull {
        return Issue {
            label: format!("issue #{number}"), url: format!("{repo}/issues/{number}"), repository: "discobox-ai/discobox".into(), number,
            title: "Reaper races the pool agent on a box it is still starting".into(), state: IssueState::Open, badge: "Open".into(),
            mark: mark(true, IssueTone::Open), author: "darren".into(), opened: "3d ago".into(), comments: 2,
            body: "The reaper takes a box the pool agent is **still starting** for one that has died.\n\n```go\nif box.State == Starting {\n\treturn // not ours yet\n}\n```\n\n- reproduce with `task test:race`\n- see the [agent's log](https://example.com/log)".into(),
            reactions: "+1 2 · eyes 1".into(),
            labels: vec![Label { name: "type/bug".into(), color: "d73a4a".into() }, Label { name: "area/pool".into(), color: "0e8a16".into() }],
            assignees: vec!["darren".into()], milestone: "0.9".into(),
            sub_issues: vec![IssueRef { number: "#818".into(), title: "a test that reaps a starting box".into(), mark: mark(false, IssueTone::Shut) }],
            sub_total: 2, sub_done: 1,
            timeline: vec![
                mock_item(IssueItemKind::Event, "darren", "3d ago", "added the type/bug label", ""),
                mock_item(IssueItemKind::Comment, "ibuildthecloud-bot", "2d ago", "", "Reproduced: the reaper's tick lands between the create and the agent's first report."),
                mock_item(IssueItemKind::Event, "ibuildthecloud-bot", "1d ago", "mentioned this in #912 reaper: leave a box that is starting", ""),
            ],
            ..Default::default()
        };
    }
    let mut review = mock_item(IssueItemKind::Review, "alice", "5h ago", "approved these changes", "Looks right; one nit.");
    review.tone = Some(IssueTone::Good);
    review.threads = vec![Thread {
        r#where: "pool-agent/reaper.go:42".into(),
        hunk: vec!["@@ -38,6 +38,9 @@ func (r *Reaper) tick() {".into(), " \tfor _, b := range boxes {".into(), "+\t\tif b.Starting() {".into(), "+\t\t\tcontinue".into()],
        comments: vec![ThreadComment { author: "alice".into(), ago: "5h ago".into(), body: "Say *why* in a comment here.".into(), reactions: String::new() }],
    }];
    let mut commit = mock_item(IssueItemKind::Commit, "darren", "1d ago", "reaper: leave a box that is starting", "");
    commit.commit = "8be21c0".into();
    Issue {
        label: format!("PR #{number}"), url: format!("{repo}/pull/{number}"), repository: "discobox-ai/discobox".into(), number, pull: true,
        title: "reaper: leave a box that is starting".into(), state: IssueState::Open, badge: "Open".into(), mark: mark(true, IssueTone::Open),
        author: "darren".into(), opened: "1d ago".into(), comments: 1, body: "Closes #817.\n\nThe reaper skips a box the agent has not reported on yet.".into(),
        pull_request: Some(PullRequest {
            head: "fix/reaper-race".into(), base: "main".into(), head_sha: "8be21c0f00d1".into(), base_sha: "41d9e2a00000".into(),
            commits: 1, additions: 58, deletions: 12, changed_files: 3,
            merge: Some(Merge { ready: true, said: "Ready to merge into main".into(), mark: mark(true, IssueTone::Good), ..Default::default() }),
            reviewers: vec![Reviewer { name: "alice".into(), said: "approved".into(), mark: mark(true, IssueTone::Open) }, Reviewer { name: "bob".into(), said: "review requested".into(), mark: mark(false, IssueTone::Quiet) }],
            checks: Checks { passed: 11, pending: 1, unpassed: vec![Check { name: "e2e (linux)".into(), url: "https://example.com/e2e".into(), mark: mark(true, IssueTone::Busy) }], ..Default::default() },
            ..Default::default()
        }),
        timeline: vec![commit, review, mock_item(IssueItemKind::Comment, "bob", "2h ago", "", "Ran it against the dev pool: no more reaped starts.")],
        ..Default::default()
    }
}

fn mock_exec(exec: &str, kind: TabKind, title: &str) -> Tab {
    // Which take images and the image keys, as the core's `Tab.TakesImages` and `TakesImageKeys` say.
    let (images, image_keys) = (kind != TabKind::Service, matches!(kind, TabKind::Primary | TabKind::Terminal));
    Tab { exec: exec.into(), kind, title: title.into(), live: true, argv: vec![], images, image_keys, ..Default::default() }
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
                tags: vec!["ENG-12".into(), "issue=817".into(), "pr=912".into()], branch: "fix/reaper-race".into(), commit: "8be21c0".into(),
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
        let requests = vec![mock_request(&boxes[1].key)];
        Mock {
            inbox, boxes, parents, list_sub: 0, filter, extra: HashMap::new(), ptys: HashMap::new(), made: 0, asks: HashMap::new(), next_ask: 1,
            services, following: HashMap::new(), headers: HashMap::new(), harnesses, files, changes, secrets: mock_secrets(),
            requests, requests_sub: 0, dismissed: vec![], explain: false, actions_taken: HashMap::new(), run: MockRun::default(), composer_sub: 0, record: Arc::default(), appearance: Appearance::System,
            issues: HashMap::new(), issue_subs: HashMap::new(), audit_subs: HashMap::new(),
        }
    }

    /// The mock as the CLI of a window opening on the theme the last one was picked to draw in, as
    /// its welcome says (`Session.appearance`): what its theme question opens on a change from.
    #[cfg(test)]
    pub fn opening_on(mut self, a: Appearance) -> Mock {
        self.appearance = a;
        self
    }

    /// The mock on a project that has yet to explain saving harness changes.
    #[cfg(test)]
    pub fn owing_explainer(mut self) -> Mock {
        self.explain = true;
        self
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
        Form { id, command: c.id, title: title.into(), fields, note, action: action.into(), ..Default::default() }
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

    /// The tools command, as the core does it: a box's tools and addresses for the menu, a tool
    /// run — one on this machine handed the box, any other's session made on the stream as its tab
    /// (a local shell here) — or a tool's file edited. None for an edit, which waits on the app.
    fn tools(&mut self, c: &Command) -> Option<CommandResult> {
        let sandbox = c.targets.first().cloned().unwrap_or_default();
        let fail = |why: String| Some(CommandResult { status: Status::Failed, why, ..Default::default() });
        let running = self.sandbox(&sandbox).is_some_and(Sandbox::running);
        let catalog = mock_tools(running);
        if c.args.tool.is_empty() {
            let addresses = ["ssh", "git url"].iter().map(|label| Address {
                label: label.to_string(),
                value: if !running { String::new() } else if *label == "ssh" { format!("ssh {sandbox}") } else { format!("ssh://{sandbox}/workspace") },
                why: if running { String::new() } else { "the box has no container; repair rebuilds it".into() },
            }).collect();
            return Some(CommandResult { status: Status::Enter, r#box: sandbox, tools: catalog, addresses, ..Default::default() });
        }
        let Some(t) = catalog.into_iter().find(|t| t.id == c.args.tool) else {
            return fail(format!("no such tool: {}", c.args.tool));
        };
        if !c.args.file.is_empty() {
            if !t.files.iter().any(|f| f.name == c.args.file) {
                return fail(format!("{} carries no file {:?}", t.label, c.args.file));
            }
            let id = self.next_ask;
            let key = (format!("tool:{}", t.id), c.args.file.clone());
            let content = self.files.get(&key).cloned().unwrap_or_else(|| "{}\n".into());
            let file = std::env::temp_dir().join(format!("discobox-mock-tool-{}-{id}.json", std::process::id()));
            if let Err(e) = std::fs::write(&file, &content) {
                return fail(format!("edit {}: {e}", c.args.file));
            }
            self.next_ask += 1;
            self.asks.insert(id, (c.clone(), Ask::ToolEdit(t.id.clone(), c.args.file.clone(), file.clone())));
            let title = format!("Editing {} — {}", c.args.file, t.id);
            self.inbox.push(Event::Edit(Edit { id, command: c.id, path: file.to_string_lossy().into_owned(), title }));
            return None;
        }
        if !t.enabled {
            return fail(format!("{}: {}", t.label, t.why));
        }
        if t.host {
            let name = self.sandbox(&sandbox).map(|b| b.name.clone()).unwrap_or_default();
            return Some(CommandResult { status: Status::Done, r#box: sandbox, note: format!("opened {name} in {}", t.label), ..Default::default() });
        }
        // One the box is running is its session opened again, never a second (the core's
        // `OpenTool`); the mock's is a local shell, so it opens a new one under the same exec.
        let running = self.extra.get(&sandbox).into_iter().flatten().find(|e| e.tool == t.id).cloned();
        let exec = running.clone().unwrap_or_else(|| {
            self.made += 1;
            Tab { tool: t.id.clone(), ..mock_exec(&format!("exec_{}", self.made), TabKind::Tool, &t.label) }
        });
        let size = |n: i64| u16::try_from(n.max(1)).unwrap_or(u16::MAX);
        if let Err(e) = self.spawn(&sandbox, &exec, c.args.stream, size(c.args.cols), size(c.args.rows)) {
            return fail(e);
        }
        if running.is_none() {
            self.extra.entry(sandbox.clone()).or_default().push(exec.clone());
        }
        Some(CommandResult { status: Status::Done, r#box: sandbox, session: Some(exec), ..Default::default() })
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
                    let choice = |key: &str, label: &str, hint: &str| Choice { key: key.into(), label: label.into(), hint: hint.into(), ..Default::default() };
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

    /// A credentials result carrying the secrets as they stand, about one of them, and the
    /// requests waiting.
    fn secreted(&self, status: Status, about: &str, note: String) -> CommandResult {
        CommandResult {
            status, secrets: self.secrets.clone(), secret: about.into(), note, secret_words: Some(mock_words(self.requests.len())),
            requests: self.requests.clone(), ..Default::default()
        }
    }

    /// What waits on a person, as the requests topic says it: each request, and the notice of
    /// each box with one its window did not dismiss.
    fn waiting(&self) -> Requests {
        let notices = self.requests.iter().filter(|r| !r.r#box.is_empty() && !self.dismissed.contains(&r.r#box)).map(|r| Notice {
            r#box: r.r#box.clone(), kind: NoticeKind::Request, subject: "credential request".into(), detail: format!("{} for {}", r.name, r.r#where),
            call: "click to answer".into(), command: "requests".into(), urgent: true, ..Default::default()
        }).collect();
        // The core's rule, against the boxes listed: the first with changes to save.
        let explain = self.boxes.iter().find(|b| self.explain && self.changes.contains_key(&b.key)).map(|b| b.key.clone()).unwrap_or_default();
        Requests { requests: self.requests.clone(), notices, explain, ..Default::default() }
    }

    /// The harness changes explainer, asked as the bridge asks it: the once the project owes it,
    /// which asking pays, whichever way it is answered; any other asking is canceled unasked.
    fn explain(&mut self, c: &Command) {
        if !self.explain {
            self.inbox.push(Event::Result(CommandResult { id: c.id, status: Status::Canceled, ..Default::default() }));
            return;
        }
        self.explain = false;
        let target = c.targets.first().cloned().unwrap_or_default();
        let (name, harness) = self.sandbox(&target).map(|b| (b.name.clone(), b.harness.clone())).unwrap_or_default();
        let id = self.next_ask;
        self.next_ask += 1;
        self.asks.insert(id, (c.clone(), Ask::Explain(target)));
        let choice = |key: &str, label: &str, hint: &str| Choice { key: key.into(), label: label.into(), hint: hint.into(), ..Default::default() };
        self.inbox.push(Event::Question(Question {
            id, command: c.id, title: "Saving harness changes".into(), subject: name,
            body: format!("{harness} in this discobox changed its settings or signed in.\n\nSaving makes those settings, and that login, the shared default for every discobox on {harness} in this project."),
            choices: vec![choice("review", "Review changes", "see what changed, then save or discard it"), choice("later", "Not now", "the workspace goes on saying so, to review it later")],
            ..Default::default()
        }));
        self.send_waiting();
    }

    fn send_waiting(&self) {
        if self.requests_sub != 0 {
            self.inbox.push(Event::Requests(self.requests_sub, Ok(self.waiting())));
        }
    }

    /// A request's card put to the app, for the command that answers it: the core's part card,
    /// accepted or denied. One no longer waiting fails.
    fn ask_request(&mut self, c: &Command, request: &str, page: bool) -> Option<CommandResult> {
        let Some(r) = self.requests.iter().find(|r| r.id == request).cloned() else {
            return Some(CommandResult { status: Status::Failed, why: "the request is no longer waiting".into(), ..Default::default() });
        };
        let id = self.next_ask;
        self.next_ask += 1;
        self.asks.insert(id, (c.clone(), Ask::Request(r.id.clone(), page)));
        let actions = vec![
            FormAction { key: CardAction::Accept, label: "Accept".into(), declines: false },
            FormAction { key: CardAction::Deny, label: "Deny".into(), declines: true },
        ];
        self.inbox.push(Event::Form(Form {
            id, command: c.id, title: "Allow credential use?".into(), aside: "1 of 1".into(), card: mock_request_card(&r), actions,
            leave: "leave it waiting".into(), ..Default::default()
        }));
        None
    }

    /// A request's card answered: approved or denied, as the core says it, and the request no
    /// longer waiting.
    fn request_answered(&mut self, request: &str, action: CardAction, page: bool) -> CommandResult {
        let name = self.requests.iter().find(|r| r.id == request).map(|r| r.name.clone()).unwrap_or_default();
        self.requests.retain(|r| r.id != request);
        self.send_waiting();
        let note = if action == CardAction::Accept { format!("approved {name} for 1 day") } else { format!("denied {name}") };
        if page { self.secreted(Status::Done, "", note) } else { CommandResult { status: Status::Done, note, ..Default::default() } }
    }

    /// The credentials command: the page's secrets, or an action done as the core does it. None
    /// for one that waits on the app: a form, or a question asked first.
    fn credentials(&mut self, c: &Command) -> Option<CommandResult> {
        let fail = |why: String| Some(CommandResult { status: Status::Failed, why, ..Default::default() });
        let Some(action) = c.args.secret_action else {
            return Some(self.secreted(Status::Enter, "", String::new()));
        };
        if action == SecretAction::Read {
            return Some(self.secreted(Status::Done, "", String::new()));
        }
        if action == SecretAction::Answer {
            let request = c.args.request.clone();
            return self.ask_request(c, &request, true);
        }
        let id = self.next_ask;
        let field = |key: &str, label: &str, value: &str| Field { key: key.into(), label: label.into(), value: value.into(), applies: true, ..Default::default() };
        if action == SecretAction::New {
            let fields = vec![
                Field { required: true, placeholder: "e.g. github".into(), ..field("name", "name", "") },
                Field { masked: true, ..field("value", "value", "") },
            ];
            self.next_ask += 1;
            self.asks.insert(id, (c.clone(), Ask::Secret(action, String::new(), String::new())));
            self.inbox.push(Event::Form(Form { id, command: c.id, title: "New secret".into(), fields, ..Default::default() }));
            return None;
        }
        let Some(secret) = self.secrets.iter().find(|x| x.id == c.args.secret).cloned() else {
            return fail(format!("no secret {} is listed", c.args.secret));
        };
        let ask = Ask::Secret(action, secret.id.clone(), c.args.binding.clone());
        let event = match action {
            SecretAction::History if secret.id == "sec_gh" => return Some(CommandResult { status: Status::Done, secret: secret.id, history: mock_history(), secret_words: Some(mock_words(self.requests.len())), ..Default::default() }),
            SecretAction::History => return Some(CommandResult { status: Status::Done, secret: secret.id, note: "no binding history".into(), ..Default::default() }),
            SecretAction::Edit => {
                let fields = vec![Field { required: true, ..field("name", "name", &secret.name) }, Field { masked: true, placeholder: "unchanged — type to replace it".into(), ..field("value", "value", "") }];
                let kind = "the kind is the one thing it cannot be told: a token and an oauth credential renew differently, so that is a new credential rather than an edit";
                let about = if secret.refused.is_empty() { kind.to_string() } else { format!("{}\n{kind}", secret.refused) };
                Event::Form(Form { id, command: c.id, title: format!("Edit {}", secret.name), fields, about, ..Default::default() })
            }
            SecretAction::Bind => {
                let choice = |key: &str, label: &str| Choice { key: key.into(), label: label.into(), ..Default::default() };
                let target = Field { choices: vec![choice("project", "every discobox in this project"), choice("sandbox", "one discobox")], ..field("target", "bound to", "project") };
                let var = field("envVar", "as variable", &secret.name.to_uppercase().replace('-', "_"));
                let about = "a binding hands this credential over before anything asks for it".into();
                Event::Form(Form { id, command: c.id, title: format!("Bind {}", secret.name), fields: vec![target, var], about, ..Default::default() })
            }
            SecretAction::Delete => {
                let body = format!("Delete {}? Its live bindings ({}) are all taken back, and a discobox holding a sentinel for it keeps a placeholder that now resolves to nothing.", secret.name, secret.bindings.len());
                Event::Question(Question { id, command: c.id, title: "Delete secret".into(), body, default_no: true, ..Default::default() })
            }
            SecretAction::Unbind => {
                let Some(b) = secret.bindings.iter().find(|b| b.id == c.args.binding) else { return fail("that binding is no longer there".into()) };
                let target = b.detail.split(" · ").next().unwrap_or_default();
                let body = format!("Take back the binding of {} to {target} as {}? The credential stops resolving at once, wherever it is being used. The request that produced it stays approved, because it is history.", secret.name, b.label);
                Event::Question(Question { id, command: c.id, title: "Unbind".into(), body, default_no: true, ..Default::default() })
            }
            SecretAction::New | SecretAction::Read | SecretAction::Answer => unreachable!(),
        };
        self.next_ask += 1;
        self.asks.insert(id, (c.clone(), ask));
        self.inbox.push(event);
        None
    }

    /// A secret's form submitted or question answered, as the core carries it out.
    fn secret_answered(&mut self, action: SecretAction, secret: &str, binding: &str, get: &dyn Fn(&str) -> String) -> CommandResult {
        let canceled = CommandResult { status: Status::Canceled, ..Default::default() };
        let name = self.secrets.iter().find(|x| x.id == secret).map(|x| x.name.clone()).unwrap_or_default();
        let note = match action {
            SecretAction::New => {
                let name = get("name");
                self.secrets.push(mock_secret(&format!("sec_{name}"), &name, "token", &[], &[], ""));
                format!("stored {name}")
            }
            SecretAction::Edit => {
                let to = get("name");
                if to.is_empty() || to == name {
                    format!("{name} is unchanged")
                } else {
                    if let Some(x) = self.secrets.iter_mut().find(|x| x.id == secret) {
                        x.name = to.clone();
                    }
                    format!("renamed {name} to {to}")
                }
            }
            SecretAction::Bind => {
                let var = Some(get("envVar")).filter(|v| !v.is_empty()).unwrap_or_else(|| name.to_uppercase().replace('-', "_"));
                let target = if get("target") == "sandbox" { "discobox rune console port" } else { "every discobox in the project" };
                if let Some(x) = self.secrets.iter_mut().find(|x| x.id == secret) {
                    let made = mock_secret("", &name, "", &[], &[(&format!("bind_{var}"), &var, target)], "");
                    x.bindings.extend(made.bindings);
                    mock_stand(x);
                }
                format!("bound {name} as {var}")
            }
            SecretAction::Delete if get("key") != "yes" => return canceled,
            SecretAction::Delete => {
                self.secrets.retain(|x| x.id != secret);
                format!("deleted {name}")
            }
            SecretAction::Unbind if get("key") != "yes" => return canceled,
            SecretAction::Unbind => {
                if let Some(x) = self.secrets.iter_mut().find(|x| x.id == secret) {
                    x.bindings.retain(|b| b.id != binding);
                    mock_stand(x);
                }
                "unbound".into()
            }
            SecretAction::History | SecretAction::Read | SecretAction::Answer => String::new(),
        };
        self.secreted(Status::Done, secret, note)
    }

    /// An edit finished with, or given up: what was saved is read back, as the core does.
    fn saved(&mut self, v: Saved) {
        if let Some((c, Ask::ToolEdit(tool, name, file))) = self.asks.get(&v.edit).cloned() {
            self.asks.remove(&v.edit);
            let result = if v.canceled {
                CommandResult { status: Status::Canceled, ..Default::default() }
            } else {
                let after = std::fs::read_to_string(&file).unwrap_or_default();
                let key = (format!("tool:{tool}"), name.clone());
                let changed = self.files.get(&key) != Some(&after);
                self.files.insert(key, after);
                let note = if changed { format!("{name} saved — the next discobox to open {tool} gets it") } else { format!("{name} unchanged") };
                CommandResult { status: Status::Done, note, ..Default::default() }
            };
            let _ = std::fs::remove_file(&file);
            self.inbox.push(Event::Result(CommandResult { id: c.id, ..result }));
            return;
        }
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
                PortGroup { protocol: "http".into(), ports: vec![
                    Link { label: "5173".into(), url: "http://localhost:5173".into(), ..Default::default() },
                    Link { label: "18080->8080".into(), url: "http://localhost:18080".into(), ..Default::default() },
                ] },
                PortGroup { protocol: "tcp".into(), ports: vec![Link { label: "5432".into(), ..Default::default() }] },
            ]
        } else {
            vec![]
        };
        let work = [false, true].into_iter().filter_map(|pull| {
            let n = work_number(&b, pull)?;
            let w = mock_issue(n, pull);
            Some(Link { label: w.label, url: w.url, command: if pull { "pr" } else { "issue" }.into() })
        }).collect();
        let tags = b.tags.iter().filter(|t| !t.starts_with("issue=") && !t.starts_with("pr=")).map(|t| format!("#{t}")).collect();
        Header {
            id: b.id.clone(), work, blocked: if b.agent == "blocked" { "blocked".into() } else { String::new() }, changes: if b.agent == "blocked" || base.is_empty() { String::new() } else if b.dirty { "dirty".into() } else { "clean".into() },
            base, tone: if b.dirty { GitTone::Dirty } else { GitTone::Quiet }, diff: b.added + b.deleted > 0, added: b.added, deleted: b.deleted,
            ports, tags,
            // The mock's poll: its local shells report nothing of their own.
            agent: b.agent.clone(), agent_msg: b.agent_msg.clone(),
            title: if b.agent_msg.is_empty() { b.name.clone() } else { format!("{}: {}", b.name, b.agent_msg) },
            ..Default::default()
        }
    }

    fn sandbox(&self, key: &str) -> Option<&Sandbox> { self.boxes.iter().find(|b| b.key == key) }

    /// Where a link pressed in a pane goes, by the core's rules roughly: a discobox link to the
    /// box whose ID it names or is the only prefix of, and a URL printed in the first box to the
    /// port its header forwards (8080, at 18080) while its header is followed; anything else as
    /// written.
    fn follow(&self, f: &crate::wire::Follow) -> CommandResult {
        if let Some(rest) = f.uri.strip_prefix("discobox:///") {
            let found: Vec<&Sandbox> = self.boxes.iter().filter(|b| b.id == rest || b.id.starts_with(rest)).collect();
            return match found.as_slice() {
                [b] => CommandResult { status: Status::Enter, r#box: b.key.clone(), ..Default::default() },
                _ => CommandResult { status: Status::Failed, why: format!("{:?}: no discobox {rest:?} on this server", f.uri), ..Default::default() },
            };
        }
        let mut url = f.uri.clone();
        if self.boxes.first().is_some_and(|b| b.key == f.r#box) && self.headers.values().any(|k| *k == f.r#box) {
            for host in ["localhost", "127.0.0.1", "0.0.0.0"] {
                if let Some(rest) = f.uri.strip_prefix(&format!("http://{host}:8080")).filter(|r| r.is_empty() || r.starts_with(['/', '?', '#'])) {
                    url = format!("http://localhost:18080{rest}");
                }
            }
        }
        CommandResult { status: Status::Done, url, ..Default::default() }
    }

    /// A box's issue or pull request as last read, read on first use.
    fn issue(&mut self, key: &str, pull: bool) -> Option<&mut Issue> {
        let n = work_number(self.sandbox(key)?, pull)?;
        Some(self.issues.entry((key.to_string(), pull)).or_insert_with(|| mock_issue(n, pull)))
    }

    /// Sends one issue subscription what it follows, or why there is none, as the bridge does.
    fn send_work(&mut self, sub: u32, key: &str, pull: bool) {
        let r = match self.issue(key, pull) {
            Some(i) => Ok(i.clone()),
            None if pull => Err("no pull request to show — tag this discobox pr=N, on a GitHub repository".into()),
            None => Err("no issue to show — tag this discobox issue=N, on a GitHub repository".into()),
        };
        self.inbox.push(Event::Issue(sub, r));
    }

    /// Sends every subscription following an issue it again, as the bridge reads it again after
    /// a command here changed it.
    fn resend_work(&mut self, key: &str, pull: bool) {
        let subs: Vec<u32> = self.issue_subs.iter().filter(|(_, w)| w.0 == key && w.1 == pull).map(|(s, _)| *s).collect();
        for sub in subs {
            self.send_work(sub, key, pull);
        }
    }

    /// The issue and pr commands: the tab, or an act on what it shows. A close or reopen and a
    /// merge are questions first, as the core asks them, and the result waits on the answer
    /// (`answered`): none here.
    fn work(&mut self, c: &Command) -> Option<CommandResult> {
        let key = c.targets.first().cloned().unwrap_or_default();
        let pull = c.command == "pr";
        let fail = |why: String| Some(CommandResult { status: Status::Failed, why, ..Default::default() });
        let Some(issue) = self.issue(&key, pull).cloned() else { return fail("no matching repository tag".into()) };
        let n = issue.number;
        match c.args.act {
            None => Some(CommandResult { status: Status::Enter, r#box: key, work: Some(Link { label: issue.label, url: issue.url, command: String::new() }), ..Default::default() }),
            Some(IssueAct::Comment) if c.args.comment.trim().is_empty() => fail("nothing to post — the comment is empty".into()),
            Some(IssueAct::Comment) => {
                let i = self.issue(&key, pull).unwrap();
                i.comments += 1;
                i.timeline.push(mock_item(IssueItemKind::Comment, "you", "just now", "", &c.args.comment));
                self.resend_work(&key, pull);
                Some(CommandResult { status: Status::Done, r#box: key, note: format!("commented on #{n}"), ..Default::default() })
            }
            Some(IssueAct::State) if issue.pull_request.as_ref().is_some_and(|p| p.merged) => fail(format!("PR #{n} is merged — there is nothing to close or reopen")),
            Some(IssueAct::State) => {
                let id = self.next_ask;
                self.next_ask += 1;
                let choice = |key: &str, label: &str, hint: &str| Choice { key: key.into(), label: label.into(), hint: hint.into(), ..Default::default() };
                let q = match (issue.state.as_str(), pull) {
                    ("closed", _) => Question { title: format!("Reopen #{n}?"), body: issue.title.clone(), ..Default::default() },
                    (_, true) => Question { title: format!("Close #{n} without merging it?"), body: issue.title.clone(), ..Default::default() },
                    _ => Question { title: format!("Close #{n}"), body: issue.title.clone(), choices: vec![
                        choice("completed", "close as completed", "done, fixed, or answered"),
                        choice("not_planned", "close as not planned", "won't fix, can't reproduce, duplicate, or stale"),
                    ], ..Default::default() },
                };
                self.asks.insert(id, (c.clone(), Ask::IssueState(key, pull)));
                self.inbox.push(Event::Question(Question { id, command: c.id, ..q }));
                None
            }
            Some(IssueAct::Merge) => {
                let Some(p) = issue.pull_request.as_ref().filter(|p| !p.merged && issue.state == IssueState::Open) else {
                    return fail(format!("PR #{n} cannot be merged here"));
                };
                let id = self.next_ask;
                self.next_ask += 1;
                let choice = |key: &str, label: &str, hint: &str, why: &str| Choice { key: key.into(), label: label.into(), hint: hint.into(), why: why.into() };
                let q = Question {
                    id, command: c.id, title: format!("Merge #{n} into {}", p.base), body: format!("{}\n\nat {}", issue.title, c.args.head),
                    choices: vec![
                        choice("squash", "squash and merge", "its commits become one commit on the base branch", ""),
                        choice("merge", "create a merge commit", "every commit is kept, joined by a merge commit", "not allowed in this repository"),
                        choice("rebase", "rebase and merge", "every commit is replayed onto the base branch", ""),
                    ],
                    ..Default::default()
                };
                self.asks.insert(id, (c.clone(), Ask::Merge(key)));
                self.inbox.push(Event::Question(q));
                None
            }
        }
    }

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
            "files" if self.sandbox(&target).and_then(|b| work_number(b, true)).is_none() => fail("no matching repository tag"),
            "files" => {
                let number = work_number(self.sandbox(&target).unwrap(), true).unwrap_or_default();
                self.made += 1;
                let exec = Tab { files: number, ..mock_exec(&format!("exec_{}", self.made), TabKind::Files, &format!("files changed #{number}")) };
                let size = |n: i64| u16::try_from(n.max(1)).unwrap_or(u16::MAX);
                if let Err(e) = self.spawn(&target, &exec, c.args.stream, size(c.args.cols), size(c.args.rows)) {
                    return fail(&e);
                }
                CommandResult { status: Status::Done, r#box: target, session: Some(exec), ..Default::default() }
            }
            "review config" if !running => fail("cannot review config: takes one running box"),
            "review config" => self.review(&c, target),
            // A folder dropped on the composer: checked, roughly as the creation path checks a
            // source, and where the creates that follow are cut from.
            "options" if !std::path::Path::new(&c.args.source).is_dir() => fail(&format!("{} is not a directory", c.args.source)),
            "options" => {
                self.run.source = c.args.source.clone();
                self.send_chips();
                CommandResult { status: Status::Done, note: format!("new discoboxes are cut from {}", c.args.source), ..Default::default() }
            }
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
        let choice = |key: &str, label: &str, hint: &str| Choice { key: key.into(), label: label.into(), hint: hint.into(), ..Default::default() };
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
            // The core's `ThemeQuestion`: every appearance, the one drawn now offered but closed.
            "theme" => {
                let choices = [(Appearance::System, "System", "follow the system's light or dark appearance"), (Appearance::Dark, "Dark", "dark, whatever the system's"), (Appearance::Light, "Light", "light, whatever the system's")]
                    .into_iter()
                    .map(|(a, label, hint)| Choice { why: if a == self.appearance { "the window is drawn this way now".into() } else { String::new() }, ..choice(a.as_str(), label, hint) })
                    .collect();
                let body = "Draw the window dark, light, or as the system does.".into();
                (Ask::Theme, Event::Question(Question { id, command: c.id, title: "Theme".into(), body, choices, ..Default::default() }))
            }
            // A harness's own way out is the window's, as the core's `LeavesWindow` says: no card.
            "new" if matches!(c.args.prompt.trim(), "/exit" | "/quit") => return None,
            "new" => (Ask::Create(c.args.prompt.clone()), Event::Form(self.run_form(id, c, true, &HashMap::new()))),
            "options" if c.args.source.is_empty() => (Ask::Options, Event::Form(self.run_form(id, c, false, &HashMap::new()))),
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
            Ask::Explain(sandbox) if get("key") == "review" => CommandResult { id: command, ..self.review(&c, sandbox) },
            Ask::Explain(_) => canceled,
            Ask::Theme => {
                // What names no appearance is the system's, as the core's `ParseAppearance` reads it.
                let a = [Appearance::Dark, Appearance::Light].into_iter().find(|a| a.as_str() == get("key")).unwrap_or_default();
                self.appearance = a;
                self.record.lock().unwrap().appearances.push(a);
                CommandResult { id: command, status: Status::Done, appearance: Some(a), ..Default::default() }
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
            Ask::IssueState(key, pull) => {
                let Some(issue) = self.issues.get_mut(&(key.clone(), pull)) else { return };
                let n = issue.number;
                let note = match (issue.state.as_str(), get("key").as_str()) {
                    ("open", "completed" | "not_planned") if !pull => {
                        let how = get("key");
                        issue.state = IssueState::Closed;
                        issue.badge = if how == "not_planned" { "Closed as not planned".into() } else { "Closed".into() };
                        issue.mark = mark(false, IssueTone::Shut);
                        format!("closed #{n}{}", if how == "not_planned" { " as not planned" } else { " as completed" })
                    }
                    ("open", "yes") if pull => {
                        issue.state = IssueState::Closed;
                        issue.badge = "Closed".into();
                        issue.mark = mark(false, IssueTone::Shut);
                        if let Some(p) = issue.pull_request.as_mut() {
                            p.merge = None;
                        }
                        format!("closed #{n}")
                    }
                    ("closed", "yes") => {
                        issue.state = IssueState::Open;
                        issue.badge = "Open".into();
                        issue.mark = mark(true, IssueTone::Open);
                        format!("reopened #{n}")
                    }
                    _ => {
                        self.asks.remove(&id);
                        self.inbox.push(Event::Result(canceled));
                        return;
                    }
                };
                self.resend_work(&key, pull);
                done(note)
            }
            Ask::Merge(key) => {
                let method = get("key");
                let Some(issue) = self.issues.get_mut(&(key.clone(), true)) else { return };
                if !matches!(method.as_str(), "squash" | "rebase") {
                    self.asks.remove(&id);
                    self.inbox.push(Event::Result(canceled));
                    return;
                }
                let n = issue.number;
                issue.state = IssueState::Closed;
                issue.badge = "Merged".into();
                issue.mark = mark(true, IssueTone::Merged);
                if let Some(p) = issue.pull_request.as_mut() {
                    p.merged = true;
                    p.merged_by = "you".into();
                    p.merged_ago = "just now".into();
                    p.merge = None;
                }
                issue.timeline.push(mock_item(IssueItemKind::Event, "you", "just now", "merged commit 8be21c0 into main", ""));
                self.resend_work(&key, true);
                done(format!("merged #{n} ({method})"))
            }
            Ask::Edit(..) | Ask::ToolEdit(..) => return,
            Ask::Secret(..) if draft => return,
            Ask::Secret(SecretAction::New, ..) if get("name").is_empty() => {
                let errors = vec![FieldError { field: "name".into(), why: "a secret needs a name".into() }];
                self.inbox.push(Event::FieldErrors(FieldErrors { form: id, errors }));
                return;
            }
            Ask::Secret(action, secret, binding) => CommandResult { id: command, ..self.secret_answered(action, &secret, &binding, &get) },
            Ask::Request(..) if draft => return,
            Ask::Request(request, page) => {
                // A card answered with none of its actions is refused on the card, as the core
                // refuses it.
                let Some(action) = self.actions_taken.remove(&id) else {
                    let errors = vec![FieldError { field: String::new(), why: "answer with one of the form's actions".into() }];
                    self.inbox.push(Event::FieldErrors(FieldErrors { form: id, errors }));
                    return;
                };
                CommandResult { id: command, ..self.request_answered(&request, action, page) }
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
                self.headers.insert(sub, r#box);
            }
            Out::Subscribe(Subscribe { sub, topic: Topic::Composer, .. }) => {
                self.composer_sub = sub;
                self.send_chips();
            }
            Out::Subscribe(Subscribe { sub, topic: Topic::Requests, .. }) => {
                self.requests_sub = sub;
                self.send_waiting();
            }
            Out::Subscribe(Subscribe { sub, topic: Topic::Provisioning, r#box, .. }) => {
                let step = self.sandbox(&r#box).map(|b| b.provisioning.clone()).unwrap_or_default();
                self.inbox.push(Event::Provisioning(sub, Ok(Provisioning { step })));
            }
            Out::Subscribe(Subscribe { sub, topic: Topic::Issue, r#box, issue, .. }) => {
                let pull = issue.unwrap_or_default().pull;
                self.issue_subs.insert(sub, (r#box.clone(), pull));
                self.send_work(sub, &r#box, pull);
            }
            Out::Draft(prompt) => self.record.lock().unwrap().drafts.push(prompt),
            Out::Follow(f) => {
                let r = self.follow(&f);
                self.inbox.push(Event::Result(CommandResult { id: f.id, ..r }));
            }
            Out::Upload(u) => {
                let r = mock_upload(&u);
                self.record.lock().unwrap().uploads.push(u);
                self.inbox.push(Event::Result(r));
            }
            Out::Unsubscribe(sub) => {
                self.record.lock().unwrap().pushing.remove(&sub);
                self.following.remove(&sub);
                self.headers.remove(&sub);
                self.issue_subs.remove(&sub);
                self.audit_subs.remove(&sub);
            }
            // As the bridge does: what the timeline says before any read, then what the read
            // brought, under the search; one made while another follows the box is answered from
            // what was read, at once.
            Out::Subscribe(Subscribe { sub, topic: Topic::Audit, r#box, audit, .. }) => {
                if !self.audit_subs.values().any(|b| *b == r#box) {
                    let reading = Audit { note: "reading the audit trails…".into(), ..Default::default() };
                    self.inbox.push(Event::Audit(sub, Ok(reading)));
                }
                self.audit_subs.insert(sub, r#box);
                let timeline = match audit {
                    Some(q) => mock_audit_search(&q.text),
                    None => mock_audit(),
                };
                self.inbox.push(Event::Audit(sub, Ok(timeline)));
            }
            // As the bridge does: a first snapshot that says nothing, then one whenever a look
            // says something, which the mock's boxes never have to.
            Out::Subscribe(Subscribe { sub, topic: Topic::Push, r#box, .. }) => {
                self.record.lock().unwrap().pushing.insert(sub, r#box);
                self.inbox.push(Event::Push(sub, Push::default()));
            }
            Out::Subscribe(_) => {}
            Out::Command(c) if c.command == "harnesses" => {
                if let Some(r) = self.harness(&c) {
                    self.inbox.push(Event::Result(CommandResult { id: c.id, ..r }));
                }
            }
            Out::Command(c) if c.command == "audit" => {
                let r = mock_audit_command(&c);
                self.inbox.push(Event::Result(CommandResult { id: c.id, ..r }));
            }
            Out::Command(c) if matches!(c.command.as_str(), "issue" | "pr") => {
                if let Some(r) = self.work(&c) {
                    self.inbox.push(Event::Result(CommandResult { id: c.id, ..r }));
                }
            }
            Out::Command(c) if c.command == "tools" => {
                if let Some(r) = self.tools(&c) {
                    self.inbox.push(Event::Result(CommandResult { id: c.id, ..r }));
                }
            }
            Out::Command(c) if c.command == "credentials" => {
                if let Some(r) = self.credentials(&c) {
                    self.inbox.push(Event::Result(CommandResult { id: c.id, ..r }));
                }
            }
            // A box's oldest request answered through its card; a refused credential's remedy,
            // which the mock has none of.
            Out::Command(c) if c.command == "requests" => {
                let request = self.requests.iter().find(|r| c.targets.first() == Some(&r.r#box)).map(|r| r.id.clone());
                let r = match request {
                    Some(request) => self.ask_request(&c, &request, false),
                    None => Some(CommandResult { status: Status::Failed, why: "nothing waiting on this discobox".into(), ..Default::default() }),
                };
                if let Some(r) = r {
                    self.inbox.push(Event::Result(CommandResult { id: c.id, ..r }));
                }
            }
            Out::Command(c) if c.command == "rejections" => {
                self.inbox.push(Event::Result(CommandResult { id: c.id, status: Status::Failed, why: "nothing refused on this discobox".into(), ..Default::default() }));
            }
            // The notice of the box whose workspace is on screen taken down, until what it is
            // about changes.
            Out::Command(c) if c.command == "dismiss" => {
                let shown = self.waiting().notices.iter().any(|n| n.r#box == c.context.workspace);
                let r = if shown {
                    self.dismissed.push(c.context.workspace.clone());
                    self.send_waiting();
                    CommandResult { id: c.id, status: Status::Done, ..Default::default() }
                } else {
                    CommandResult { id: c.id, status: Status::Failed, why: "no banner to dismiss".into(), ..Default::default() }
                };
                self.inbox.push(Event::Result(r));
            }
            Out::Saved(v) => self.saved(v),
            Out::Command(c) if c.command == "review config" && c.args.explain => self.explain(&c),
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
                if let Some(action) = f.action {
                    self.actions_taken.insert(f.form, action);
                }
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

/// Where an upload's images land, as the core names them roughly: the clipboard's image, or
/// files dropped when every one is an image by its name (the CLI reads them, and their bytes say
/// so too); a drop that is not all images, done with no paths.
fn mock_upload(u: &Upload) -> CommandResult {
    let image = |f: &String| ["png", "jpg", "jpeg", "gif", "webp"].iter().any(|e| f.to_ascii_lowercase().ends_with(&format!(".{e}")));
    let name = |f: &str| f.rsplit(['/', '\\']).next().unwrap_or(f).to_string();
    let paths = if u.files.is_empty() {
        vec![format!("/tmp/discobox-paste/{}", name(&u.name))]
    } else if u.files.iter().all(image) {
        u.files.iter().map(|f| format!("/tmp/discobox-paste/{}", name(f))).collect()
    } else {
        vec![]
    };
    CommandResult { id: u.id, status: Status::Done, paths, ..Default::default() }
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
        let tool = exec.kind == TabKind::Tool;
        std::thread::spawn(move || {
            let mut buf = [0u8; 16 * 1024];
            loop {
                match reader.read(&mut buf) {
                    Ok(0) | Err(_) => {
                        // What the ending does, as the core's Tab.End says it: the primary takes the
                        // workspace with it, a tool its tab, and anything else that finished is held.
                        let ended = if primary {
                            Stream { stream, state: StreamState::Ended, end: Some(TabEnd::Workspace), ..Default::default() }
                        } else if tool {
                            Stream { stream, state: StreamState::Ended, end: Some(TabEnd::Tab), ..Default::default() }
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
        let Some(Event::Form(f)) = read(r#"{"type":"form","form":{"id":8,"command":4,"title":"Rename","fields":[{"key":"name","label":"name","choices":[],"value":"one","applies":true}],"card":[],"actions":[{"key":"deny","label":"Deny","declines":true}]}}"#) else { panic!("form") };
        assert_eq!((f.id, f.command, f.fields[0].key.as_str(), f.fields[0].applies), (8, 4, "name", true));
        assert_eq!((f.actions[0].key, f.actions[0].declines), (CardAction::Deny, true));
        let Some(Event::FieldErrors(e)) = read(r#"{"type":"fieldErrors","fieldErrors":{"form":8,"errors":[{"field":"name","why":"a discobox needs a configured name"}]}}"#) else { panic!("fieldErrors") };
        assert_eq!((e.form, e.errors[0].field.as_str()), (8, "name"));
        let Some(Event::Question(q)) = read(r#"{"type":"question","question":{"id":9,"command":4,"title":"Purge","body":"Purge one?","choices":[],"defaultNo":true}}"#) else { panic!("question") };
        assert!(q.default_no && q.choices.is_empty());
        let Some(Event::Requests(10, Ok(w))) = read(r#"{"type":"snapshot","snapshot":{"sub":10,"topic":"requests","requests":{"requests":[{"id":"r1","box":"b1","kind":"credential","created":"2026-11-09T12:00:00Z","subject":"github","requester":"agent · b1"}],"rejections":[],"notices":[{"box":"b1","kind":"request","subject":"credential request","call":"click to answer","command":"requests","urgent":true}],"arrived":[{"request":"r1","box":"b1","title":"Credential request","body":"one · github"}],"said":[{"text":"renewed npm"}]}}}"#) else { panic!("requests") };
        assert_eq!((w.notices[0].kind, w.notices[0].urgent, w.arrived[0].request.as_str(), w.said[0].text.as_str()), (NoticeKind::Request, true, "r1", "renewed npm"));
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
