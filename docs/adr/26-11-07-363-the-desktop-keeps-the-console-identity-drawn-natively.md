# 26-11-07-363 — The desktop keeps the console's identity, drawn natively

- **Status**: Proposed
- **Date**: 2026-11-07
- **Issue**: [#786](https://github.com/discobox-ai/discobox/issues/786) (epic), [#787](https://github.com/discobox-ai/discobox/issues/787)

## Context

The desktop console should read as the same product as `discobox console`.
It should also look like a desktop app rather than a terminal screenshot.

Checked at `00541f9c90fefeabe1b61134a737fab287861803`:

- The TUI's identity:
  - **Colours** (`theme.go`): `colMark` (#f45cff) for focus and the brand,
    the gold band (`colGold`, 220) for the list's title, and dim and grey text
    on the terminal's black.
  - **Frames:** boxes drawn with box-drawing characters, titles and tabs riding
    the top border as `[ name ]`, and `[+][x]` buttons.
  - **Marks:** state and agent glyphs (`stateDot`, `agentMark`) and the logo
    (`logo.json`), drawn with block elements.
- The prototype used Inter for chrome and the system monospace font for
  terminals. It drew the TUI's bracket glyphs as text (`[ name ]`, `[+]`) and
  its logo as rectangles taken from the block elements.
- Rune draws vector shapes, shadows, gradients and text with spring motion.
  Fonts are supplied by the app (`AppSpec::fonts`), replacing its embedded
  default.

## Decision

### 1. Same identity, the desktop's materials

The desktop keeps the TUI's palette, glyphs, mark and structure:
- a list with folder sections and a gold title band;
- a workspace whose sessions sit in framed columns with their names on the
  frame;
- a pink focus colour.

It draws them as a desktop app would, never by imitating terminal cells:

| TUI device | Desktop rendering |
| --- | --- |
| Box-drawing frames | Rounded vector strokes, hairline when unfocused |
| `[ 0 name ]` tabs in the border | Tab chips set into the frame's top edge, with hover, drag to reorder, and a close control |
| `[+]` `[x]` | Icon buttons with tooltips naming their shortcut |
| Cursor row on 237 | A rounded selection with hover and press states |
| Hint line of leader keys | Menus, tooltips, and the palette's shortcut column; a status line only for messages |
| Block-element logo | The mark as vector art |
| Instant redraw | Spring motion for focus, tabs, maximize and the sidebar; none in terminal content |

### 2. JetBrains Mono throughout

JetBrains Mono is the console's typeface on desktop, in the chrome and in
terminals. It is bundled with the app under the SIL Open Font License, at the
weights the chrome uses. Hierarchy comes from weight, size and colour rather
than a second family. Terminal cells take JetBrains Mono's measured advance.
Rune's own rasteriser draws it, so it renders the same on macOS and Windows.

### 3. Dark first, with a light theme

The dark palette is the TUI's. A light palette uses the same tokens, with
`colMark` and the gold deepened for contrast on light ground. The app follows
the system appearance unless the user picks one. Terminal content keeps the
program's own colours in both themes, on a terminal ground from the active
palette.

### 4. Tokens from the design language, through the core

The design language (`assets/brand/LANGUAGE.md`) and its tokens
(`assets/brand/tokens.json`) are the one source of colour, type, space and
shape for every surface: each colour role's dark and light value, and its
xterm-256 index. The core's spec
([26-11-07-876](26-11-07-876-the-console-is-one-core-with-two-renderers.md)
§4) carries them to the desktop. The TUI uses each role's xterm index, as
today. The website copies the rendered `tokens.css`. The desktop adds no
colour the tokens do not name.

## Alternatives rejected

- **A native platform look** (system fonts, AppKit or WinUI controls). The
  two consoles would no longer read as one product, and the desktop would
  diverge between macOS and Windows.
- **Draw the TUI's characters** (bracket tabs, box-drawing frames,
  block-element logo). The prototype did this. It looks like a terminal in a
  window, and it fights Rune's pointer, motion and accessibility support.
- **Inter for chrome, monospace only in terminals.** The prototype did this
  as well. JetBrains Mono throughout keeps the console's monospaced voice and
  one family to tune.

## Consequences

- The desktop app bundles JetBrains Mono and replaces Rune's embedded default.
- The core's spec carries both palettes, read from `assets/brand/tokens.json`.
- The TUI's appearance does not change.
