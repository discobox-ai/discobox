//! The bridge's contract suite (ADR 26-11-07-289 §4): this crate's client, the one the app talks
//! through, against the CLI's real bridge (`cli/internal/console/bridge`) serving a fake of the
//! console core, over a pipe each way.
//!
//! The CLI's end is that package's `TestContractCore`, built once by `go test -c` and run as a
//! process per test; what its boxes do is described there. Each message either way is the
//! generated types', so what one side writes and the other cannot read fails here. It covers
//! every message kind the bridge carries but `edit`, which nothing the core runs sends yet.

use std::collections::{HashMap, VecDeque};
use std::path::{Path, PathBuf};
use std::process::{Child, Command as Process, Stdio};
use std::sync::OnceLock;
use std::sync::mpsc::{Receiver, RecvTimeoutError, channel};
use std::time::{Duration, Instant};

use discobox_bridge::wire::{
    Answer, Args, Close, Command, Context, Message, Open, PROTOCOL, RequestKind, SPEC_VERSION, Saved, State, Status,
    StreamState, Submit, Subscribe, TabKind, Topic, Type, Unsubscribe, Upload, Welcome,
};
use discobox_bridge::{Client, Incoming};

/// How long anything is waited for before the test fails.
const PATIENCE: Duration = Duration::from_secs(20);

/// The CLI's end: `TestContractCore` in the Go package's test binary, built once for the suite.
fn fake_core() -> &'static Path {
    static BUILT: OnceLock<PathBuf> = OnceLock::new();
    BUILT.get_or_init(|| {
        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("../..");
        let out = Path::new(env!("CARGO_TARGET_TMPDIR")).join(format!("bridge-fake-core{}", std::env::consts::EXE_SUFFIX));
        let status = Process::new("go")
            .current_dir(&root)
            .args(["test", "-c", "-o"])
            .arg(&out)
            .arg("./cli/internal/console/bridge")
            .status()
            .expect("go, to build the CLI's end of the bridge");
        assert!(status.success(), "building the CLI's end of the bridge: {status}");
        out
    })
}

/// A CLI on the other end of a connected client, and everything it sent that a test has not
/// taken yet.
struct Cli {
    process: Child,
    client: Client,
    welcome: Welcome,
    rx: Receiver<Incoming>,
    backlog: VecDeque<Incoming>,
    next_id: u64,
}

impl Cli {
    fn start(scenario: &str) -> Cli {
        let mut process = Process::new(fake_core())
            .arg("-test.run=^TestContractCore$")
            .env("DISCOBOX_BRIDGE_CONTRACT", scenario)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .expect("the CLI's end of the bridge");
        let input = process.stdout.take().unwrap();
        let output = process.stdin.take().unwrap();
        let (tx, rx) = channel();
        let (client, welcome) = Client::connect(input, output, move |i| {
            let _ = tx.send(i);
        })
        .expect("connect");
        Cli { process, client, welcome, rx, backlog: VecDeque::new(), next_id: 0 }
    }

    fn send(&self, m: Message) { self.client.send(&m).expect("send"); }

    fn command(&mut self, command: &str, targets: &[&str], context: Context, args: Args) -> u64 {
        self.next_id += 1;
        let id = self.next_id;
        let targets = targets.iter().map(|t| t.to_string()).collect();
        self.send(Message {
            r#type: Type::Command,
            command: Some(Command { id, command: command.into(), targets, context, args }),
            ..Default::default()
        });
        id
    }

    fn subscribe(&self, sub: u32, topic: Topic, sandbox: &str) {
        self.send(Message {
            r#type: Type::Subscribe,
            subscribe: Some(Subscribe { sub, topic, r#box: sandbox.into() }),
            ..Default::default()
        });
    }

    fn answer(&self, question: u64, key: &str, declined: bool) {
        self.send(Message {
            r#type: Type::Answer,
            answer: Some(Answer { question, key: key.into(), declined }),
            ..Default::default()
        });
    }

    fn submit(&self, form: u64, answers: &[(&str, &str)], draft: bool, cancel: bool) {
        let answers: HashMap<String, String> = answers.iter().map(|(k, v)| (k.to_string(), v.to_string())).collect();
        self.send(Message { r#type: Type::Submit, submit: Some(Submit { form, answers, cancel, draft }), ..Default::default() });
    }

    /// The first thing the CLI sent that `want` takes, in order; what it passes over is kept.
    fn take<T>(&mut self, what: &str, mut want: impl FnMut(&Incoming) -> Option<T>) -> T {
        if let Some(i) = self.backlog.iter().position(|i| want(i).is_some()) {
            return want(&self.backlog.remove(i).unwrap()).unwrap();
        }
        let deadline = Instant::now() + PATIENCE;
        loop {
            let left = deadline.saturating_duration_since(Instant::now());
            match self.rx.recv_timeout(left) {
                Ok(i) => {
                    if let Some(t) = want(&i) {
                        return t;
                    }
                    self.backlog.push_back(i);
                }
                Err(RecvTimeoutError::Timeout) => panic!("never arrived: {what}; arrived instead: {:?}", self.backlog),
                Err(RecvTimeoutError::Disconnected) => panic!("the client stopped before {what}"),
            }
        }
    }

    /// The next message of a type.
    fn expect(&mut self, t: Type) -> Message {
        self.take(&format!("a {t:?}"), |i| match i {
            Incoming::Message(m) if m.r#type == t => Some(m.clone()),
            _ => None,
        })
    }

    /// The result of one command.
    fn result(&mut self, id: u64) -> discobox_bridge::wire::Result {
        self.take(&format!("the result of {id}"), |i| match i {
            Incoming::Message(Message { result: Some(r), .. }) if r.id == id => Some(r.clone()),
            _ => None,
        })
    }

    /// A stream's output, as lines, up to its end; and why it ended.
    fn output(&mut self, stream: u32) -> (Vec<String>, String) {
        let mut out = Vec::new();
        loop {
            let next = self.take(&format!("output on {stream}"), |i| match i {
                Incoming::Output(s, b) if *s == stream => Some(Ok(b.clone())),
                Incoming::Exited(s, why) if *s == stream => Some(Err(why.clone())),
                _ => None,
            });
            match next {
                Ok(bytes) => out.extend(bytes),
                Err(why) => return (String::from_utf8_lossy(&out).lines().map(String::from).collect(), why),
            }
        }
    }

    /// Keeps everything that arrives within a while.
    fn settle(&mut self, within: Duration) {
        let deadline = Instant::now() + within;
        while let Ok(i) = self.rx.recv_timeout(deadline.saturating_duration_since(Instant::now())) {
            self.backlog.push_back(i);
        }
    }

    /// Fails if a message of the type arrives within a while.
    fn quiet(&mut self, t: Type, within: Duration) {
        self.settle(within);
        assert!(!self.backlog.iter().any(|i| matches!(i, Incoming::Message(m) if m.r#type == t)), "a {t:?} arrived: {:?}", self.backlog);
    }
}

impl Drop for Cli {
    fn drop(&mut self) {
        let _ = self.process.kill();
        let _ = self.process.wait();
    }
}

fn pane(workspace: &str) -> Context {
    Context { workspace: workspace.into(), has_workspace: true, workspace_shown: true, pane_focused: true, ..Default::default() }
}

// The welcome carries this side's protocol, the console's spec in the shape the app reads, and
// the session the window is opened on.
#[test]
fn the_welcome_carries_the_spec_and_the_session() {
    let cli = Cli::start("default");
    assert_eq!(cli.welcome.protocol, PROTOCOL);
    let spec = cli.welcome.spec.as_ref().expect("a spec");
    assert_eq!(spec.version, SPEC_VERSION);
    assert!(spec.command("attach").is_some_and(|c| c.lifecycle));
    let session = cli.welcome.session.as_ref().expect("a session");
    assert_eq!(
        (session.primary_server.as_str(), session.project.as_str(), session.directory.as_str(), session.branch.as_str()),
        ("home", "proj", "/src", "main")
    );
}

// Each topic is sent whole on subscribe, and the listing again when a command changes it, until
// it is unsubscribed.
#[test]
fn subscriptions_send_snapshots_until_unsubscribed() {
    let mut cli = Cli::start("default");
    cli.subscribe(1, Topic::Listing, "");
    let listing = cli.expect(Type::Snapshot).snapshot.unwrap();
    assert_eq!((listing.sub, listing.topic), (1, Topic::Listing));
    let boxes = listing.listing.unwrap().boxes;
    let b1 = boxes.iter().find(|b| b.key == "b1").expect("b1 listed");
    assert!(b1.running() && b1.attachable);

    cli.subscribe(2, Topic::Sessions, "b1");
    let sessions = cli.expect(Type::Snapshot).snapshot.unwrap();
    assert_eq!(sessions.sub, 2);
    let tabs: Vec<_> = sessions.sessions.unwrap().sessions.into_iter().map(|t| (t.kind, t.exec)).collect();
    assert_eq!(tabs, [(TabKind::Primary, "primary".to_string()), (TabKind::Shell, "sh".to_string())]);

    cli.subscribe(3, Topic::Resources, "");
    let resources = cli.expect(Type::Snapshot).snapshot.unwrap().resources.expect("resources");
    assert!(resources.known && resources.cpu_capacity == 8.0);

    cli.subscribe(4, Topic::Requests, "");
    let requests = cli.expect(Type::Snapshot).snapshot.unwrap().requests.expect("requests");
    assert_eq!(requests.requests.len(), 1);
    assert_eq!((requests.requests[0].r#box.as_str(), requests.requests[0].kind), ("b1", RequestKind::Credential));

    let id = cli.command("archive", &["b1"], Context::default(), Args::default());
    assert_eq!(cli.result(id).status, Status::Done);
    let after = cli.take("the listing again", |i| match i {
        Incoming::Message(Message { snapshot: Some(s), .. }) if s.sub == 1 => s.listing.clone(),
        _ => None,
    });
    assert_eq!(after.boxes.iter().find(|b| b.key == "b1").unwrap().state, State::Archived);

    for sub in [1, 2, 3, 4] {
        cli.send(Message { r#type: Type::Unsubscribe, unsubscribe: Some(Unsubscribe { sub }), ..Default::default() });
    }
    // Commands are answered in order of their results, which go out after the unsubscribes
    // are read; a relist after this one has nobody to send to.
    let id = cli.command("refresh", &[], Context::default(), Args::default());
    cli.result(id);
    cli.backlog.clear();
    let id = cli.command("unarchive", &["b1"], Context::default(), Args::default());
    assert_eq!(cli.result(id).status, Status::Done);
    cli.quiet(Type::Snapshot, Duration::from_millis(500));
}

// A command ends in its result: done, with what it made; the app's flow to enter, with the
// core's entries; refused, with why. An upload is answered by its ID, and `saved`, which answers
// nothing yet, leaves the bridge serving.
#[test]
fn commands_and_uploads_end_in_results() {
    let mut cli = Cli::start("default");
    let id = cli.command("refresh", &[], Context::default(), Args::default());
    assert_eq!(cli.result(id).status, Status::Done);

    let id = cli.command("actions", &["b1"], Context { list_has_keys: true, ..Default::default() }, Args::default());
    let r = cli.result(id);
    assert_eq!(r.status, Status::Enter);
    let stop = r.entries.iter().find(|e| e.command == "stop").expect("stop is an action");
    let purge = r.entries.iter().find(|e| e.command == "purge").expect("purge is an action");
    assert!(stop.enabled && stop.why.is_empty());
    assert!(!purge.enabled && !purge.why.is_empty());

    let id = cli.command("launch rockets", &[], Context::default(), Args::default());
    let r = cli.result(id);
    assert_eq!(r.status, Status::Failed);
    assert!(r.why.contains("launch rockets"), "{}", r.why);

    let id = cli.command("stop", &["nobody"], Context::default(), Args::default());
    let r = cli.result(id);
    assert_eq!(r.status, Status::Failed);
    assert!(r.why.contains("nobody"), "{}", r.why);

    cli.send(Message { r#type: Type::Saved, saved: Some(Saved { edit: 1, error: String::new() }), ..Default::default() });
    // [1, 2, 3], as Go writes bytes in JSON.
    let upload = Upload { id: 99, r#box: "b1".into(), name: "shot.png".into(), data: "AQID".into() };
    cli.send(Message { r#type: Type::Upload, upload: Some(upload), ..Default::default() });
    let r = cli.result(99);
    assert_eq!((r.status, r.path.as_str()), (Status::Done, "/tmp/shot.png"));
}

// A form is the core's: a draft is answered with the form again, a refusal with field errors
// while the form stays up, an accepted answer with the result; a dismissed one cancels.
#[test]
fn forms_take_drafts_refusals_and_answers() {
    let mut cli = Cli::start("default");
    let id = cli.command("settings", &["b1"], Context::default(), Args::default());
    let form = cli.expect(Type::Form).form.unwrap();
    assert_eq!(form.command, id);
    let name = form.fields.iter().find(|f| f.key == "name").expect("a name field");
    assert!(name.required && name.applies && name.value == "b1", "{name:?}");

    cli.submit(form.id, &[("name", " ")], true, false);
    assert_eq!(cli.expect(Type::Form).form.unwrap().id, form.id);

    cli.submit(form.id, &[("name", "")], false, false);
    let errors = cli.expect(Type::FieldErrors).field_errors.unwrap();
    assert_eq!(errors.form, form.id);
    assert_eq!(errors.errors.len(), 1);
    assert_eq!(errors.errors[0].field, "name");

    cli.submit(form.id, &[("name", "second")], false, false);
    assert_eq!(cli.result(id).status, Status::Done);

    let id = cli.command("rename", &["b1"], Context::default(), Args::default());
    let form = cli.expect(Type::Form).form.unwrap();
    cli.submit(form.id, &[], false, true);
    assert_eq!(cli.result(id).status, Status::Canceled);
}

// A running command's report arrives as progress and its question as a question; a key answers
// it, and a declined question cancels what asked it.
#[test]
fn progress_and_questions_reach_the_app() {
    let mut cli = Cli::start("default");
    let id = cli.command("start", &["b3"], Context::default(), Args::default());
    let progress = cli.expect(Type::Progress).progress.unwrap();
    assert_eq!((progress.command, progress.text.as_str(), progress.warning), (id, "pulling", false));
    let q = cli.expect(Type::Question).question.unwrap();
    assert_eq!((q.command, q.title.as_str()), (id, "Git credentials"));
    let keys: Vec<_> = q.choices.iter().map(|c| c.key.as_str()).collect();
    assert_eq!(keys, ["once", "never"]);
    cli.answer(q.id, "once", false);
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str()), (Status::Done, "chose once"));

    let id = cli.command("purge", &["b2"], Context::default(), Args::default());
    let q = cli.expect(Type::Question).question.unwrap();
    assert!(q.default_no && q.choices.is_empty() && q.body.contains("b2"), "{q:?}");
    cli.answer(q.id, "", true);
    assert_eq!(cli.result(id).status, Status::Canceled);
}

// A question asked while another command is in flight is answered on its own, and the command
// in flight goes on to its own result after it.
#[test]
fn a_question_while_another_command_is_in_flight() {
    let mut cli = Cli::start("default");
    let slow = cli.command("stop", &["slow"], Context::default(), Args::default());
    let purge = cli.command("purge", &["b2"], Context::default(), Args::default());
    let q = cli.expect(Type::Question).question.unwrap();
    assert_eq!(q.command, purge);
    assert!(
        !cli.backlog.iter().any(|i| matches!(i, Incoming::Message(Message { result: Some(r), .. }) if r.id == slow)),
        "the slow stop ended before the question was answered"
    );
    cli.answer(q.id, "yes", false);
    assert_eq!(cli.result(purge).status, Status::Done);
    let r = cli.result(slow);
    assert_eq!((r.status, r.note.as_str()), (Status::Done, "stopped after the purge"));
}

// Frames the app sends on a stream before its open is read, and while it opens, reach the
// session in order once it attaches; its output and its end come back on the stream.
#[test]
fn early_stream_frames_are_kept_until_the_stream_attaches() {
    let mut cli = Cli::start("early");
    cli.client.resize(5, 90, 30).unwrap();
    cli.client.input(5, b"ls\r").unwrap();
    let open = Open { stream: 5, r#box: "b1".into(), exec: "sh".into(), cols: 80, rows: 24 };
    cli.send(Message { r#type: Type::Open, open: Some(open), ..Default::default() });
    cli.client.input(5, b"pwd\r").unwrap();
    cli.client.repaint(5).unwrap();
    // Every open waits until b3 is started: by then all of the above was read.
    let start = cli.command("start", &["b3"], Context::default(), Args::default());
    let stream = cli.expect(Type::Stream).stream.unwrap();
    assert_eq!((stream.stream, stream.state), (5, StreamState::Attached));
    let q = cli.expect(Type::Question).question.unwrap();
    cli.answer(q.id, "never", false);
    cli.result(start);

    cli.client.input(5, b"exit\r").unwrap();
    let (lines, why) = cli.output(5);
    assert_eq!(lines, ["resize 90x30", "input ls", "input pwd", "repaint", "input exit"]);
    assert_eq!(why, "");
}

// A stream that cannot open fails, with why; one the app closes is detached, and the bridge
// goes on serving the others.
#[test]
fn streams_fail_and_close() {
    let mut cli = Cli::start("default");
    let missing = Open { stream: 1, r#box: "b1".into(), exec: "missing".into(), cols: 80, rows: 24 };
    cli.send(Message { r#type: Type::Open, open: Some(missing), ..Default::default() });
    let s = cli.expect(Type::Stream).stream.unwrap();
    assert_eq!((s.stream, s.state), (1, StreamState::Failed));
    assert!(s.why.contains("no such session"), "{}", s.why);

    let open = Open { stream: 2, r#box: "b1".into(), exec: "sh".into(), cols: 80, rows: 24 };
    cli.send(Message { r#type: Type::Open, open: Some(open), ..Default::default() });
    let s = cli.expect(Type::Stream).stream.unwrap();
    assert_eq!((s.stream, s.state), (2, StreamState::Attached));
    cli.client.input(2, b"hi\r").unwrap();
    assert_eq!(cli.take("the echo", |i| match i {
        Incoming::Output(2, b) => Some(b.clone()),
        _ => None,
    }), b"input hi\r\n");
    cli.send(Message { r#type: Type::Close, close: Some(Close { stream: 2 }), ..Default::default() });
    // Detached: what is typed into it reaches nothing, and it is not ended either.
    cli.client.input(2, b"after\r").unwrap();

    // A shell made by a command is attached to the command's stream.
    let id = cli.command("shell", &["b1"], pane("b1"), Args { stream: 3, cols: 100, rows: 40, ..Default::default() });
    let r = cli.result(id);
    let session = r.session.expect("the shell's session");
    assert_eq!((r.status, session.kind, session.exec.as_str()), (Status::Done, TabKind::Shell, "sh1"));
    cli.client.input(3, b"exit\r").unwrap();
    let (lines, _) = cli.output(3);
    assert_eq!(lines, ["input exit"]);
    cli.settle(Duration::from_millis(300));
    let after = cli.backlog.iter().filter(|i| matches!(i, Incoming::Output(2, _) | Incoming::Exited(2, _))).count();
    assert_eq!(after, 0, "stream 2 went on after it was closed: {:?}", cli.backlog);
}

// A CLI that exits while a command is in flight closes the client: no result comes, the app is
// told the bridge is gone, and what it sends after fails rather than hangs.
#[test]
fn a_cli_exiting_mid_command_closes_the_client() {
    let mut cli = Cli::start("default");
    let id = cli.command("stop", &["doomed"], Context::default(), Args::default());
    cli.take("the bridge closing", |i| matches!(i, Incoming::Closed).then_some(()));
    assert!(
        !cli.backlog.iter().any(|i| matches!(i, Incoming::Message(Message { result: Some(r), .. }) if r.id == id)),
        "a result arrived for the command the CLI exited in"
    );
    let status = cli.process.wait().unwrap();
    assert_eq!(status.code(), Some(3), "the CLI ended some other way: {status}");
    // The pipe is closed now; a write may still land in its buffer once, but not for long.
    let deadline = Instant::now() + PATIENCE;
    while cli.client.input(1, b"anyone there?").is_ok() {
        assert!(Instant::now() < deadline, "writes to a CLI that exited went on succeeding");
        std::thread::sleep(Duration::from_millis(10));
    }
}
