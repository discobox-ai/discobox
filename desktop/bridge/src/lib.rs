//! The desktop app's side of the bridge (ADR 26-11-07-289).
//!
//! `discobox desktop` runs the app with `--bridge` and serves it the console core over a pipe
//! each way (`cli/internal/console/bridge`). This crate is everything the app reads and writes
//! on it: the wire's types (`wire`, generated from the bridge's Go types through its schema), the
//! framing, the opening `hello`, and a reader that hands each frame on as it arrives. What the
//! app makes of a message is the app's (`desktop/src/backend.rs`).
//!
//! It has nothing of the window in it, so its contract suite (`tests/contract.rs`), which runs
//! this client against the Go bridge over a fake of the core, builds on every platform.

pub mod wire;

use std::io::{self, Read, Write};
use std::sync::{Arc, Mutex};

use serde_json::Value;

use crate::wire::{
    Form, Hello, Message, PROTOCOL, Question, ResizePayload, Result as CommandResult, SPEC_VERSION, Status, Stream,
    StreamState, Type, Welcome, kind,
};

impl wire::Box {
    /// Whether the box is up.
    pub fn running(&self) -> bool { self.state == wire::State::Running }
}

impl wire::Spec {
    /// A registry command, by its ID.
    pub fn command(&self, id: &str) -> Option<&wire::SpecCommand> { self.commands.iter().find(|c| c.id == id) }

    /// A gesture, by its ID.
    pub fn gesture(&self, id: &str) -> Option<&wire::SpecGesture> { self.gestures.iter().find(|g| g.id == id) }
}

/// What arrives from the CLI, in the order it was sent.
#[derive(Debug, PartialEq)]
pub enum Incoming {
    /// A JSON message. One the wire's types could not read arrives as what lets whatever waits
    /// on it go on (`unreadable`).
    Message(Message),
    /// A stream's output, standard or error.
    Output(u32, Vec<u8>),
    /// A stream's end, and why: empty when it ended cleanly.
    Exited(u32, String),
    /// The CLI is gone: nothing more arrives, and nothing sent reaches it. Always the last.
    Closed,
}

/// The app's end of the bridge: writes whole frames, from any thread.
#[derive(Clone)]
pub struct Client {
    out: Arc<Mutex<Box<dyn Write + Send>>>,
}

impl Client {
    /// Says hello on `output` and waits on `input` for the welcome, then hands `input` to a
    /// reader thread that passes everything after it to `on`, ending with `Incoming::Closed`.
    /// A CLI of another protocol or console spec is refused, with which side is older.
    pub fn connect<R, W>(
        mut input: R, output: W, mut on: impl FnMut(Incoming) + Send + 'static,
    ) -> Result<(Client, Welcome), String>
    where
        R: Read + Send + 'static,
        W: Write + Send + 'static,
    {
        let client = Client { out: Arc::new(Mutex::new(Box::new(output))) };
        let hello = Message { r#type: Type::Hello, hello: Some(Hello { protocol: PROTOCOL }), ..Default::default() };
        client.send(&hello).map_err(|e| format!("the bridge closed: {e}"))?;
        let welcome = welcome(&mut input)?;
        std::thread::spawn(move || {
            while let Ok((k, stream, payload)) = read_frame(&mut input) {
                match k {
                    kind::JSON => {
                        debug(|| format!("← {}", String::from_utf8_lossy(&payload)));
                        match serde_json::from_slice::<Message>(&payload) {
                            Ok(m) => on(Incoming::Message(m)),
                            Err(e) => {
                                if let Some(m) = unreadable(&payload, e) {
                                    on(Incoming::Message(m));
                                }
                            }
                        }
                    }
                    kind::STDOUT | kind::STDERR => on(Incoming::Output(stream, payload)),
                    kind::EXIT => on(Incoming::Exited(stream, String::from_utf8_lossy(&payload).into())),
                    _ => {}
                }
            }
            on(Incoming::Closed);
        });
        Ok((client, welcome))
    }

    /// Sends one message.
    pub fn send(&self, m: &Message) -> io::Result<()> {
        let m = serde_json::to_string(m).map_err(io::Error::other)?;
        debug(|| format!("→ {m}"));
        self.frame(kind::JSON, 0, m.as_bytes())
    }

    /// Sends what was typed into a stream's terminal.
    pub fn input(&self, stream: u32, bytes: &[u8]) -> io::Result<()> { self.frame(kind::INPUT, stream, bytes) }

    /// Sends a stream's pane size.
    pub fn resize(&self, stream: u32, cols: u16, rows: u16) -> io::Result<()> {
        let size = serde_json::to_vec(&ResizePayload { cols, rows }).map_err(io::Error::other)?;
        self.frame(kind::RESIZE, stream, &size)
    }

    /// Asks for a stream's screen again.
    pub fn repaint(&self, stream: u32) -> io::Result<()> { self.frame(kind::REPAINT, stream, &[]) }

    /// Sends one frame. Private: the app sends a message, or one of the frames ADR 289 §1 gives
    /// it, through the methods above.
    fn frame(&self, kind: u8, stream: u32, payload: &[u8]) -> io::Result<()> {
        write_frame(&mut *self.out.lock().unwrap(), kind, stream, payload)
    }
}

/// Reads up to the welcome, and is it, holding its protocol and its spec's version to this
/// side's before anything else is read: another version's fields may not parse.
fn welcome(input: &mut impl Read) -> Result<Welcome, String> {
    loop {
        let (k, _, payload) = read_frame(input).map_err(|e| format!("the bridge closed: {e}"))?;
        if k != kind::JSON {
            continue;
        }
        let mut v: Value = serde_json::from_slice(&payload).map_err(|e| e.to_string())?;
        // Protocol 1 had no message types: a CLI speaking it answers this hello with an error
        // of its own shape.
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
        let version = v["welcome"]["spec"]["version"].as_i64().unwrap_or(0);
        if version != SPEC_VERSION {
            return Err(format!(
                "this desktop app reads console spec version {SPEC_VERSION}, and the CLI handed over version {version}: run the app built with this CLI"
            ));
        }
        let welcome: Welcome = serde_json::from_value(v["welcome"].take()).map_err(|e| e.to_string())?;
        if welcome.spec.is_none() {
            return Err("the CLI's welcome carried no spec".into());
        }
        return Ok(welcome);
    }
}

/// Writes one frame: kind (1), stream (u32), length (u32), big-endian, then the payload.
fn write_frame(w: &mut impl Write, kind: u8, stream: u32, payload: &[u8]) -> io::Result<()> {
    let len = u32::try_from(payload.len()).map_err(io::Error::other)?;
    let mut head = [0u8; 9];
    head[0] = kind;
    head[1..5].copy_from_slice(&stream.to_be_bytes());
    head[5..9].copy_from_slice(&len.to_be_bytes());
    w.write_all(&head)?;
    w.write_all(payload)?;
    w.flush()
}

/// Reads one frame: its kind, stream and payload.
fn read_frame(r: &mut impl Read) -> io::Result<(u8, u32, Vec<u8>)> {
    let mut head = [0u8; 9];
    r.read_exact(&mut head)?;
    let stream = u32::from_be_bytes(head[1..5].try_into().unwrap());
    let len = u32::from_be_bytes(head[5..9].try_into().unwrap()) as usize;
    let mut payload = vec![0; len];
    r.read_exact(&mut payload)?;
    Ok((head[0], stream, payload))
}

/// Why this app and the CLI cannot talk: which side is older.
fn mismatch(cli: i64) -> String {
    let older = if cli < PROTOCOL { "the CLI is older" } else { "this desktop app is older" };
    format!("this desktop app speaks bridge protocol {PROTOCOL} and the CLI speaks {cli}: {older}; run the app built with this CLI")
}

/// A message the generated types could not read: never expected, since both sides are built
/// from one schema, so it is always said. What the CLI waits on still gets its way out, read
/// by its ID from the raw message: a result ends its command as failed, a question and a form
/// arrive bare, for the app to decline and dismiss, and a stream is failed, so its pane does not
/// wait either. Anything else is only said.
fn unreadable(payload: &[u8], e: serde_json::Error) -> Option<Message> {
    let v: Value = serde_json::from_slice(payload).unwrap_or_default();
    let kind = v["type"].as_str().unwrap_or("JSON");
    eprintln!("desktop: a {kind} message from the CLI could not be read: {e}");
    let body = &v[kind];
    let why = format!("the desktop app could not read the CLI's {kind}: {e}; run the app built with this CLI");
    let title = || body["title"].as_str().unwrap_or("the CLI's message").to_string();
    Some(match kind {
        "result" => {
            let id = body["id"].as_u64()?;
            Message {
                r#type: Type::Result,
                result: Some(CommandResult { id, status: Status::Failed, why, ..Default::default() }),
                ..Default::default()
            }
        }
        "question" => Message {
            r#type: Type::Question,
            question: Some(Question { id: body["id"].as_u64()?, title: title(), ..Default::default() }),
            ..Default::default()
        },
        "form" => Message {
            r#type: Type::Form,
            form: Some(Form { id: body["id"].as_u64()?, title: title(), ..Default::default() }),
            ..Default::default()
        },
        "stream" => {
            let stream = u32::try_from(body["stream"].as_u64()?).ok()?;
            Message {
                r#type: Type::Stream,
                stream: Some(Stream { stream, state: StreamState::Failed, why }),
                ..Default::default()
            }
        }
        _ => return None,
    })
}

/// `DISCOBOX_DESKTOP_DEBUG=1` logs every message each way to stderr.
pub fn debug(line: impl FnOnce() -> String) {
    static ON: std::sync::OnceLock<bool> = std::sync::OnceLock::new();
    if *ON.get_or_init(|| std::env::var_os("DISCOBOX_DESKTOP_DEBUG").is_some()) {
        eprintln!("desktop: {}", line());
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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

    // A welcome of another protocol, or from a CLI of protocol 1, is refused before its fields
    // are read.
    #[test]
    fn a_welcome_of_another_protocol_is_refused() {
        let frames = |json: &str| {
            let mut wire = Vec::new();
            write_frame(&mut wire, kind::JSON, 0, json.as_bytes()).unwrap();
            wire
        };
        let newer = frames(&format!(r#"{{"type":"welcome","welcome":{{"protocol":{},"spec":"not a spec"}}}}"#, PROTOCOL + 1));
        assert!(welcome(&mut newer.as_slice()).unwrap_err().contains("this desktop app is older"));
        let one = frames(r#"{"id":0,"error":"the app is older"}"#);
        assert!(welcome(&mut one.as_slice()).unwrap_err().contains("the CLI is older"));
    }

    // What the types cannot read but something waits on still gets its way out: a result fails
    // its command, a question and a form arrive to be declined and dismissed, a stream fails;
    // the rest is only said.
    #[test]
    fn an_unreadable_result_fails_its_command() {
        let unread = |s: &str| unreadable(s.as_bytes(), serde_json::from_str::<Message>(s).unwrap_err());
        let r = unread(r#"{"type":"result","result":{"id":6,"status":"queued"}}"#).and_then(|m| m.result).expect("no result");
        assert_eq!((r.id, r.status), (6, Status::Failed));
        assert!(r.why.contains("could not read"));
        let q = unread(r#"{"type":"question","question":{"id":7,"command":1,"title":"Trust?","choices":[{"key":1}]}}"#)
            .and_then(|m| m.question)
            .expect("no question");
        assert_eq!((q.id, q.title.as_str()), (7, "Trust?"));
        let f = unread(r#"{"type":"form","form":{"id":8,"command":1,"title":"Rename","fields":null}}"#).and_then(|m| m.form).expect("no form");
        assert_eq!(f.id, 8);
        let st = unread(r#"{"type":"stream","stream":{"stream":9,"state":"paused"}}"#).and_then(|m| m.stream).expect("no stream");
        assert_eq!((st.stream, st.state), (9, StreamState::Failed));
        assert!(unread(r#"{"type":"snapshot","snapshot":{"sub":1,"topic":"weather"}}"#).is_none());
    }
}
