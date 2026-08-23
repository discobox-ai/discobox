# Brand assets

The source art for the discobox mark, copied out of the discobot repository so
this repo does not depend on a sibling checkout. These are the originals: any
rendered form — the TUI mark, a favicon, an icon — is derived from one of the
SVGs here, not hand-edited.

| File | What it is |
| --- | --- |
| `logo-purple.svg` | the mark alone, `#9e4aa7`. The one everything terminal-side derives from. |
| `logo-black.svg`, `logo-white.svg` | the same mark, single-color, for light and dark grounds |
| `wordmark-gradient.svg` | mark plus wordmark, the gradient treatment |
| `wordmark-black.svg`, `wordmark-white.svg` | mark plus wordmark, single-color |
| `favicon.svg` | the mark on a 512×512 canvas, ready to rasterize |
| `favicon.png`, `favicon-32x32.png`, `favicon-128x128.png`, `favicon.ico` | rasterizations of it |
| `logo-80col.ansi` | the mark as an 80-column truecolor terminal capture |

The mark and wordmark SVGs came out of Illustrator and share a viewBox origin
of `123.4 193.7`; the single-color variants differ from the purple only in
their `fill`. `favicon.svg` is the same paths translated and scaled onto a
square canvas.

## The TUI mark

`cli/internal/tui/logo.chars` is a much smaller capture of this mark — about
thirty columns of 256-color block characters, embedded into the CLI and drawn
beside the discobox list. It carries its own colors and inverse-video runs, so
it is used as captured and dropped entirely on a colorless terminal. See
`cli/internal/tui/logo.go`.

To re-render it, rasterize `logo-purple.svg` and re-capture at the target
width; `logo-80col.ansi` is the same idea at 80 columns and shows what the
mark looks like with room to breathe.
