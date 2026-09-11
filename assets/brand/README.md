# Brand assets

The source art for the discobox mark, copied out of the discobot repository so
this repo does not depend on a sibling checkout. These are the originals: any
rendered form — the TUI mark, a favicon, an icon — is derived from one of the
SVGs here, not hand-edited.

| File | What it is |
| --- | --- |
| `logo-purple.svg` | the mark alone, `#9e4aa7`. The one the terminal captures are rendered from. |
| `logo-black.svg`, `logo-white.svg` | the same mark, single-color, for light and dark grounds |
| `wordmark-gradient.svg` | mark plus wordmark: the mark in `#9e4aa7`, the letters in a gradient |
| `wordmark-black.svg`, `wordmark-white.svg` | mark plus wordmark: the mark in `#9e4aa7`, the letters black or white |
| `favicon.svg` | the mark on a 512×512 canvas, ready to rasterize |
| `favicon.png`, `favicon-32x32.png`, `favicon-128x128.png`, `favicon.ico` | rasterizations of it (512, 32, 128, and a 16/32 icon) |
| `logo-80col.ansi` | the mark as a large truecolor terminal capture, 158 columns by 79 rows |

The `logo-*.svg` files came out of Illustrator and share the viewBox
`123.4 193.7 171.4 176.9`; the single-color variants differ from the purple only
in their `fill`. The wordmarks have their own viewBoxes, with origin `0 0`.
`favicon.svg` is the mark's paths translated and scaled onto a square canvas.

## Who reads them

- The root `README.md` shows `wordmark-gradient.svg`.
- `sandbox-agent/Dockerfile` copies the directory to
  `/usr/local/share/discobox/brand`, which the desktop viewer serves as
  `/brand/`; the desktop wallpaper uses `wordmark-white.svg` and the viewer's
  mark is rasterized from `logo-purple.svg`.
- The marketing site (`discobox-ai/site`) syncs this art with its own
  `pnpm gen:logo`.

## The TUI mark

`cli/internal/tui/logo.chars` is a much smaller capture of this mark: 25 columns
of block characters in 16-color indices with inverse-video runs. It is kept as
provenance and is not drawn directly. `go tool task logo:cells` runs
`scripts/logo-cells.mjs`, which turns it into `cli/internal/tui/logo.json`: the
indices become explicit brand RGB and the inverse runs become explicit
backgrounds. `logo.json` is what the CLI embeds and draws beside the discobox
list and on the welcome screen. It is dropped on a colorless terminal and on
one narrower than 100 columns. See `cli/internal/tui/logo.go`.

To re-render it, rasterize `logo-purple.svg`, re-capture at the target width
into `logo.chars`, and run `go tool task logo:cells`. `logo-80col.ansi` is a
larger capture of the same mark and shows what it looks like with room to
breathe.
