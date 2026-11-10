//! Images carried in from this machine, and a folder dropped on the composer (`paste.rs`), on
//! the mock: what is sent, what the pane holds while it waits, and what is pasted after.

use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use base64::Engine;
use rune::clipboard::{ClipboardContent, ClipboardImage, MemoryClipboard};
use rune::{Env, Key, KeyEvent, Mods, Runtime, Tx};

use crate::backend::{Backend, Event, Out};
use crate::paste::{DropOver, MAX_PASTED_IMAGE, quote};
use crate::wire::{Result as CommandResult, Status, Upload};
use crate::{Side, State, on_key, poll, run_command, theme, view};

fn runtime() -> Runtime<State> {
    let mut rt = Runtime::with_text(State::new(), view::view, (1440.0, 900.0), Env { system_dark: true, scale: 1.0 }, theme::text_system());
    rt.poll = Some(poll);
    rt.on_key = Some(on_key);
    settle(&mut rt);
    rt
}

fn settle(rt: &mut Runtime<State>) {
    for _ in 0..20 {
        rt.frame(1.0 / 60.0);
    }
}

/// What the app sent: each upload, held back from the mock so a test answers it, and the input
/// to each stream.
#[derive(Clone, Default)]
struct Sent {
    uploads: Arc<Mutex<Vec<Upload>>>,
    input: Arc<Mutex<Vec<(u32, Vec<u8>)>>>,
}

impl Sent {
    fn input_to(&self, stream: u32) -> Vec<u8> {
        self.input.lock().unwrap().iter().filter(|(s, _)| *s == stream).flat_map(|(_, b)| b.clone()).collect()
    }
    fn uploads(&self) -> Vec<Upload> { self.uploads.lock().unwrap().clone() }
}

struct Recorder {
    inner: Box<dyn Backend + Send>,
    sent: Sent,
}

impl Backend for Recorder {
    fn send(&mut self, m: Out) {
        match m {
            Out::Upload(u) => self.sent.uploads.lock().unwrap().push(u),
            m => self.inner.send(m),
        }
    }
    fn input(&mut self, stream: u32, bytes: &[u8]) {
        self.sent.input.lock().unwrap().push((stream, bytes.to_vec()));
    }
    fn resize(&mut self, stream: u32, cols: u16, rows: u16) { self.inner.resize(stream, cols, rows); }
}

struct Null;
impl Backend for Null {
    fn send(&mut self, _: Out) {}
    fn input(&mut self, _: u32, _: &[u8]) {}
    fn resize(&mut self, _: u32, _: u16, _: u16) {}
}

/// The first box attached, its primary focused, with what is sent recorded and a clipboard of
/// the test's own.
fn attached() -> (Runtime<State>, Sent, MemoryClipboard, u32) {
    let mut rt = runtime();
    rt.key(KeyEvent { key: Key::Enter, mods: Mods::default() });
    settle(&mut rt);
    let sent = Sent::default();
    let inner = std::mem::replace(&mut rt.state.backend, Box::new(Null));
    rt.state.backend = Box::new(Recorder { inner, sent: sent.clone() });
    let clip = rune::clipboard::memory();
    rt.use_clipboard(&clip);
    let stream = rt.state.focused_pane().expect("a focused pane").stream;
    (rt, sent, clip, stream)
}

/// The smallest PNG the clipboard takes: a header and a 2×3 IHDR.
fn png() -> Vec<u8> {
    let mut b = b"\x89PNG\r\n\x1a\n\0\0\0\rIHDR".to_vec();
    b.extend_from_slice(&[0, 0, 0, 2, 0, 0, 0, 3, 8, 6, 0, 0, 0]);
    b
}

fn an_image(clip: &MemoryClipboard) { clip.set_content(ClipboardContent::new("").with_image(ClipboardImage::from_png(png()).unwrap())); }

/// A key with the platform's primary modifier: ⌘ on macOS, Ctrl+Shift elsewhere.
fn primary(c: char) -> KeyEvent {
    let mods = if cfg!(target_os = "macos") { Mods { cmd: true, ..Mods::default() } } else { Mods { ctrl: true, shift: true, cmd: true, ..Mods::default() } };
    KeyEvent { key: Key::Command(c), mods }
}

/// Ctrl+V: the key the agents bind to pasting an image. Off macOS, Command mirrors Control.
fn ctrl_v() -> KeyEvent { KeyEvent { key: Key::Command('v'), mods: Mods { ctrl: true, cmd: !cfg!(target_os = "macos"), ..Mods::default() } } }

fn typed(c: &str) -> KeyEvent { KeyEvent { key: Key::Text(c.into()), mods: Mods::default() } }

/// The upload answered as the CLI would, and the window given a moment to take it.
fn answer(rt: &mut Runtime<State>, u: &Upload, status: Status, paths: &[&str], why: &str) {
    let paths = paths.iter().map(|p| p.to_string()).collect();
    rt.state.inbox.push(Event::Result(CommandResult { id: u.id, status, paths, why: why.into(), ..Default::default() }));
    settle(rt);
}

// The paste gesture with an image on the clipboard carries it into the box on screen; what is
// typed meanwhile is held, and goes to the program after the path the image landed at, pasted
// as its program asked to be pasted to.
#[test]
fn a_pasted_image_is_carried_in_and_its_path_pasted_before_what_was_typed() {
    let (mut rt, sent, clip, stream) = attached();
    an_image(&clip);
    rt.key(primary('v'));
    settle(&mut rt);
    let uploads = sent.uploads();
    let [u] = uploads.as_slice() else { panic!("uploads {uploads:?}") };
    assert_eq!((u.r#box.as_str(), u.name.as_str()), (rt.state.live.as_deref().unwrap(), "clipboard.png"));
    assert_eq!(base64::engine::general_purpose::STANDARD.decode(&u.data).unwrap(), png());
    assert!(u.files.is_empty());
    assert!(rt.state.message.as_deref().is_some_and(|m| m.starts_with("copying into")), "{:?}", rt.state.message);

    rt.key(typed("x"));
    settle(&mut rt);
    assert!(sent.input_to(stream).is_empty(), "a key overtook the image");

    answer(&mut rt, u, Status::Done, &["/tmp/discobox-paste/20261110-0-clipboard.png"], "");
    let mut want = rt.state.pane(stream).unwrap().screen.paste("/tmp/discobox-paste/20261110-0-clipboard.png");
    want.push(b'x');
    assert_eq!(sent.input_to(stream), want);
    assert_eq!(rt.state.message.as_deref(), Some("pasted the image"));
    assert!(!rt.state.holding(stream));

    // Text on the clipboard is pasted as text, image or not: it is what a terminal's paste is.
    clip.set_content(ClipboardContent::new("words").with_image(ClipboardImage::from_png(png()).unwrap()));
    rt.key(primary('v'));
    settle(&mut rt);
    assert_eq!(sent.uploads().len(), 1, "text was carried in as an image");
    assert!(sent.input_to(stream).ends_with(&rt.state.pane(stream).unwrap().screen.paste("words")));
}

// One that cannot be carried in is said, and nothing is pasted for it; what was held still
// reaches the program.
#[test]
fn a_failed_upload_pastes_nothing_and_lets_the_held_input_go() {
    let (mut rt, sent, clip, stream) = attached();
    an_image(&clip);
    rt.key(primary('v'));
    settle(&mut rt);
    rt.key(typed("y"));
    settle(&mut rt);
    let u = sent.uploads().pop().expect("an upload");
    answer(&mut rt, &u, Status::Failed, &[], "copy clipboard.png into the discobox: no room");
    assert_eq!(sent.input_to(stream), b"y");
    assert_eq!(rt.state.message.as_deref(), Some("copy clipboard.png into the discobox: no room"));
    assert!(!rt.state.holding(stream));
}

// Ctrl+V on a harness terminal is the agents' image paste: an image on the clipboard is carried
// in; with none, the key reaches the program as typed. On a shell it is always the key.
#[test]
fn the_paste_image_key_carries_an_image_in_on_a_harness_terminal_only() {
    let (mut rt, sent, clip, stream) = attached();
    clip.set_text("not an image");
    rt.key(ctrl_v());
    settle(&mut rt);
    assert!(sent.uploads().is_empty());
    assert_eq!(sent.input_to(stream), [0x16], "Ctrl+V with no image is the program's");

    an_image(&clip);
    rt.key(ctrl_v());
    settle(&mut rt);
    let u = sent.uploads().pop().expect("an image under Ctrl+V was not carried in");
    answer(&mut rt, &u, Status::Done, &["/tmp/discobox-paste/a.png"], "");
    let want = [vec![0x16], rt.state.pane(stream).unwrap().screen.paste("/tmp/discobox-paste/a.png")].concat();
    assert_eq!(sent.input_to(stream), want);

    run_command(&mut rt.state, "shell", &mut Tx::default());
    settle(&mut rt);
    let shell = rt.state.live_ws().unwrap().column(Side::Shells)[0];
    rt.state.select_pane(shell);
    let shell = rt.state.focused_pane().unwrap().stream;
    rt.key(ctrl_v());
    settle(&mut rt);
    assert_eq!(sent.uploads().len(), 1, "Ctrl+V on a shell carried an image in");
    assert_eq!(sent.input_to(shell), [0x16], "Ctrl+V on a shell is quoted-insert");
}

// The mouse and the wheel are not held behind an image on its way in, as the TUI holds neither:
// replayed after the path, a full-screen agent's wheel arrows would walk its prompt history over it.
#[test]
fn the_wheel_is_not_held_behind_an_image() {
    let (mut rt, sent, clip, stream) = attached();
    // A program that asked for the mouse (SGR): the wheel is its button presses.
    rt.state.pane_mut(stream).unwrap().screen.feed(b"\x1b[?1000h\x1b[?1006h");
    settle(&mut rt);
    an_image(&clip);
    rt.key(primary('v'));
    settle(&mut rt);
    assert!(rt.state.holding(stream));
    let (x, y) = rt.find(&format!("screen-{stream}")).unwrap();
    rt.wheel_event(rune::WheelEvent::new(0.0, 120.0).at(x, y));
    settle(&mut rt);
    assert!(!sent.input_to(stream).is_empty(), "the wheel was held behind the image");
    assert!(rt.state.holding(stream));
}

// Files copied in a file manager are the image key's too: carried in when they are images, and
// the key as typed when they are not.
#[test]
fn the_paste_image_key_takes_copied_image_files() {
    let (mut rt, sent, clip, stream) = attached();
    clip.set_content(ClipboardContent::new("shot.png").with_files([PathBuf::from("/home/me/shot.png")]));
    rt.key(ctrl_v());
    settle(&mut rt);
    let u = sent.uploads().pop().expect("copied files under Ctrl+V were not sent");
    assert_eq!(u.files, ["/home/me/shot.png"]);
    answer(&mut rt, &u, Status::Done, &["/tmp/p/0-shot.png"], "");
    assert_eq!(sent.input_to(stream), rt.state.pane(stream).unwrap().screen.paste("/tmp/p/0-shot.png"));

    let before = sent.input_to(stream).len();
    clip.set_content(ClipboardContent::new("notes.txt").with_files([PathBuf::from("/home/me/notes.txt")]));
    rt.key(ctrl_v());
    settle(&mut rt);
    let u = sent.uploads().pop().unwrap();
    answer(&mut rt, &u, Status::Done, &[], "");
    assert_eq!(sent.input_to(stream)[before..], [0x16], "copied files that are not images: the key as typed");
}

// Files dropped on a pane go to the CLI by their paths, which carries them in when every one is
// an image; a drop that is not all images is pasted as the files' paths, quoted as a shell
// needs them. The pane is lit while a drag is over it.
#[test]
fn files_dropped_on_a_pane_are_carried_in_or_pasted_as_paths() {
    let (mut rt, sent, _clip, stream) = attached();
    let at = rt.find(&format!("screen-{stream}"));
    rt.drag_files(vec!["/home/me/a.png".into()], at);
    settle(&mut rt);
    assert_eq!(rt.state.drop_over, Some(DropOver::Pane(stream)), "the pane is not lit under the drag");
    rt.drop_files(vec!["/home/me/a.png".into(), "/home/me/b.jpg".into()], at);
    settle(&mut rt);
    assert_eq!(rt.state.drop_over, None);
    let u = sent.uploads().pop().expect("the drop was not sent");
    assert_eq!(u.files, ["/home/me/a.png", "/home/me/b.jpg"]);
    assert!(u.data.is_empty());
    answer(&mut rt, &u, Status::Done, &["/tmp/p/0-a.png", "/tmp/p/1-b.jpg"], "");
    assert_eq!(sent.input_to(stream), rt.state.pane(stream).unwrap().screen.paste("/tmp/p/0-a.png /tmp/p/1-b.jpg"));
    assert_eq!(rt.state.message.as_deref(), Some("pasted 2 images"));

    let before = sent.input_to(stream).len();
    rt.drop_files(vec!["/home/me/my notes.txt".into(), "/home/me/a.png".into()], at);
    settle(&mut rt);
    let u = sent.uploads().pop().unwrap();
    answer(&mut rt, &u, Status::Done, &[], "");
    let text = format!("{} /home/me/a.png", quote("/home/me/my notes.txt"));
    assert_eq!(sent.input_to(stream)[before..], rt.state.pane(stream).unwrap().screen.paste(&text));
}

// While an image is on its way, another is not started over it: it is said and left.
#[test]
fn one_image_at_a_time_per_pane() {
    let (mut rt, sent, clip, stream) = attached();
    an_image(&clip);
    rt.key(primary('v'));
    settle(&mut rt);
    let at = rt.find(&format!("screen-{stream}"));
    rt.drop_files(vec!["/home/me/a.png".into()], at);
    settle(&mut rt);
    assert_eq!(sent.uploads().len(), 1);
    assert_eq!(rt.state.message.as_deref(), Some("still copying the last image in"));
}

// An image past what a message on the bridge carries is said, and never sent: the bridge would
// end on it.
#[test]
fn an_image_too_large_for_the_bridge_is_said() {
    let (mut rt, sent, clip, stream) = attached();
    let mut big = png();
    big.resize(MAX_PASTED_IMAGE + 1, 0);
    clip.set_content(ClipboardContent::new("").with_image(ClipboardImage::from_png(big).unwrap()));
    rt.key(primary('v'));
    settle(&mut rt);
    assert!(sent.uploads().is_empty());
    assert!(rt.state.message.as_deref().is_some_and(|m| m.contains("drop the file")), "{:?}", rt.state.message);
    assert!(!rt.state.holding(stream));
}

// A folder dropped on the composer is where the next discobox is cut from: the core checks it
// and the composer's line names it. Anything but a folder passes the composer by, and one the
// core refuses is said.
#[test]
fn a_folder_dropped_on_the_composer_is_the_next_source() {
    let mut rt = runtime();
    let dir = std::env::temp_dir();
    let at = rt.find("composer-box");
    rt.drag_files(vec![dir.clone()], at);
    settle(&mut rt);
    assert_eq!(rt.state.drop_over, Some(DropOver::Composer));
    rt.drop_files(vec![dir.clone()], at);
    settle(&mut rt);
    let named = dir.to_string_lossy().into_owned();
    assert!(rt.state.summary.iter().any(|c| c.text == named && c.chosen), "the line does not name it: {:?}", rt.state.summary);
    assert!(rt.state.message.as_deref().is_some_and(|m| m.contains(&named)), "{:?}", rt.state.message);

    // A file is no source: the composer is not lit for it, and nothing changes.
    let file: PathBuf = dir.join("discobox-not-a-folder.txt");
    std::fs::write(&file, "x").unwrap();
    rt.state.message = None;
    rt.drag_files(vec![file.clone()], at);
    settle(&mut rt);
    assert_ne!(rt.state.drop_over, Some(DropOver::Composer));
    rt.drop_files(vec![file.clone()], at);
    settle(&mut rt);
    assert_eq!(rt.state.message, None);
    let _ = std::fs::remove_file(&file);

    rt.state.drop_source(&[dir.join("discobox-gone")]);
    settle(&mut rt);
    assert!(rt.state.message.as_deref().is_some_and(|m| m.contains("not a directory")), "{:?}", rt.state.message);
}

#[test]
fn a_path_is_quoted_only_when_it_needs_to_be() {
    assert_eq!(quote("/home/me/a.png"), "/home/me/a.png");
    if !cfg!(windows) {
        assert_eq!(quote("/home/me/my notes.txt"), "'/home/me/my notes.txt'");
        assert_eq!(quote("/tmp/it's"), "'/tmp/it'\\''s'");
        assert_eq!(quote("/tmp/$(x)"), "'/tmp/$(x)'");
    }
}
