# 26-11-24-732 — Ctrl-V on a watched clipboard pastes its text

- **Status**: Proposed (would supersede [26-10-16-535](26-10-16-535-a-pasted-image-is-carried-into-the-discobox-as-a-file.md)
  §3's "with no image the key goes through unchanged" for Ctrl-V with text on a
  watched clipboard, and its Consequences' "only for a Ctrl-V within a second
  of the change")
- **Date**: 2026-11-24

## Context

ADR 26-10-16-535 takes Ctrl-V and Alt-V on a harness terminal as "paste an
image". With no image, §3 lets the key through unchanged, so that the agent
gets the key as if the console were not there. §5 adds one exception. On
Windows and WSL the console watches the clipboard (`cli/internal/clipwatch`),
and text that arrived within `dictationWindow` (one second) before the key is a
dictation tool's. That text is pasted (`readKey`, `cli/internal/tui/imagepaste.go`).
Any other text lets the key through, and `TestCtrlVWithAStaleClipboardIsTheKey`
asserts this.

That rule assumed the outer terminal owns text paste. Windows Terminal, macOS
terminals and Linux terminals do: their paste is Ctrl-V, Cmd-V or
Ctrl-Shift-V, handled before any key reaches the console. The legacy Windows
console (conhost, the default window for Windows PowerShell 5.1 and Command
Prompt on Windows 10) does not. There Ctrl-V reaches the console as the key
`\x16`, and nothing else on the keyboard pastes text. The key goes through
to the agent, the agent reads the box's own empty clipboard, and Claude Code
shows "no image in clipboard". Text can only be pasted with the mouse (#1573).

## Decision

1. **Ctrl-V on a harness terminal, with a live clipboard watch, pastes the
   clipboard's text when it carries no image.** It no longer matters how
   recently the text arrived, so the text held when the watch started counts
   too: copying and then running `discobox attach` is the ordinary order. The
   text is given to the pane as a bracketed paste, the way the dictation text
   already is (`imageReadMsg.text`). An image still wins: a snapshot that may
   hold one is read and carried in as before. If that read finds no image, the
   snapshot's text is pasted, and the key is sent only when there is no text
   either.
2. **Ctrl-V only, not Alt-V.** Alt-V is the image key where the terminal takes
   Ctrl-V for its own paste, in Windows Terminal, and in every terminal where
   Alt-V reaches the console there is another text paste. Alt-V keeps
   26-10-16-535 §3: with no image, the key goes through.
3. **With no live watch, the key still goes through.** This covers every
   platform with no watch (macOS, Linux off WSL), where the outer terminal owns
   text paste. It also covers a Windows or WSL watch that failed to start or has
   not reported yet. Ctrl-V reads the clipboard for an image there as before,
   and the failed watch is reported once, as it is today. A text read on the key
   is not added (see below).
4. **The dictation wait stays.** A press still waits up to `dictationWait`
   (150ms) for a change within `dictationWindow` that the watch has not yet
   reported, so a dictation tool's write that the key overtook is pasted
   instead of the older snapshot. Its text is pasted under rule 1 as before.
   The window now only decides whether the press waits, never whether text is
   pasted.
5. **Unchanged elsewhere:** shells and tool panes never have Ctrl-V taken
   (26-10-16-535 §3), and a leader on Ctrl-V is still the leader.

## Alternatives rejected

- **Detect conhost and change behaviour only there** (no `WT_SESSION`, a
  console host process check). The detection is unreliable: WSL started from
  conhost, Windows Terminal profiles that don't set it, a console behind SSH.
  It also isn't needed, because a terminal that pastes on Ctrl-V never sends the
  key. The rule is right in any terminal that does send it, including a
  Windows Terminal whose user unbound Ctrl-V.
- **Alt-V pastes text too.** It would change what the image key means in
  Windows Terminal, where Ctrl-V already pastes text, and fixes nothing that
  Ctrl-V does not.
- **Read the text with PowerShell on the key when the watch is not live.** The
  key already starts PowerShell to look for an image, so this would cost little.
  But the read takes most of a second, and a dictation tool restores the old
  clipboard within half a second. The press would then paste whatever was
  copied before the dictation, in place of what was dictated, and nothing would
  say so. A key that pastes nothing, next to a status line saying the watch is
  off, is the safer failure. Revisit if a failed watch turns out to be common
  in practice (its report is already shown).
- **Widen `dictationWindow`** to some minutes. It would fix the issue's
  reproduction but not a copy made earlier than that. Any window is
  arbitrary when the right answer doesn't depend on when the text arrived.
- **Let the key through and print a hint** to use the mouse. That is what
  happens today, with Claude Code's hint. It leaves conhost with no keyboard
  text paste.

## Consequences

- In conhost, Ctrl-V in a harness terminal pastes text and images, as Ctrl-V
  does everywhere else on Windows. `TestCtrlVWithAStaleClipboardIsTheKey`
  reverses into the triage's
  `TestCtrlVWithTextOnAWatchedClipboardPastesTheText`.
- With text on the clipboard, an agent never receives a bare Ctrl-V from the
  keyboard on Windows or WSL. An agent's own Ctrl-V there only ever read a
  clipboard the box doesn't have.
- The watched text is now used for any Ctrl-V pressed in a harness terminal,
  not only within a second of the change. It is still kept only in memory,
  only the latest change, never logged, and only the keyboard can trigger the
  paste: no program in the box can press Ctrl-V on the console's side. A
  password copied earlier pastes when the user presses Ctrl-V, which is what
  Ctrl-V does in every other window.
- A clipboard holding both text and a picture (an Excel range, a Word
  selection) still pastes the picture.
- Every Ctrl-V with older text waits up to 150ms for a change notice before it
  pastes.
- The console help (`docs/public/console.md`) and the TUI's `DESIGN.md` must
  stop describing Ctrl-V with no image as always going through.

## Open for implementation

- The wording of the once-only status line for a watch that is not running,
  today "dictation through Ctrl-V is off". It now also means Ctrl-V text paste
  is off.
