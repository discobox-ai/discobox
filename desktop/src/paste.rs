//! Images and folders carried in from this machine: an image pasted or dropped on a pane, and a
//! folder dropped on the composer.
//!
//! An agent in a box asked to look at a screenshot has nowhere to take it from: the clipboard
//! it would read is the box's, which is empty, and a file dragged onto the window is a path on
//! this machine, which the box cannot open. So an image meant for a pane is carried in, as the
//! TUI carries one (`cli/internal/tui/imagepaste.go`): written into the box by the core
//! (`upload`, `DataSource.PutImage`, under the core's name for it), and what reaches the
//! program is the path it landed at, pasted as a terminal pastes a dragged file — which every
//! agent takes as an image.
//!
//! What means an image:
//!
//! - the paste gesture with an image on the clipboard and no text, or files on it (a file copied
//!   in Finder or Explorer), into any of a box's sessions but a service's;
//! - the paste image gesture (Ctrl+V, Alt+V: the keys the agents bind to pasting an image) on a
//!   harness terminal with an image on the clipboard, or image files copied there; with none, the
//!   key goes through as it was typed. Shells are left out, where Ctrl+V is quoted-insert;
//! - files dropped on a pane, which the CLI reads, and carries in when every one is an image
//!   (`clipimage.Files`). A drop that is not all images is pasted as the files' paths, as a
//!   terminal pastes them.
//!
//! Which panes take images, and which the image keys, is the core's (`Tab.images`,
//! `Tab.image_keys`: `console.Tab.TakesImages`, `TakesImageKeys`).
//!
//! While the clipboard is read and the images carried in, what is typed or pasted into the pane
//! is held, and sent after the paths, in order, so nothing typed behind a paste arrives in front
//! of it. The mouse and the wheel are not held, as the TUI holds neither (`State::send_to`). One
//! image at a time per pane: another paste of one while it waits is said and left.
//!
//! A folder dropped on the composer is where the next discobox is cut from: the `options`
//! command with the folder as its source, which the core checks as it checks a source typed on
//! the create card, and the composer's line names.

use std::path::{Path, PathBuf};

use base64::Engine;
use rune::{ClipboardContent, Tx};

use crate::backend::Out;
use crate::wire::{Args, Result as CommandResult, Status, Upload, MAX_FRAME};
use crate::{Pending, State};

/// What a drop is over, for its highlight: a pane by its stream, or the composer.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum DropOver {
    Pane(u32),
    Composer,
}

/// What goes to the program when an upload carried no image in: the files dropped, pasted as
/// their paths; or the image key, as it was typed.
pub enum NoImage {
    Paths(Vec<String>),
    Key(Vec<u8>),
}

/// The JSON an upload is wrapped in, beside its image's base64: what keeps the message under the
/// bridge's bound, generously.
const UPLOAD_OVERHEAD: usize = 4096;

/// The largest clipboard image the bridge carries: its base64, in a message under the bound.
pub const MAX_PASTED_IMAGE: usize = (MAX_FRAME - UPLOAD_OVERHEAD) / 4 * 3;

impl State {
    /// The box a pane is in, by its key.
    fn pane_box(&self, stream: u32) -> Option<String> {
        self.workspaces.iter().find(|(_, w)| w.panes.iter().any(|p| p.stream == stream)).map(|(k, _)| k.clone())
    }

    /// Whether a pane takes images: one the core says does (`console.Tab.TakesImages`: any of
    /// its box's sessions but a service's), still running.
    pub fn takes_images(&self, stream: u32) -> bool { self.pane(stream).is_some_and(|p| !p.exited && p.exec.images) }

    /// Whether the focused pane takes the paste image gesture: one the core says does
    /// (`console.Tab.TakesImageKeys`: a harness terminal), still running.
    pub fn harness_focused(&self) -> bool {
        self.pane_has_keys() && self.focused_pane().is_some_and(|p| !p.exited && p.exec.image_keys)
    }

    /// Starts holding a pane's input, and says whether it was free to: one already holding is
    /// waiting on an image.
    fn hold(&mut self, stream: u32) -> bool {
        if !self.takes_images(stream) {
            return false;
        }
        let Some(p) = self.pane_mut(stream) else { return false };
        if p.held.is_some() {
            return false;
        }
        p.held = Some(vec![]);
        true
    }

    /// Whether a pane is holding its input, for an image on its way.
    pub fn holding(&self, stream: u32) -> bool { self.pane(stream).is_some_and(|p| p.held.is_some()) }

    /// Sends `first`, then everything held behind it, in the order it came, and lets the pane's
    /// input go.
    fn release(&mut self, stream: u32, first: Vec<u8>) {
        let Some(p) = self.pane_mut(stream) else { return };
        let held = p.held.take().unwrap_or_default();
        if p.exited {
            return;
        }
        if !first.is_empty() {
            p.screen.to_bottom();
        }
        for bytes in std::iter::once(first).chain(held).filter(|b| !b.is_empty()) {
            self.backend.input(stream, &bytes);
        }
    }

    /// Text as a pane's program is pasted it: bracketed when it asked, markers taken out.
    fn paste_bytes(&self, stream: u32, text: &str) -> Vec<u8> {
        self.pane(stream).filter(|_| !text.is_empty()).map(|p| p.screen.paste(text)).unwrap_or_default()
    }

    /// The paste gesture on a pane: the clipboard read, then its text pasted, or its files or
    /// image carried in. The pane's input is held until it is.
    pub fn paste(&mut self, tx: &mut Tx) {
        let Some(stream) = self.focused_pane().filter(|p| !p.exited).map(|p| p.stream) else { return };
        if self.hold(stream) {
            tx.read_clipboard(move |s: &mut State, content, _| s.pasted(stream, content, None));
            return;
        }
        // A service's tab, or a pane waiting on an image: text goes in, behind what it holds.
        tx.read_clipboard(move |s: &mut State, content, _| {
            if !content.text().is_empty() {
                s.paste_text(stream, content.text());
            } else if content.image().is_some() || !content.files().is_empty() {
                s.message = s.holding(stream).then(|| "still copying the last image in".into());
            }
        });
    }

    /// The paste image gesture on a harness terminal: the clipboard's image carried in, or the
    /// key as it was typed when there is none.
    pub fn paste_image(&mut self, key: Vec<u8>, tx: &mut Tx) {
        let Some(stream) = self.focused_pane().map(|p| p.stream) else { return };
        if !self.hold(stream) {
            if self.holding(stream) {
                // Behind the image on its way, in order, like any key.
                self.type_into(stream, &key);
            }
            return;
        }
        tx.read_clipboard(move |s: &mut State, content, _| s.pasted(stream, content, Some(key)));
    }

    /// What a read of the clipboard means for a pane holding its input. Files copied there are
    /// carried in as a drop is, when every one is an image; then, for the paste gesture (no
    /// `key`), text is pasted; then an image is carried in. For the paste image gesture, files
    /// that are not all images, or no image at all, are the key as it was typed — the clipboard
    /// reader the TUI's image key asks takes an image file first too (`clipimage`).
    fn pasted(&mut self, stream: u32, content: &ClipboardContent, key: Option<Vec<u8>>) {
        if !content.files().is_empty() {
            return self.upload_files(stream, content.files(), key);
        }
        if key.is_none() && !content.text().is_empty() {
            let bytes = self.paste_bytes(stream, content.text());
            return self.release(stream, bytes);
        }
        match content.image() {
            Some(image) => self.upload_image(stream, image.png()),
            None => self.release(stream, key.unwrap_or_default()),
        }
    }

    /// Files dropped on a pane: carried in when every one is an image, pasted as their paths
    /// when not. A pane that takes no images takes no drop; one waiting on an image says so.
    pub fn drop_files(&mut self, stream: u32, files: &[PathBuf]) {
        self.drop_over = None;
        if files.is_empty() || !self.takes_images(stream) {
            return;
        }
        if !self.hold(stream) {
            self.message = Some("still copying the last image in".into());
            return;
        }
        self.upload_files(stream, files, None);
    }

    /// The upload of files to the pane's box, which the CLI reads: the pane holds its input. When
    /// they are not all images, the image key that asked goes to the program, or with none, the
    /// files' paths are pasted.
    fn upload_files(&mut self, stream: u32, files: &[PathBuf], key: Option<Vec<u8>>) {
        let files: Vec<String> = files.iter().map(|f| f.to_string_lossy().into_owned()).collect();
        let otherwise = match key {
            Some(key) => NoImage::Key(key),
            None => NoImage::Paths(files.clone()),
        };
        self.upload(stream, Upload { files, ..Default::default() }, otherwise);
    }

    /// The upload of the clipboard's image, as PNG, to the pane's box. One the bridge cannot
    /// carry is said, and the pane let go.
    fn upload_image(&mut self, stream: u32, png: &[u8]) {
        if png.len() > MAX_PASTED_IMAGE {
            self.message = Some(format!("the image is {:.1} MB, more than a paste carries: save it, and drop the file on the pane", png.len() as f64 / 1e6));
            return self.release(stream, vec![]);
        }
        let data = base64::engine::general_purpose::STANDARD.encode(png);
        self.upload(stream, Upload { name: "clipboard.png".into(), data, ..Default::default() }, NoImage::Key(vec![]));
    }

    fn upload(&mut self, stream: u32, mut u: Upload, otherwise: NoImage) {
        let Some(sandbox) = self.pane_box(stream) else { return self.release(stream, vec![]) };
        let id = self.next_id;
        self.next_id += 1;
        u.id = id;
        u.r#box = sandbox.clone();
        self.pending.insert(id, Pending::Upload(stream, otherwise));
        let name = self.box_by_id(&sandbox).map(|b| b.name.clone()).unwrap_or(sandbox);
        self.message = Some(format!("copying into {name}…"));
        self.backend.send(Out::Upload(u));
    }

    /// An upload's result: the paths its images landed at pasted in their place, or, when they
    /// were not all images, what goes instead (`NoImage`); then what the pane held. A failure
    /// pastes nothing: the key behind it would only have the agent read the box's own empty
    /// clipboard.
    pub(crate) fn uploaded(&mut self, stream: u32, otherwise: NoImage, r: &CommandResult) {
        let bytes = match (r.status, otherwise) {
            (Status::Done, _) if !r.paths.is_empty() => {
                self.message = Some(if r.paths.len() == 1 { "pasted the image".into() } else { format!("pasted {} images", r.paths.len()) });
                self.paste_bytes(stream, &r.paths.join(" "))
            }
            (Status::Done, NoImage::Paths(files)) => {
                self.message = None;
                self.paste_bytes(stream, &files.iter().map(|f| quote(f)).collect::<Vec<_>>().join(" "))
            }
            (Status::Done, NoImage::Key(key)) => {
                self.message = None;
                key
            }
            _ => vec![],
        };
        self.release(stream, bytes);
    }

    /// A folder dropped on the composer: where the creates that follow are cut from, once the
    /// core has checked it. A discobox is cut from one source, so a drop of several is said.
    pub fn drop_source(&mut self, folders: &[PathBuf]) {
        self.drop_over = None;
        let [folder] = folders else {
            self.message = Some("drop one folder: a discobox is cut from one source".into());
            return;
        };
        let context = self.context();
        let args = Args { source: folder.to_string_lossy().into_owned(), ..Default::default() };
        self.command("options", vec![], context, args, Pending::Enter("options".into()));
    }

    /// A file drag over a drop target, or off it.
    pub fn drop_hover(&mut self, over: DropOver, on: bool) {
        if on {
            self.drop_over = Some(over);
        } else if self.drop_over == Some(over) {
            self.drop_over = None;
        }
    }
}

/// Whether a dropped path is a folder the composer takes.
pub fn is_folder(path: &Path) -> bool { path.is_dir() }

/// A path as a terminal pastes a dragged file: as it is when nothing in it needs quoting, and
/// quoted for the shell otherwise.
pub fn quote(path: &str) -> String {
    // A backslash is a separator on Windows, and an escape to a shell everywhere else.
    let safe = if cfg!(windows) { "/._-+:@,%=~\\" } else { "/._-+:@,%=~" };
    let plain = !path.is_empty() && path.chars().all(|c| c.is_ascii_alphanumeric() || safe.contains(c));
    if plain {
        path.to_string()
    } else if cfg!(windows) {
        format!("\"{path}\"")
    } else {
        format!("'{}'", path.replace('\'', "'\\''"))
    }
}
