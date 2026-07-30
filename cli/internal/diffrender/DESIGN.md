# diffrender Design

Parses a unified diff and lays it out for a terminal. It knows nothing about
sandboxes, git invocation, or the API: input is patch text, output is styled
lines. `disco diff` is the only caller today; the TUI is the obvious next one.

Two stages, deliberately separate:

- `Parse` turns patch text into `[]File` — hunks, per-line kinds, and the line
  numbers a unified diff only implies via `@@` headers. It is tolerant by
  design: unrecognized input is skipped rather than rejected, because a
  rendering command that refuses output a human could have read is worse than
  one that renders it imperfectly.
- `Render` writes those files out. Everything about presentation lives here, so
  a second front end can reuse the parse and choose its own layout.

## Layout

```
<line number> <sign><content, padded to the right margin>
```

- The line number sits *outside* the colored band: it is not part of the file.
  The band starts at the sign so a changed line reads as one solid block rather
  than as ragged text — this is the single biggest difference from `git diff`'s
  look, and it is why content is padded to the terminal width.
- Only changed lines are padded. Padding context lines would put trailing
  whitespace into everything the reader copies out.
- The sign column (` `, `+`, `-`) is not decoration: with `Color: false` it
  carries the entire meaning, which is what makes `NO_COLOR` and a dumb terminal
  work.
- Within a changed pair, the common prefix and suffix are trimmed and only the
  differing span is highlighted. Past `maxEmphasisFraction` of the line the
  highlight is dropped: emphasizing nearly everything says less than the
  background color already did.
- Emphasis, wrapping, and padding are all measured in columns, so tabs are
  expanded *before* any of them run. Measuring the raw text and rendering the
  expanded text silently shifts every highlight.
- Long lines wrap with the sign repeated, rather than truncating: a diff that
  hides the end of a line is a diff that can mislead.

## Syntax Highlighting

The code inside the diff is highlighted too, with `chroma`. The two coloring
channels are orthogonal and are combined, never chosen between: **background**
says what the diff did to the line (added, removed, emphasized), **foreground**
says what the code is. `styleRuns` resolves both at every rune and cuts the line
into runs wherever the rendered style actually changes.

- The lexer is fed **one hunk side at a time** — context plus removed lines as
  the old text, context plus added lines as the new — not one line at a time. A
  lexer is a state machine; per line it restarts in the initial state and
  mis-colors everything that spans lines (block comments, multi-line strings,
  heredocs). A hunk that itself opens inside a block comment still starts wrong;
  fixing that needs whole files, which costs a round trip per file and cannot
  recover the old side at all.
- `lexers.Match` on the file name, and **no fallback lexer**: a fallback returns
  the text as one plain token, so it costs a tokenise pass to produce what not
  highlighting already produces.
- The palette is short and hand-picked rather than one of chroma's styles,
  because these foregrounds have to stay legible *on top of* the diff bands — a
  style tuned for a black terminal will paint a string green and drop it onto
  the green band of an added line.
- Plain identifiers are deliberately uncolored. Coloring every name as well as
  every keyword, type, literal, and comment leaves almost nothing uncolored, and
  a line where everything is emphasized emphasizes nothing.

## Color

- Styles are `lipgloss` with 256-color indexes, matching the rest of the CLI.
  The caller wraps its writer in a `colorprofile.Writer`, which downsamples for
  a 16-color terminal and strips for a dumb one, so nothing here asks what the
  terminal can do.
- `Options.Dark` picks between a dark and a light palette. The caller decides
  it; this package does not query the terminal.
- Backgrounds carry the line's state and foregrounds are left alone, so the
  terminal's own theme still governs the code text.
