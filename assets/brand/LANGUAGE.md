# The discobox design language

How discobox looks and reads, in every place it appears:
- the console (`discobox console`, a TUI);
- the desktop app (`discobox desktop`);
- the website (`discobox-ai/site`);
- documents and reports made about it.

The tokens are in [`tokens.json`](tokens.json), with a CSS rendering in
[`tokens.css`](tokens.css). This file says what they are for.

## Where it comes from

Discobox is used in a terminal. The language started there: the console's
pink focus frame, its gold title band, its state glyphs, and a mark drawn in
block characters. Everything else carries that terminal into a lighter, more
spacious setting without pretending to be a terminal.

## Principles

1. **One accent, used for focus.** Magenta (`--magenta`, the mark's hue at full
   strength) shows what has focus or needs a decision: the focused pane, the
   recommended option, the selected row, a link. If everything is magenta,
   nothing is.
2. **Gold marks the one title of a view.** The gold band heads a list or a table,
   once per view. Gold's only other job is an in-between state, as the console
   gives it: starting, suspended, partly done. It is never a button, a border
   around something important, or a warning.
3. **Monospace is the voice.** JetBrains Mono sets headings, interface text, keys,
   identifiers and data everywhere. Only long-form prose on the web uses the
   companion sans, so that paragraphs stay comfortable to read.
4. **Structure means something.** Numbers appear only on real sequences
   (phases, steps). Frames appear only around things that need attention.
   Labels name true categories. Nothing is decoration.
5. **Dark first, light as an equal.** The canonical ground is the purple-black of
   a terminal. The light theme uses the same roles and is designed with the same
   care, not inverted. A picture of a terminal (a terminal window, a capture of
   the console) stays dark in both themes: it shows the product as it is.
6. **Native, not imitated.** Outside a terminal, the terminal's devices are drawn
   with real shapes. Tabs, buttons and frames are drawn as vectors. Box-drawing
   characters and `[ ]` brackets are never typed out as text. The bracket
   survives only as the frame's title marks.
7. **Plain words.** Name things the way the person using them does. Use the
   active voice and short sentences. An error says what went wrong and what to do.

## Colour

Colours are roles, not swatches. Use the role, never the hex.

| Role | Token | Dark | Light | TUI (xterm-256) | Use |
| --- | --- | --- | --- | --- | --- |
| Ground | `--ink` | `#0a0410` | `#f7f4fa` | terminal background | page and window background |
| Raised | `--ink-raised` | `#150b1f` | `#ffffff` | — | cards, frames, panels |
| Sunken | `--ink-sunken` | `#06020a` | `#eee8f4` | — | code blocks, inputs, terminal panes |
| Line | `--line` | `#2a1839` | `#e1d8ea` | 236 | hairlines, unfocused frames |
| Line, bright | `--line-bright` | `#3d2352` | `#c9bad8` | 238 | dividers that must show, hover outlines |
| Text | `--text` | `#f4ecf9` | `#1d1424` | terminal foreground | body and headings |
| Text, muted | `--text-muted` | `#b0a0bf` | `#5e4f6c` | 245 | secondary text, captions |
| Text, faint | `--text-faint` | `#7c6a8c` | `#8a7b98` | 240 | hints, disabled, metadata |
| Accent | `--magenta` | `#f45cff` | `#a81fbd` | `#f45cff` | focus, selection, links, the recommended choice |
| Brand | `--brand` | `#9e4aa7` | `#9e4aa7` | — | the mark's own purple; brand art only, untinted |
| Second light | `--violet` | `#8b2fd6` | `#6d22b0` | — | atmosphere: background glow, gradients |
| Title band | `--gold` | `#ffd700` | `#e8c200` | 220 | the one title band per view, key hints, the cursor chevron |
| On gold | `--on-gold` | `#0a0410` | `#0a0410` | 232 | text on the band |
| Running, OK | `--ok` | `#5fff5f` | `#1a7a34` | 83 | running state, success |
| Added, done | `--green` | `#87ff87` | `#237a3a` | 120 | diff additions, a finished agent |
| Removed | `--salmon` | `#ffaf87` | `#b4532a` | 216 | diff deletions |
| Warning | `--warn` | `#ffaf00` | `#a35f00` | 214 | needs attention soon, status messages |
| Error | `--err` | `#ff6b62` | `#b3261e` | 196 | failed, blocked |
| Info | `--info` | `#87afff` | `#2c5fb8` | 111 | neutral notices |
| Selection | `--selection` | `#2a1530` | `#f3defa` | 237 | the cursor row, selected text |

Rules:
- **Contrast.** Text roles hold 4.5:1 against the ground in both themes, and the
  accent holds 5:1, which is why light uses `#a81fbd`, not `#f45cff`. Faint text
  (about 3.6:1) is for hints and metadata only, never for something a person must
  read to act.
- **Semantic colours** (OK, warning, error) are never the accent, and never
  carry meaning alone. A state also has a glyph or a word.
- **The brand purple** is the art's colour. It is not lightened for type; the
  accent does that job.
- **The TUI** uses the xterm indices in the table, so it renders the same in
  every terminal's 256-colour palette. Truecolor is used only for the mark.

## Type

| Role | Token | Face | Where |
| --- | --- | --- | --- |
| Voice | `--font-mono` | JetBrains Mono, then `ui-monospace`, SF Mono, Menlo, Consolas | headings, interface, keys, IDs, data, code, terminals |
| Prose | `--font-prose` | IBM Plex Sans, then `system-ui` | long paragraphs on the web and in documents only |

- **Desktop:** bundles JetBrains Mono under the SIL Open Font License.
- **Web:** loads JetBrains Mono and IBM Plex Sans from Google Fonts, with the
  fallbacks above.
- **TUI:** uses whatever font the terminal uses.

### Scale

A 1.25 ratio from 16 px:

| Step | Size | Weight | Use |
| --- | --- | --- | --- |
| `--step-3` | 2.5rem | 700, tracking −0.02em | page title |
| `--step-2` | 1.75rem | 700, −0.015em | section heading |
| `--step-1` | 1.25rem | 600 | sub-heading, lede |
| `--step-0` | 1rem | 400 | body, interface |
| `--step--1` | 0.8125rem | 500 | captions, hints, table headers |

Rules:
- **Eyebrows:** `--step--1` in the accent, uppercase, tracking +0.08em.
- **Measure:** running text stays under about 68 characters.
- **Headings** balance their lines.
- **Numbers** that line up in columns use tabular figures.

## Space, shape, motion

- **Space:** a 4 px grid. The steps are 4, 8, 12, 16, 24, 32, 48 and 64 px.
  Siblings are spaced with `gap`, not margins.
- **Radii:**
  - 4 px for keys and chips;
  - 6 px for rows and bands;
  - 10 px for cards;
  - 12 px for frames and panes.
- **Strokes:**
  - a focused frame is 1.5 px in the accent;
  - an unfocused frame is 1 px in `--line`;
  - nothing else has a border unless it is a control.
- **Elevation:** only floating things cast a shadow: menus, the palette,
  sheets. Cards on the page are flat.
- **Motion:**
  - **Desktop:** springs (Rune's snappy, 0.25 s, for focus and tabs; calm,
    0.4 s, for theme changes).
  - **Web:** at most 180 ms, ease-out.
  - **Never:** terminal content or text that is being read is never animated.
  - **Reduced motion:** honour it everywhere.

## Components

Each is a device with a meaning. Use it for that meaning only.

### Frame

A rounded box with its title riding the top border, written `[ title ]`, with
the brackets in the frame's colour.

- **In the product:** a pane, with its tabs riding the border.
- **On a page:** the one thing that needs a decision or holds the conclusion.
- **Focused** frames use the accent at 1.5 px. **Quiet** frames use `--line`
  at 1 px, with brackets in `--text-muted`.
- **No more than one focused frame per screen.**

```html
<div class="frame"><span class="frame-title">decisions needed</span> … </div>
```

### Band

The gold title band. It heads one list or table per view: the name on the left
and the count on the right, in mono. A section band (`--line` fill, muted text)
groups rows beneath it. It is never a call to action.

### State marks

Glyphs come from the console. They always appear with a word nearby, or as a
row's leading mark.

| Box | Glyph | Colour |
| --- | --- | --- |
| running | ● | `--ok` |
| starting, repairing | ◐ | `--gold` |
| suspended | ‖ | `--gold` |
| stopped | ○ | `--text-faint` |
| archived | ▪ | `--text-faint` |
| error | ✗ | `--err` |

| Agent | Glyph | Colour |
| --- | --- | --- |
| working | ▶ | `--text` |
| idle | ◇ | `--text-faint` |
| blocked | ◆ | `--err` |
| done | ✓ | `--green` |
| failed | × | `--err` |

### Keys

- **Shortcuts** are set as keycaps: mono, a 1 px border with a 2 px bottom
  edge, 4 px radius.
- **On desktop and the web,** a shortcut is written in the platform's
  spelling: `⌘T` on macOS, `Ctrl+Shift+T` elsewhere.
- **In the TUI** it is written as the console prints it: `ctrl+a s`.

### Chip

A small outlined label in mono, `--step--1`, 4 px radius, its border in its
text colour. Use it for tags, waves and phases. It is never filled with the
accent.

### Option

A card per alternative in a decision.
- The chosen one gets the accent border and a "Recommended" or "Chosen"
  label.
- Rejected ones stay quiet and say why in one sentence.

### Timeline

A vertical rule with a ring per step, for real sequences only. A step gets an
accent ring. A track that runs in parallel gets a dashed ring in the muted
colour. Each step can end with a line that starts "Done when".

### Wire

A sunken block in mono for protocols, commands and file shapes. Keywords are in
the accent; nothing else is coloured.

### Eyebrow

A mono label above a heading that names the section's kind: Scope, Decision,
Inventory. It is never a number unless the sections are steps.

## Across surfaces

| Device | TUI | Desktop | Web and documents |
| --- | --- | --- | --- |
| Frame | box-drawing, `[ title ]` in the border | vector stroke, tab chips set into the edge | CSS border, title chip over the edge |
| Band | 220 background, 232 text | filled rounded rect | `.band` |
| Focus | `#f45cff` border | accent stroke, spring | accent outline, 2 px offset |
| Hints | dim hint line of leader keys | menus, tooltips, palette shortcut column | keycaps inline in prose |
| Mark | block-element capture (`logo.json`) | vector mark | `logo.svg` / `wordmark-*.svg` |
| Type | the terminal's font | JetBrains Mono, bundled | JetBrains Mono and IBM Plex Sans |
| Theme | dark (the terminal's) | dark and light, following the system | dark and light, following the system |

## Writing

- **Name things by what people recognise:** a box, a shell, a session. Not an
  exec or a binding ID, unless the reader is configuring one.
- **A control says exactly what it does** (Start, Archive), and its result
  confirms in the same words (Started).
- **An error explains what happened and what to do,** without apology.
- **Short, direct sentences.** No asides set off by dashes, no scare quotes,
  no stock phrases.

## Using the tokens

- **Terminal pictures:** put `.theme-dark` on a terminal window or capture, and
  everything inside it reads the dark roles in either theme.
- **Web:** copy [`tokens.css`](tokens.css). The site does this with
  `pnpm sync:assets`, like the brand art. Its custom property names are the
  site's existing ones.
- **Desktop:** the console core hands the colour tokens to the app in its spec
  (ADR 26-11-07-363 §4), read from [`tokens.json`](tokens.json).
- **TUI:** uses the `xterm` value of each colour token in `tokens.json`.

Change a value here, never in a consumer. A consumer that needs a colour this
file does not name asks for a new role here first.
