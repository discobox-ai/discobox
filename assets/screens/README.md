# Screenshots

The launcher, captured from a real run rather than mocked up. The root
`README.md` shows `claude-code.png`; nothing else in this repository reads them.

| File | Screen |
| --- | --- |
| `welcome.png` | the introduction, shown once per project — the mark and the three steps |
| `launcher.png` | the discobox list, with the prompt a new one starts from |
| `claude-code.png` | one discobox open: Claude Code, and a shell beside it |
| `codex.png` | the same, running Codex |
| `tools.png` | the tools menu, `ctrl+a o` under the default leader |
| `review.png` | the built-in diff and approval tool, on what the agent just wrote |

## Re-taking one

Run the launcher in a real terminal on a headless X display and photograph the
screen. `tmux` is in the middle only so the screens can be driven from outside
the display.

```bash
Xvfb :99 -screen 0 3000x1700x24 &
DISPLAY=:99 kitty --config kitty.conf bash -c "tmux new-session -A -s K -x 132 -y 23 discobox" &
tmux set -t K -g status off        # the status bar is not part of the shot
tmux send-keys -t K Escape; tmux send-keys -t K Up
DISPLAY=:99 import -window root raw.png
magick raw.png -trim +repage -bordercolor '#12101a' -border 28 launcher.png
```

**The terminal has to be kitty, or something else that draws block characters
itself.** The mark is drawn in eighth-blocks that have to tile seamlessly, and a
terminal that takes those glyphs from the font gets them back sized to the
font's metrics rather than the cell's — so they land a fraction off and the mark
comes out with hairline seams through it and its rows offset from each other.
kitty renders them procedurally, at exact cell boundaries. xterm does not, and
`freeze` — which is a text renderer, not a terminal, and positions runs at
fractional advances — is worse again.

The window size is the crop: `initial_window_width`/`initial_window_height` in
the `kitty.conf` you pass (the repository ships none), in cells, sized to the
screen being shot, then `-trim` takes the rest.

Two crops, not one. The first `-trim` takes off the desktop around the terminal
window; a second one takes off the terminal's own empty area, which is what a
dialog like `tools.png` is mostly surrounded by. A screen that fills its window
only needs the first.

`discobox admin project update <project> --welcomed=false` shows the welcome
screen again, which is otherwise a once-per-project screen and cannot be
re-opened.
