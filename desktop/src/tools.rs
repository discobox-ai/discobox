//! A box's tools: the tools menu, a tool's session as a tab given the window, a tool that runs on
//! this machine handed the box, a tool's files edited, and the box's addresses copied.
//!
//! What the menu lists, why a tool cannot run, and what each comes to are the core's (the tools
//! command's tools and addresses, `console.PickerTools`, `Tool.Why`, `AddressOffers`); the app
//! draws them and binds the keys. A tool's session is a tab after the shells (the core's order),
//! like any other; choosing a tool gives its tab the window (the workspace maximized onto it), and
//! `maximize` gives the workspace back with the tool still running on its tab, as the TUI's
//! window over the workspace is put away. A tool that is already running is shown, never started
//! twice; one the box left running is picked up by the sessions topic like any other session.

use rune::Tx;

use crate::wire::{Address, Args, Entry, Result as CommandResult, Tool};
use crate::{DEFAULT_SIZE, Focus, Menu, MenuKind, Pending, State};

/// The menu's entry for a tool, by its ID.
const RUN: &str = "tool:";
/// The menu's entry for a tool's files, by its ID.
const CONFIG: &str = "config:";
/// The menu's entry for an address, by its label.
const COPY: &str = "copy:";

/// Where the tools menu opens: over the workspace, as the services menu does.
fn at(s: &State) -> (f32, f32) { (if s.sidebar { 340.0 } else { 40.0 }, 96.0) }

/// The tools command's flow: the box's tools as the core listed them, each by its label with what
/// it is or why it cannot run; then the box's addresses, each copied as it is; then the files of
/// every tool that carries any, to edit.
pub fn open_menu(s: &mut State, r: &CommandResult) {
    s.close_overlays();
    let sandbox = r.r#box.clone();
    let mut entries: Vec<Entry> = r.tools.iter().map(|t| Entry {
        command: format!("{RUN}{}", t.id), label: t.label.clone(), detail: t.detail.clone(), enabled: t.enabled, why: t.why.clone(), uses: 0,
    }).collect();
    entries.extend(r.addresses.iter().map(|a| Entry {
        command: format!("{COPY}{}", a.label), label: format!("copy {}", a.label), detail: a.value.clone(),
        enabled: !a.value.is_empty(), why: a.why.clone(), uses: 0,
    }));
    entries.extend(r.tools.iter().filter(|t| !t.files.is_empty()).map(|t| Entry {
        command: format!("{CONFIG}{}", t.id), label: format!("{} config", t.label),
        detail: match t.files.as_slice() {
            [one] => one.detail.clone(),
            many => format!("{} files, edited in your editor", many.len()),
        },
        enabled: true, why: String::new(), uses: 0,
    }));
    if entries.is_empty() {
        s.message = Some(if r.note.is_empty() { "no tools".into() } else { r.note.clone() });
        return;
    }
    if r.tools.is_empty() && !r.note.is_empty() {
        s.message = Some(r.note.clone());
    }
    let picked = entries.iter().position(|e| e.enabled).unwrap_or(0);
    let at = at(s);
    s.menu = Some(Menu { kind: MenuKind::Tools(sandbox, r.tools.clone(), r.addresses.clone()), at, entries, picked, sub: 0 });
}

/// One of the tools menu's entries, chosen.
pub fn choose(s: &mut State, sandbox: String, tools: Vec<Tool>, addresses: Vec<Address>, id: &str, tx: &mut Tx) {
    s.close_overlays();
    if let Some(label) = id.strip_prefix(COPY) {
        // What the menu showed is what is copied.
        let value = addresses.into_iter().find(|a| a.label == label).map(|a| a.value).unwrap_or_default();
        if !value.is_empty() {
            tx.copy(value.clone());
            s.message = Some(format!("copied {value}"));
        }
        return;
    }
    if let Some(t) = id.strip_prefix(RUN).and_then(|id| tools.iter().find(|t| t.id == id)) {
        run(s, sandbox, t);
        return;
    }
    if let Some(t) = id.strip_prefix(CONFIG).and_then(|id| tools.iter().find(|t| t.id == id)).cloned() {
        match t.files.as_slice() {
            [one] => edit(s, sandbox, &t, one.name.clone()),
            _ => open_files(s, sandbox, t),
        }
    }
}

/// A tool's files, to pick the one to edit: each by its name, with where it lands in a box and
/// where its copy is on this machine.
fn open_files(s: &mut State, sandbox: String, t: Tool) {
    let entries = t.files.iter().map(|f| Entry {
        command: f.name.clone(), label: f.name.clone(), detail: f.detail.clone(), enabled: true, why: String::new(), uses: 0,
    }).collect();
    let at = at(s);
    s.menu = Some(Menu { kind: MenuKind::ToolFiles(sandbox, t), at, entries, picked: 0, sub: 0 });
}

/// Has one of a tool's files edited: the core has the file opened in the person's editor (an
/// edit, as a sheet that waits) and says what came of it.
pub fn edit(s: &mut State, sandbox: String, t: &Tool, file: String) {
    s.close_overlays();
    s.message = Some(format!("opening {file}…"));
    let context = s.context();
    let args = Args { tool: t.id.clone(), file, ..Default::default() };
    s.command("tools", vec![sandbox], context, args, Pending::Enter("tools".into()));
}

/// Runs a tool against its box, as the core offers it: one that runs on this machine is handed
/// the box; any other's tab is given the window — the one running, or a session made for it on a
/// stream allocated for its pane.
pub fn run(s: &mut State, sandbox: String, t: &Tool) {
    s.close_overlays();
    let context = s.context();
    if t.host {
        s.message = Some(format!("opening {}…", t.label));
        let args = Args { tool: t.id.clone(), ..Default::default() };
        s.command("tools", vec![sandbox], context, args, Pending::Enter("tools".into()));
        return;
    }
    if show(s, &sandbox, &t.id) {
        return;
    }
    let stream = s.next_stream;
    s.next_stream += 1;
    let (cols, rows) = DEFAULT_SIZE;
    s.message = Some(format!("{}…", t.label));
    let args = Args { tool: t.id.clone(), stream, cols: cols.into(), rows: rows.into(), ..Default::default() };
    let shown = s.live.clone();
    s.command("tools", vec![sandbox.clone()], context, args, Pending::Session(stream, sandbox, shown));
}

/// Gives a tool's tab in a box's workspace the window, and whether the workspace held one.
pub fn show(s: &mut State, sandbox: &str, tool: &str) -> bool {
    let held = s.workspaces.get(sandbox).is_some_and(|w| w.panes.iter().any(|p| p.exec.tool == tool && !p.exited));
    if !held {
        return false;
    }
    if s.live.as_deref() != Some(sandbox) {
        s.show_workspace(sandbox);
    }
    let Some(ws) = s.workspaces.get_mut(sandbox) else { return false };
    let Some(i) = ws.panes.iter().position(|p| p.exec.tool == tool && !p.exited) else { return false };
    ws.show(i);
    ws.maximized = true;
    s.focus = Focus::Panes;
    true
}
