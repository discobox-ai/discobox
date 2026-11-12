//! The bridge's contract suite (ADR 26-11-07-289 §4): this crate's client, the one the app talks
//! through, against the CLI's real bridge (`cli/internal/console/bridge`) serving a fake of the
//! console core, over a pipe each way.
//!
//! The CLI's end is that package's `TestContractCore`, built once by `go test -c` and run as a
//! process per test; what its boxes do is described there. Each message either way is the
//! generated types', so what one side writes and the other cannot read fails here. It covers
//! every message kind the bridge carries.

use std::collections::{HashMap, VecDeque};
use std::path::{Path, PathBuf};
use std::process::{Child, Command as Process, Stdio};
use std::sync::OnceLock;
use std::sync::mpsc::{Receiver, RecvTimeoutError, channel};
use std::time::{Duration, Instant};

use discobox_bridge::wire::{
    Answer, Appearance, Args, AuditPart, AuditQuery, Close, Command, Context, Draft, Follow, HarnessAction, HarnessChange, CardAction, Message, NoticeKind, Open, PROTOCOL, Push, RequestKind, SPEC_VERSION, Saved, SecretAction, SecretStanding, ServiceVerb, State, Status,
    StreamState, Submit, Subscribe, TabEnd, TabKind, Topic, Type, Unsubscribe, Upload, Welcome, Write, IssueAct, IssueItemKind, IssueQuery, IssueTone,
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
            subscribe: Some(Subscribe { sub, topic, r#box: sandbox.into(), ..Default::default() }),
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
        self.send(Message { r#type: Type::Submit, submit: Some(Submit { form, answers, cancel, draft, ..Default::default() }), ..Default::default() });
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
    assert_eq!(session.appearance, Appearance::Dark, "the welcome lost the theme the last window was picked to draw in");
    assert!(session.start.is_none(), "a window asked for nothing opens on {:?}", session.start);

    // `discobox desktop BOX` opens the window on that box's workspace, named by its key.
    let cli = Cli::start("start");
    let start = cli.welcome.session.as_ref().and_then(|s| s.start.clone()).expect("what the window opens on");
    assert_eq!((start.command.as_str(), start.r#box.as_str()), ("attach", "home/b1"));
}

// The theme command asks the core's question, opening on a change from the palette drawn now, and
// its answer comes back for the window to draw in.
#[test]
fn a_picked_theme_reaches_the_app() {
    let mut cli = Cli::start("default");
    let theme = cli.command("theme", &[], Context::default(), Args::default());
    let q = cli.expect(Type::Question).question.expect("the theme's question");
    let closed: Vec<_> = q.choices.iter().filter(|c| !c.why.is_empty()).map(|c| c.key.as_str()).collect();
    assert_eq!(closed, ["dark"], "{q:?}");
    cli.answer(q.id, "light", false);
    let r = cli.result(theme);
    assert_eq!((r.status, r.appearance), (Status::Done, Some(Appearance::Light)), "{r:?}");
}

/// What b1's push says first, then next, as the app's client reads them.
fn b1_push(scenario: &str) -> (Push, Push) {
    let mut cli = Cli::start(scenario);
    cli.subscribe(1, Topic::Listing, "");
    cli.expect(Type::Snapshot);
    cli.subscribe(2, Topic::Push, "b1");
    let mut said = |what: &str| cli.take(what, |i| match i {
        Incoming::Message(Message { snapshot: Some(s), .. }) if s.sub == 2 => s.push.clone(),
        _ => None,
    });
    (said("the push's first snapshot"), said("what the push's look said"))
}

// A workspace's push is the core's loop, run by the CLI for as long as it is subscribed: its first
// snapshot says nothing, then a look says what moved, or what was refused as a warning, each with
// the words for when the box is not the one on screen.
#[test]
fn a_workspaces_push_says_what_moved_or_was_refused() {
    let (first, moved) = b1_push("default");
    assert_eq!(first, Push::default());
    assert_eq!(
        (moved.said, moved.text.as_str(), moved.warning, moved.elsewhere.as_str()),
        (1, "pushed c1aaaaaaaaaa to origin/main in the discobox", false, "b1: pushed c1aaaaaaaaaa to origin/main in the discobox"),
    );
    let (first, refused) = b1_push("refused");
    assert_eq!(first, Push::default());
    assert_eq!(
        (refused.said, refused.text.as_str(), refused.warning, refused.elsewhere.as_str()),
        (1, "the lease moved", true, "b1: the lease moved"),
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
    let listing = listing.listing.unwrap();
    // The core's list: its rows are the boxes the filter shows, b1 among them.
    assert!(listing.rows.iter().any(|r| r.r#box == "b1"), "b1 is not a row: {:?}", listing.rows);
    let boxes = listing.boxes;
    let b1 = boxes.iter().find(|b| b.key == "b1").expect("b1 listed");
    assert!(b1.running() && b1.attachable);

    cli.subscribe(2, Topic::Sessions, "b1");
    let sessions = cli.expect(Type::Snapshot).snapshot.unwrap();
    assert_eq!(sessions.sub, 2);
    let sessions = sessions.sessions.unwrap().sessions;
    let tabs: Vec<_> = sessions.iter().map(|t| (t.kind, t.exec.clone())).collect();
    let services = ["service:web", "service:migrate", "service:broken"].map(|e| (TabKind::Service, e.to_string()));
    assert_eq!(tabs, [&services[..], &[(TabKind::Primary, "primary".to_string()), (TabKind::Shell, "sh".to_string())]].concat());
    // Which take images and the image keys is the core's: every session but a service's, and the
    // keys on the harness terminal only.
    let images: Vec<_> = sessions.iter().map(|t| (t.exec.as_str(), t.images, t.image_keys)).collect();
    assert_eq!(images, [
        ("service:web", false, false), ("service:migrate", false, false), ("service:broken", false, false),
        ("primary", true, true), ("sh", true, false),
    ]);

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
    // Archived, it leaves the rows (the archived are hidden until asked for) and is counted there.
    assert!(!after.rows.iter().any(|r| r.r#box == "b1") && after.archived >= 1, "rows {:?}, archived {}", after.rows, after.archived);

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

    cli.send(Message { r#type: Type::Saved, saved: Some(Saved { edit: 1, ..Default::default() }), ..Default::default() });
    // [1, 2, 3], as Go writes bytes in JSON.
    let upload = Upload { id: 99, r#box: "b1".into(), name: "shot.png".into(), data: "AQID".into(), ..Default::default() };
    cli.send(Message { r#type: Type::Upload, upload: Some(upload), ..Default::default() });
    let r = cli.result(99);
    assert_eq!(r.status, Status::Done);
    let [path] = r.paths.as_slice() else { panic!("paths {:?}", r.paths) };
    assert!(path.starts_with("/tmp/") && path.ends_with("-0-shot.png"), "{path}: not the core's name for it");

    // Files dropped on a pane go by their paths, which the CLI reads: one that is not an image is
    // answered with no paths, for the app to paste it as text.
    let upload = Upload { id: 100, r#box: "b1".into(), files: vec!["/nowhere/notes.txt".into()], ..Default::default() };
    cli.send(Message { r#type: Type::Upload, upload: Some(upload), ..Default::default() });
    let r = cli.result(100);
    assert_eq!((r.status, r.paths.len()), (Status::Done, 0));
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
    // A picker's choices carry the keys the core gives them, their places.
    let picker = form.fields.iter().find(|f| f.choices.len() > 1).expect("a picker");
    let presses: Vec<_> = picker.choices.iter().map(|c| c.press.as_str()).collect();
    assert_eq!(&presses[..2], ["1", "2"], "{picker:?}");

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

// Text written in the app goes on in the editor: a write is answered with an edit of a file
// that holds it, under a text extension, and once saved with what the file holds; one given up
// is canceled.
#[test]
fn a_write_goes_on_in_the_editor() {
    let mut cli = Cli::start("default");
    let write = |cli: &mut Cli, id: u64| {
        cli.send(Message { r#type: Type::Write, write: Some(Write { id, text: "fix the reaper".into(), what: "the prompt".into() }), ..Default::default() });
        let edit = cli.expect(Type::Edit).edit.unwrap();
        assert_eq!((edit.command, edit.title.as_str()), (id, "Writing the prompt"));
        assert!(edit.path.ends_with(".txt"), "{}", edit.path);
        assert_eq!(std::fs::read_to_string(&edit.path).unwrap(), "fix the reaper");
        std::fs::write(&edit.path, "fix the reaper and its tests\n").unwrap();
        edit
    };
    let edit = write(&mut cli, 900);
    cli.send(Message { r#type: Type::Saved, saved: Some(Saved { edit: edit.id, ..Default::default() }), ..Default::default() });
    let r = cli.result(900);
    assert_eq!((r.status, r.text.as_str()), (Status::Done, "fix the reaper and its tests"));
    let edit = write(&mut cli, 901);
    cli.send(Message { r#type: Type::Saved, saved: Some(Saved { edit: edit.id, canceled: true, ..Default::default() }), ..Default::default() });
    let r = cli.result(901);
    assert_eq!((r.status, r.text.as_str()), (Status::Canceled, ""));
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
    // The keys its answers are pressed by are the core's.
    assert_eq!((q.presses["once"].as_str(), q.presses["never"].as_str()), ("1", "2"));
    cli.answer(q.id, "once", false);
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str()), (Status::Done, "chose once"));

    let id = cli.command("purge", &["b2"], Context::default(), Args::default());
    let q = cli.expect(Type::Question).question.unwrap();
    assert!(q.default_no && q.choices.is_empty() && q.body.contains("b2"), "{q:?}");
    assert_eq!((q.presses["yes"].as_str(), q.presses["no"].as_str()), ("y", "n"));
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
    assert_eq!((s.stream, s.state, s.end), (1, StreamState::Failed, Some(TabEnd::Tab)));
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
    // What its ending does is the core's, said before the stream's last frame: a shell that
    // finished is held with its last screen.
    let ended = cli.take("the shell's ending", |i| match i {
        Incoming::Message(Message { stream: Some(s), .. }) if s.stream == 3 && s.state == StreamState::Ended => Some(s.clone()),
        _ => None,
    });
    assert_eq!((ended.end, ended.status.as_str()), (Some(TabEnd::Held), "finished"));
    cli.settle(Duration::from_millis(300));
    let after = cli.backlog.iter().filter(|i| matches!(i, Incoming::Output(2, _) | Incoming::Exited(2, _))).count();
    assert_eq!(after, 0, "stream 2 went on after it was closed: {:?}", cli.backlog);
}

// A box's services reach the app as the core says them: their tabs among its sessions, each on
// its run and saying its state when it is not running; a tab's stream with the service's card and
// last output; the services command's menu with each service's verbs; and a verb run, or refused
// in the core's words.
#[test]
fn services_reach_the_app() {
    let mut cli = Cli::start("default");
    cli.subscribe(1, Topic::Sessions, "b1");
    let tabs = cli.expect(Type::Snapshot).snapshot.unwrap().sessions.unwrap().sessions;
    let migrate = tabs.iter().find(|t| t.exec == "service:migrate").expect("migrate's tab");
    assert_eq!((migrate.kind, migrate.service.as_str(), migrate.live, migrate.status.as_str()), (TabKind::Service, "migrate", false, "exited"));
    assert!(!migrate.run.is_empty(), "a service's tab carries its run");

    let open = Open { stream: 4, r#box: "b1".into(), exec: "service:migrate".into(), cols: 80, rows: 24 };
    cli.send(Message { r#type: Type::Open, open: Some(open), ..Default::default() });
    let s = cli.expect(Type::Stream).stream.unwrap();
    assert_eq!((s.stream, s.state), (4, StreamState::Attached));
    let mut card = Vec::new();
    while !String::from_utf8_lossy(&card).contains("migrated 3 tables\n") {
        card.extend(cli.take("migrate's card", |i| match i {
            Incoming::Output(4, b) => Some(b.clone()),
            _ => None,
        }));
    }

    let id = cli.command("services", &["b1"], pane("b1"), Args::default());
    let r = cli.result(id);
    assert_eq!((r.status, r.r#box.as_str()), (Status::Enter, "b1"));
    assert_eq!(r.services.iter().map(|v| v.id.as_str()).collect::<Vec<_>>(), ["web", "migrate", "broken"]);
    let broken = &r.services[2];
    assert_eq!(broken.actions.iter().map(|a| a.verb).collect::<Vec<_>>(), [ServiceVerb::Start, ServiceVerb::Stop, ServiceVerb::Restart]);
    assert!(broken.actions.iter().all(|a| !a.enabled && a.why == "it is not executable"), "{:?}", broken.actions);

    let restart = Args { service: "web".into(), verb: Some(ServiceVerb::Restart), ..Default::default() };
    let id = cli.command("services", &["b1"], pane("b1"), restart);
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str()), (Status::Done, "restarted web"));
    let start = Args { service: "broken".into(), verb: Some(ServiceVerb::Start), ..Default::default() };
    let id = cli.command("services", &["b1"], pane("b1"), start);
    let r = cli.result(id);
    assert_eq!((r.status, r.why.as_str()), (Status::Failed, "cannot start broken: it is not executable"));
}

// The harnesses page and a box's harness changes reach the app as the core's: the harnesses
// with their actions, a file to edit as an edit the app answers with saved, and a review whose
// save names the candidate it showed.
#[test]
fn harnesses_edits_and_harness_changes_reach_the_app() {
    let mut cli = Cli::start("default");
    let id = cli.command("harnesses", &[], Context { tree: true, ..Default::default() }, Args::default());
    let r = cli.result(id);
    assert_eq!((r.status, r.harnesses.len()), (Status::Enter, 1));
    let claude = &r.harnesses[0];
    assert_eq!((claude.id.as_str(), claude.default, claude.files[0].path.as_str()), ("hc_claude", true, ".claude.json"));
    let default = claude.actions.iter().find(|a| a.action == HarnessAction::Default).expect("the default action");
    assert_eq!((default.enabled, default.why.as_str()), (false, "already the default"));

    let files = Args { harness: "hc_claude".into(), action: Some(HarnessAction::Files), path: ".claude.json".into(), ..Default::default() };
    let id = cli.command("harnesses", &[], Context { tree: true, ..Default::default() }, files);
    let edit = cli.expect(Type::Edit).edit.unwrap();
    assert_eq!((edit.command, edit.title.as_str()), (id, "Editing .claude.json"));
    assert_eq!(std::fs::read_to_string(&edit.path).unwrap(), "before");
    std::fs::write(&edit.path, "after").unwrap();
    cli.send(Message { r#type: Type::Saved, saved: Some(Saved { edit: edit.id, ..Default::default() }), ..Default::default() });
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str(), r.harness.as_str()), (Status::Done, "updated .claude.json", "hc_claude"));

    // One given up is canceled, and what was written to the file is not read back.
    let files = Args { harness: "hc_claude".into(), action: Some(HarnessAction::Files), path: ".claude.json".into(), ..Default::default() };
    let id = cli.command("harnesses", &[], Context { tree: true, ..Default::default() }, files);
    let edit = cli.expect(Type::Edit).edit.unwrap();
    std::fs::write(&edit.path, "never saved").unwrap();
    cli.send(Message { r#type: Type::Saved, saved: Some(Saved { edit: edit.id, canceled: true, ..Default::default() }), ..Default::default() });
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str(), r.harnesses.len()), (Status::Canceled, "", 0));

    let id = cli.command("review config", &["b1"], Context::default(), Args::default());
    let r = cli.result(id);
    let changes = r.changes.expect("the box's harness changes");
    assert_eq!((r.status, changes.candidate.as_str(), changes.files[0].path.as_str()), (Status::Enter, "c1", "settings.json"));
    assert!(changes.offers.iter().all(|o| o.enabled), "{:?}", changes.offers);
    let save = Args { change: Some(HarnessChange::Save), candidate: changes.candidate.clone(), ..Default::default() };
    let id = cli.command("review config", &["b1"], Context::default(), save);
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str()), (Status::Done, "harness changes: saved"));
}

// A box's tools reach the app as the core's: the tools command's tools, each with its files, and
// the box's addresses; a tool on this machine handed the box; a tool's session made on a stream
// as its tab, naming its tool; and a tool's file to edit as an edit the app answers with saved.
#[test]
fn tools_reach_the_app() {
    let mut cli = Cli::start("default");
    let id = cli.command("tools", &["b1"], pane("b1"), Args::default());
    let r = cli.result(id);
    assert_eq!((r.status, r.r#box.as_str()), (Status::Enter, "b1"));
    let tools: Vec<_> = r.tools.iter().map(|t| (t.id.as_str(), t.detail.as_str(), t.host, t.enabled, t.why.as_str())).collect();
    assert_eq!(tools, [
        ("diff", "a diff viewer", false, true, ""), ("fresh", "", false, true, ""), ("ide", "", true, true, ""),
        ("broken", "", false, false, "its command is empty"),
    ]);
    assert_eq!(r.tools[1].files.iter().map(|f| f.name.as_str()).collect::<Vec<_>>(), ["config.json"]);
    assert!(r.tools[1].files[0].detail.starts_with("→ ~/.config/fresh/config.json"), "{}", r.tools[1].files[0].detail);
    assert_eq!(r.addresses.iter().map(|a| (a.label.as_str(), a.value.as_str())).collect::<Vec<_>>(), [("ssh", "ssh b1"), ("git url", "ssh://b1/work")]);

    // A box that is not up has no tool to run, and nothing to reach it by.
    let id = cli.command("tools", &["b3"], Context { list_has_keys: true, tree: true, ..Default::default() }, Args::default());
    let r = cli.result(id);
    assert!(r.tools.iter().all(|t| !t.enabled && !t.why.is_empty()), "{:?}", r.tools);
    assert!(r.addresses.iter().all(|a| a.value.is_empty() && !a.why.is_empty()), "{:?}", r.addresses);

    let id = cli.command("tools", &["b1"], pane("b1"), Args { tool: "ide".into(), ..Default::default() });
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str()), (Status::Done, "opened b1 in IDE"));

    let run = Args { tool: "diff".into(), stream: 6, cols: 100, rows: 30, ..Default::default() };
    let id = cli.command("tools", &["b1"], pane("b1"), run);
    let r = cli.result(id);
    let tab = r.session.expect("the tool's tab");
    assert_eq!((r.status, tab.kind, tab.tool.as_str(), tab.title.as_str()), (Status::Done, TabKind::Tool, "diff", "diff"));

    let edit = Args { tool: "fresh".into(), file: "config.json".into(), ..Default::default() };
    let id = cli.command("tools", &["b1"], pane("b1"), edit);
    let e = cli.expect(Type::Edit).edit.unwrap();
    assert_eq!((e.command, e.title.as_str()), (id, "Editing config.json"));
    assert_eq!(std::fs::read_to_string(&e.path).unwrap(), "{}");
    std::fs::write(&e.path, "{\"theme\":\"mine\"}").unwrap();
    cli.send(Message { r#type: Type::Saved, saved: Some(Saved { edit: e.id, ..Default::default() }), ..Default::default() });
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str()), (Status::Done, "config.json saved — the next discobox to open fresh gets it"));
}

// The secrets page reaches the app as the core's: each secret with its row, its card and its
// live bindings each read in full; an unbind asked first, the page read again after; and a
// secret's history with each decision read in full.
#[test]
fn secrets_reach_the_app() {
    let mut cli = Cli::start("default");
    let page = Context { tree: true, ..Default::default() };
    let id = cli.command("credentials", &[], page.clone(), Args::default());
    let r = cli.result(id);
    assert_eq!((r.status, r.secrets.len()), (Status::Enter, 2));
    let gh = &r.secrets[0];
    assert_eq!((gh.name.as_str(), gh.r#where.as_str(), gh.standing, gh.standing_text.as_str()), ("gh", "sent nowhere", SecretStanding::Bound, "1 binding"));
    assert!(gh.card.iter().flat_map(|s| &s.fields).any(|f| f.label == "kind" && f.value == "token"), "{:?}", gh.card);
    let b = &gh.bindings[0];
    assert_eq!((b.id.as_str(), b.label.as_str()), ("bind_1", "GH_TOKEN"));
    assert!(b.detail.starts_with("discobox b1 · "), "{}", b.detail);

    let history = Args { secret: "sec_gh".into(), secret_action: Some(SecretAction::History), ..Default::default() };
    let id = cli.command("credentials", &[], page.clone(), history);
    let r = cli.result(id);
    assert_eq!((r.status, r.history.len(), r.history[0].label.as_str()), (Status::Done, 1, "1970-01-01T00:00:00Z · created · ann"));
    assert_eq!(r.history[0].card[0].label, "the decision");

    let unbind = Args { secret: "sec_gh".into(), secret_action: Some(SecretAction::Unbind), binding: "bind_1".into(), ..Default::default() };
    let id = cli.command("credentials", &[], page, unbind);
    let q = cli.expect(Type::Question).question.unwrap();
    assert!(q.default_no && q.body.contains("as GH_TOKEN"), "{q:?}");
    cli.send(Message { r#type: Type::Answer, answer: Some(Answer { question: q.id, key: "yes".into(), ..Default::default() }), ..Default::default() });
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str(), r.secret.as_str()), (Status::Done, "unbound", "sec_gh"));
    assert_eq!((r.secrets[0].standing, r.secrets[0].bindings.len()), (SecretStanding::Unbound, 0));
}

// A box's audit timeline reaches the app as the core's: first what it says before any read, then
// its records with their times and the runs they fold into with what a run says; a search,
// subscribed before the last subscription ends, narrows what was read at once, lit, with its
// tally; and the audit command reads a record in full, with the recordings it offers, and a
// recording.
#[test]
fn the_audit_timeline_and_its_records_reach_the_app() {
    let mut cli = Cli::start("default");
    let id = cli.command("audit", &["b1"], pane("b1"), Args::default());
    let r = cli.result(id);
    assert_eq!((r.status, r.r#box.as_str()), (Status::Enter, "b1"));

    cli.subscribe(1, Topic::Audit, "b1");
    let first = cli.take("the timeline before a read", |i| match i {
        Incoming::Message(Message { snapshot: Some(s), .. }) if s.sub == 1 => s.audit.clone(),
        _ => None,
    });
    assert_eq!((first.records.len(), first.note.as_str()), (0, "reading the audit trails…"));
    let timeline = cli.take("the timeline", |i| match i {
        Incoming::Message(Message { snapshot: Some(s), .. }) if s.sub == 1 => s.audit.clone().filter(|a| !a.records.is_empty()),
        _ => None,
    });
    let ids: Vec<_> = timeline.records.iter().map(|r| r.id.as_str()).collect();
    assert_eq!(ids, ["http-1", "http-2", "creds-1"]);
    assert!(!timeline.records[0].clock.is_empty() && timeline.note.is_empty());
    let runs: Vec<_> = timeline.runs.iter().map(|r| (r.first, r.last, r.summary.as_str())).collect();
    assert_eq!(runs, [(0, 1, "GET 200 https://api.github.com (count 2)"), (2, 2, "")]);

    cli.send(Message {
        r#type: Type::Subscribe,
        subscribe: Some(Subscribe { sub: 2, topic: Topic::Audit, r#box: "b1".into(), audit: Some(AuditQuery { text: "lent".into() }), ..Default::default() }),
        ..Default::default()
    });
    cli.send(Message { r#type: Type::Unsubscribe, unsubscribe: Some(Unsubscribe { sub: 1 }), ..Default::default() });
    let found = cli.take("the timeline searched", |i| match i {
        Incoming::Message(Message { snapshot: Some(s), .. }) if s.sub == 2 => s.audit.clone(),
        _ => None,
    });
    let ids: Vec<_> = found.records.iter().map(|r| r.id.as_str()).collect();
    assert_eq!((ids.as_slice(), found.tally.as_str()), (["creds-1"].as_slice(), "1 of 3"), "a search read the trails again");
    assert_eq!((found.records[0].source_lit.as_slice(), found.records[0].summary_lit.as_slice()), ([].as_slice(), [7, 8, 9, 10].as_slice()));

    let id = cli.command("audit", &["b1"], pane("b1"), Args { record: "http-1".into(), ..Default::default() });
    let r = cli.result(id);
    let text = r.audit.expect("the record");
    assert_eq!((r.status, text.title.as_str(), text.text.as_str()), (Status::Done, "Audit record", "GET https://api.github.com/user\n"));
    let recordings: Vec<_> = text.recordings.iter().map(|r| (r.part, r.label.as_str())).collect();
    assert_eq!(recordings, [(AuditPart::Response, "response body")]);
    assert_eq!(text.part, None);

    let id = cli.command("audit", &["b1"], pane("b1"), Args { record: "http-1".into(), part: Some(AuditPart::Response), ..Default::default() });
    let body = cli.result(id).audit.expect("the body");
    assert_eq!((body.title.as_str(), body.text.as_str(), body.part), ("Response body", "{\"login\":\"b1\"}", Some(AuditPart::Response)));
    let id = cli.command("audit", &["b1"], pane("b1"), Args { record: "gone".into(), ..Default::default() });
    let r = cli.result(id);
    assert_eq!((r.status, r.why.as_str()), (Status::Failed, "gone: no such record"));
}

// What waits on a person reaches the app with each box's notice, which a dismissal takes down;
// a box's request is answered through the core's card, a form with its card and actions, the
// action pressed riding on the submit.
#[test]
fn requests_reach_the_app() {
    let mut cli = Cli::start("default");
    cli.subscribe(1, Topic::Listing, "");
    cli.expect(Type::Snapshot);
    cli.subscribe(2, Topic::Requests, "");
    let w = cli.expect(Type::Snapshot).snapshot.unwrap().requests.expect("requests");
    assert_eq!((w.requests[0].subject.as_str(), w.requests[0].requester.as_str()), ("github", "agent · b1"));
    let n = &w.notices[0];
    assert_eq!((n.r#box.as_str(), n.kind, n.subject.as_str(), n.command.as_str(), n.urgent), ("b1", NoticeKind::Request, "credential request", "requests", true));
    assert!(w.arrived.is_empty() && w.said.is_empty(), "{w:?}");

    let id = cli.command("dismiss", &[], pane("b1"), Args::default());
    assert_eq!(cli.result(id).status, Status::Done);
    let w = cli.take("the requests again", |i| match i {
        Incoming::Message(Message { snapshot: Some(s), .. }) if s.sub == 2 => s.requests.clone(),
        _ => None,
    });
    assert!(w.notices.is_empty(), "a dismissed notice: {:?}", w.notices);

    let id = cli.command("requests", &["b1"], pane("b1"), Args::default());
    let f = cli.expect(Type::Form).form.unwrap();
    assert_eq!((f.title.as_str(), f.aside.as_str()), ("Allow credential use?", "1 of 1"));
    assert_eq!(f.actions.iter().map(|a| a.key.as_str()).collect::<Vec<_>>(), ["accept", "deny"]);
    assert!(f.card.iter().flat_map(|s| &s.lines).any(|l| l.text == "This box may use the credential."), "{:?}", f.card);
    cli.send(Message { r#type: Type::Submit, submit: Some(Submit { form: f.id, action: Some(CardAction::Deny), ..Default::default() }), ..Default::default() });
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str()), (Status::Done, "denied github"));
}

// A project that owes the harness changes explainer has the requests topic name the box it is
// about; the review config command with `explain` asks it once, and Review changes opens the
// box's review.
#[test]
fn the_explainer_reaches_the_app() {
    let mut cli = Cli::start("explain");
    cli.subscribe(1, Topic::Listing, "");
    cli.expect(Type::Snapshot);
    cli.subscribe(2, Topic::Requests, "");
    let w = cli.expect(Type::Snapshot).snapshot.unwrap().requests.expect("requests");
    assert_eq!(w.explain, "b1");

    let explain = Args { explain: true, ..Default::default() };
    let id = cli.command("review config", &["b1"], Context::default(), explain.clone());
    let q = cli.expect(Type::Question).question.unwrap();
    assert_eq!((q.title.as_str(), q.subject.as_str()), ("Saving harness changes", "b1"));
    assert_eq!(q.choices.iter().map(|c| c.key.as_str()).collect::<Vec<_>>(), ["review", "later"]);
    let w = cli.take("the requests no longer naming it", |i| match i {
        Incoming::Message(Message { snapshot: Some(s), .. }) if s.sub == 2 => s.requests.clone(),
        _ => None,
    });
    assert!(w.explain.is_empty(), "{w:?}");
    cli.answer(q.id, "review", false);
    let r = cli.result(id);
    assert_eq!((r.status, r.r#box.as_str(), r.changes.map(|c| c.candidate)), (Status::Enter, "b1", Some("c1".to_string())));

    let id = cli.command("review config", &["b1"], Context::default(), explain);
    assert_eq!(cli.result(id).status, Status::Canceled, "asked twice");
}

// A request that arrives after the first snapshot is one to notify; the secrets page lists the
// requests waiting on its server with how long each has waited, and answers one by its ID; a
// refresh allowed for the session renews the next one without asking, and says so.
#[test]
fn arrivals_and_renewals_reach_the_app() {
    let mut cli = Cli::start("default");
    cli.subscribe(1, Topic::Listing, "");
    cli.expect(Type::Snapshot);
    cli.subscribe(2, Topic::Requests, "");
    let w = cli.expect(Type::Snapshot).snapshot.unwrap().requests.expect("requests");
    assert!(w.arrived.is_empty(), "{w:?}");
    let requests = |cli: &mut Cli, what: &str, want: &dyn Fn(&discobox_bridge::wire::Requests) -> bool| cli.take(what, |i| match i {
        Incoming::Message(Message { snapshot: Some(s), .. }) if s.sub == 2 => s.requests.clone().filter(|r| want(r)),
        _ => None,
    });

    let id = cli.command("stop", &["b1"], Context::default(), Args::default());
    cli.result(id);
    let w = requests(&mut cli, "the refresh arriving", &|r| !r.arrived.is_empty());
    assert_eq!((w.arrived[0].request.as_str(), w.arrived[0].r#box.as_str(), w.arrived[0].title.as_str()), ("f1", "b1", "Refresh request"));

    let id = cli.command("credentials", &[], Context::default(), Args::default());
    let page = cli.result(id);
    let f1 = page.requests.iter().find(|r| r.id == "f1").expect("the page's requests");
    assert!(!f1.age.is_empty() && page.secret_words.as_ref().is_some_and(|w| w.waiting == "2 waiting" && w.requests == "Requests"), "{page:?}");

    let answer = Args { secret_action: Some(SecretAction::Answer), request: "f1".into(), ..Default::default() };
    let id = cli.command("credentials", &[], Context::default(), answer);
    let f = cli.expect(Type::Form).form.unwrap();
    assert_eq!(f.actions.iter().map(|a| (a.key, a.declines)).collect::<Vec<_>>(), [(CardAction::Refresh, false), (CardAction::Dismiss, true)]);
    let answers = HashMap::from([("permission".to_string(), "session".to_string())]);
    cli.send(Message { r#type: Type::Submit, submit: Some(Submit { form: f.id, answers, action: Some(CardAction::Refresh), ..Default::default() }), ..Default::default() });
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str()), (Status::Done, "renewed npm"));
    assert!(r.requests.iter().all(|x| x.id != "f1"), "the answered request is still on the page");

    let id = cli.command("stop", &["b1"], Context::default(), Args::default());
    cli.result(id);
    let w = requests(&mut cli, "the renewal said", &|r| !r.said.is_empty());
    assert_eq!((w.said[0].text.as_str(), w.said[0].warning), ("renewed npm", false));
}

// A box's primary is read for what its program says of itself: the header says the poll's
// before it reports, and what a report the program wrote (here, echoed) says after.
#[test]
fn a_primarys_report_reaches_the_header() {
    let mut cli = Cli::start("default");
    cli.subscribe(1, Topic::Listing, "");
    cli.expect(Type::Snapshot);
    cli.subscribe(2, Topic::Header, "b1");
    let header = |cli: &mut Cli| loop {
        let s = cli.expect(Type::Snapshot).snapshot.unwrap();
        if s.sub == 2 {
            break s.header.expect("the header");
        }
    };
    let h = header(&mut cli);
    assert_eq!((h.id.as_str(), h.agent.as_str(), h.title.as_str()), ("b1", "", "b1"), "the poll says nothing for b1");
    let open = Open { stream: 4, r#box: "b1".into(), exec: "primary".into(), cols: 80, rows: 24 };
    cli.send(Message { r#type: Type::Open, open: Some(open), ..Default::default() });
    cli.expect(Type::Stream);
    // "which?" in base64.
    cli.client.input(4, b"\x1b]7501;state=blocked:kind=question:msg=d2hpY2g/\x07").unwrap();
    let h = header(&mut cli);
    assert_eq!((h.agent.as_str(), h.agent_msg.as_str(), h.title.as_str()), ("blocked", "which?", "b1: which?"));
    cli.client.input(4, b"\x1b]2;go test\x07").unwrap();
    assert_eq!(header(&mut cli).title, "go test: which?", "the title the primary set titles the window");
}

// A link pressed in a pane is answered by its ID with where it goes: a URL its box printed at
// the port its header's forward bound, a box's address with the box to enter, and one naming no
// box with why.
#[test]
fn a_followed_link_is_answered_with_where_it_goes() {
    let mut cli = Cli::start("default");
    cli.subscribe(1, Topic::Listing, "");
    cli.expect(Type::Snapshot);
    cli.subscribe(2, Topic::Header, "b1");
    cli.expect(Type::Snapshot);
    let mut follow = |id: u64, uri: &str| {
        let f = Follow { id, uri: uri.into(), r#box: "b1".into() };
        cli.send(Message { r#type: Type::Follow, follow: Some(f), ..Default::default() });
        cli.result(id)
    };
    let r = follow(70, "http://localhost:8080/app");
    assert_eq!((r.status, r.url.as_str()), (Status::Done, "http://localhost:18080/app"));
    let r = follow(71, "discobox:///slo");
    assert_eq!((r.status, r.r#box.as_str()), (Status::Enter, "slow"));
    let r = follow(72, "discobox:///nobody");
    assert_eq!(r.status, Status::Failed);
    assert!(r.why.contains("no discobox"), "{}", r.why);
}

// A CLI that exits while a command is in flight closes the client: no result comes, the app is
// told the bridge is gone, and what it sends after fails rather than hangs.
// A create from the composer: the composer topic says what it would be made with as it is subscribed, a draft the
// app sends is kept, the create card is a form with what it would run, and the create reports
// its steps, says the box the moment the server takes it, and enters it.
#[test]
fn a_create_reaches_the_app() {
    let mut cli = Cli::start("default");
    cli.subscribe(1, Topic::Composer, "");
    let composer = cli.take("the composer", |i| match i {
        Incoming::Message(m) if m.r#type == Type::Snapshot => m.snapshot.as_ref().and_then(|s| s.composer.clone()),
        _ => None,
    });
    // The project's harness, read as the topic is subscribed, and nobody's choice.
    let chips: Vec<_> = composer.chips.iter().map(|c| (c.text.as_str(), c.chosen)).collect();
    assert_eq!(chips, [("claude", false)], "{composer:?}");
    cli.send(Message { r#type: Type::Draft, draft: Some(Draft { prompt: "half a thought".into() }), ..Default::default() });

    let id = cli.command("new", &[], Context::default(), Args { prompt: "fix it".into(), ..Default::default() });
    let form = cli.expect(Type::Form).form.unwrap();
    assert_eq!((form.command, form.action.as_str()), (id, "Create"));
    assert!(form.note.contains("-p 'fix it'"), "{}", form.note);
    assert!(form.fields.iter().any(|f| f.key == "harness"), "{:?}", form.fields);
    cli.submit(form.id, &[("skills:0", "./team")], true, false);
    let again = cli.expect(Type::Form).form.unwrap();
    assert_eq!(again.id, form.id);
    cli.submit(form.id, &[], false, false);

    let step = cli.expect(Type::Progress).progress.unwrap();
    assert_eq!(step.command, id);
    let mut steps = vec![step];
    while steps.last().unwrap().accepted.is_none() {
        steps.push(cli.expect(Type::Progress).progress.unwrap());
    }
    assert!(steps.iter().any(|p| p.text == "draft: half a thought"), "{steps:?}");
    assert_eq!(steps.last().unwrap().accepted.as_ref().unwrap().key, "made");
    let r = cli.result(id);
    assert_eq!((r.status, r.r#box.as_str()), (Status::Enter, "made"));
}

// A folder dropped on the composer is the options command with a source: checked, it is done
// without a form, and the composer's line names it.
#[test]
fn a_dropped_folder_is_the_next_source() {
    let mut cli = Cli::start("default");
    cli.subscribe(1, Topic::Composer, "");
    cli.expect(Type::Snapshot);
    let id = cli.command("options", &[], Context::default(), Args { source: "app".into(), ..Default::default() });
    let r = cli.result(id);
    assert_eq!(r.status, Status::Done, "{}", r.why);
    let composer = cli.take("the composer naming the folder", |i| match i {
        Incoming::Message(m) if m.r#type == Type::Snapshot => {
            m.snapshot.as_ref().and_then(|s| s.composer.clone()).filter(|c| c.chips.iter().any(|c| c.text == "/abs/app" && c.chosen))
        }
        _ => None,
    });
    assert!(!composer.chips.is_empty());
}

// The copy question's count comes as the same question sent again, by its ID, each time it
// climbs, until it is final; the question is answered once, by that ID.
#[test]
fn a_question_sent_again_carries_its_count() {
    let mut cli = Cli::start("copy");
    let id = cli.command("new", &[], Context::default(), Args::default());
    let form = cli.expect(Type::Form).form.unwrap();
    cli.submit(form.id, &[], false, false);
    let q = cli.expect(Type::Question).question.unwrap();
    assert_eq!((q.command, q.subject.as_str(), q.count.as_str()), (id, "/work/notes", "calculating…"));
    for count in ["5.0 MiB in 3 files, still counting…", "7.0 MiB in 4 files"] {
        let again = cli.expect(Type::Question).question.unwrap();
        assert_eq!((again.id, again.count.as_str()), (q.id, count));
    }
    cli.answer(q.id, "false", false);
    let r = cli.result(id);
    assert_eq!((r.status, r.r#box.as_str()), (Status::Enter, "made"));
}

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

// A box's issue and pull request reach the app: the issue topic as the core reads it, the pr
// command's tab, a merge asked of the ways the repository allows — one offered with why it
// cannot be chosen — and the files changed as a session of their own.
#[test]
fn issues_reach_the_app() {
    let mut cli = Cli::start("default");
    cli.send(Message {
        r#type: Type::Subscribe,
        subscribe: Some(Subscribe { sub: 40, topic: Topic::Issue, r#box: "b1".into(), issue: Some(IssueQuery { pull: true, full: true, ..Default::default() }), ..Default::default() }),
        ..Default::default()
    });
    let i = loop {
        let m = cli.expect(Type::Snapshot);
        let s = m.snapshot.unwrap();
        if s.sub == 40 {
            break s.issue.expect("the pull request");
        }
    };
    assert_eq!((i.label.as_str(), i.badge.as_str(), i.mark.tone), ("PR #9", "Open", IssueTone::Open));
    let p = i.pull_request.expect("what the pull request adds");
    let m = p.merge.as_ref().expect("an open pull request's merge box");
    assert!(m.ready && m.said == "Ready to merge into main", "{m:?}");
    assert_eq!((p.checks.failing, p.checks.passed, p.checks.unpassed[0].url.as_str()), (1, 1, "https://ci.example/lint"));
    let kinds: Vec<_> = i.timeline.iter().map(|t| t.kind).collect();
    assert_eq!(kinds, [IssueItemKind::Comment, IssueItemKind::Review, IssueItemKind::Commit, IssueItemKind::Event, IssueItemKind::Event]);
    assert_eq!(i.timeline[1].threads[0].r#where, "a.go:3");

    let id = cli.command("pr", &["b1"], pane("b1"), Args::default());
    let r = cli.result(id);
    let work = r.work.expect("the tab's work");
    assert_eq!((r.status, work.label.as_str()), (Status::Enter, "PR #9"));

    let id = cli.command("pr", &["b1"], pane("b1"), Args { act: Some(IssueAct::Merge), head: "abc1234567890".into(), ..Default::default() });
    let q = cli.expect(Type::Question).question.unwrap();
    assert_eq!(q.title, "Merge #9 into main");
    let why: Vec<_> = q.choices.iter().map(|c| (c.key.as_str(), c.why.as_str())).collect();
    assert_eq!(why, [("squash", ""), ("merge", "not allowed in this repository"), ("rebase", "not allowed in this repository")]);
    cli.answer(q.id, "squash", false);
    let r = cli.result(id);
    assert_eq!((r.status, r.note.as_str()), (Status::Done, "merged #9 (squash)"));

    let run = Args { stream: 7, cols: 100, rows: 30, head: "abc1234567890".into(), base: "base000000000".into(), ..Default::default() };
    let id = cli.command("files", &["b1"], pane("b1"), run);
    let r = cli.result(id);
    let tab = r.session.expect("the files changed tab");
    assert_eq!((r.status, tab.kind, tab.files, tab.title.as_str()), (Status::Done, TabKind::Files, 9, "files changed #9"));
}
