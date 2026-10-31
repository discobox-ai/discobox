package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/discobox-ai/discobox/cli/internal/clipimage"
	"github.com/discobox-ai/discobox/cli/internal/clipwatch"
)

// An agent in a discobox asked to take a screenshot has nowhere to take it
// from. The clipboard it would read is the discobox's, which is empty, and a
// path dragged onto the window names a file on this machine, which the
// discobox cannot open. So a paste into a pane that means an image is read
// here and carried in: the image is written into the discobox (PutImage), and
// what reaches the program is the path it was written to, pasted the way a
// terminal pastes a dragged file — which is what every agent already takes as
// an image.
//
// Three things mean an image:
//
//   - Ctrl-V, or Alt-V, on a harness terminal, with an image on the clipboard.
//     Or with text that reached the clipboard a moment before, which is a
//     dictation tool pasting what it heard: that is pasted as the text it is
//     (readKey).
//     Those are the keys the agents bind to "paste an image": Ctrl-V where the
//     terminal's own paste is Cmd-V or Ctrl-Shift-V, Alt-V on Windows, where
//     the terminal takes Ctrl-V. With no image there, the key goes through as
//     it was typed. Shells are left out: Ctrl-V is quoted-insert there and
//     visual-block in vi, and asking the clipboard first would put a
//     PowerShell start-up in front of every press on WSL.
//   - An empty paste, on any of the discobox's sessions. It is what a terminal
//     sends when its own paste found no text — the clipboard has an image on
//     it — and nothing else ever sends one.
//   - A paste that is nothing but paths to image files on this machine: a file
//     dragged onto the window, or a path copied from a file manager.
//
// While one is being read and carried in, the pane's keys and pastes are held
// and delivered after it, in order, so what was typed behind a paste does not
// arrive in front of it. The leader and the key after it are not held: they
// are the window's own commands, quit and detach among them. A leader chord
// that turns out to bind nothing is typed at once, ahead of what is held.

// imagePutTimeout bounds carrying a paste's images into the discobox. It is
// long, because a screenshot over a slow link is megabytes, but not forever:
// the pane's keys are held while it runs.
const imagePutTimeout = 2 * time.Minute

// imageReadMsg is what a paste turned out to mean: the images it named or the
// clipboard held, none, or why it could not be read. original is the key or
// paste it was, delivered as it came when it meant no image.
type imageReadMsg struct {
	pane     int
	original tea.Msg
	images   []clipimage.Image
	// text is a dictation tool's text, pasted in place of its Ctrl-V.
	text string
	// watchErr is why the clipboard watch is not running where it should be,
	// for the console to say once (Model.clipWatchReported).
	watchErr error
	err      error
}

// imagePutMsg is the images carried into the discobox: the paths they are at
// there, or why they are not.
type imagePutMsg struct {
	pane  int
	paths []string
	err   error
}

// imageKey reports whether a key is one the agents bind to pasting an image.
func imageKey(msg tea.KeyPressMsg) bool {
	name := msg.String()
	return name == "ctrl+v" || name == "alt+v"
}

// boxSession reports whether a pane is typing at a session in a discobox: the
// one kind of pane an image can be carried to.
func boxSession(p *pane) bool {
	return p.execID != "" && p.service == "" && p.issue == nil && p.sandbox.ID != ""
}

// harnessTerminal reports whether a pane is one of the discobox's harness
// terminals, the panes the image keys are taken on.
func (m *Model) harnessTerminal(p *pane) bool {
	column, _ := m.paneColumn(p)
	return column == &m.terminals && boxSession(p) && p.tool == ""
}

// imageInput takes a key or a paste that may mean an image, and holds the
// pane's input while one is on its way. It reports whether it took msg.
func (m *Model) imageInput(p *pane, msg tea.Msg) (tea.Cmd, bool) {
	if !boxSession(p) {
		return nil, false
	}
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if p.imageBusy {
			// The leader, and the key that follows it, are the window's: held,
			// a slow copy would leave no way to quit, detach or move to
			// another pane until it ended.
			if keyName(msg) == m.leader() || p.term.PrefixPending() {
				return m.toTerm(p, msg), true
			}
			p.imageHeld = append(p.imageHeld, msg)
			return nil, true
		}
		if !imageKey(msg) || p.term.PrefixPending() || !m.harnessTerminal(p) {
			return nil, false
		}
		return m.readKey(p, msg), true
	case tea.PasteMsg:
		if p.imageBusy {
			p.imageHeld = append(p.imageHeld, msg)
			return nil, true
		}
		if strings.TrimSpace(msg.Content) == "" {
			return m.readImages(p, msg, m.readClipboard), true
		}
		// Prose that mentions a.png goes through untouched, and without
		// waiting: only a paste whose every word is an image name is held
		// for the filesystem to be asked.
		if clipimage.LooksLikePaths(msg.Content) {
			text := msg.Content
			return m.readImages(p, msg, func(ctx context.Context) ([]clipimage.Image, error) {
				return clipimage.Paths(ctx, text)
			}), true
		}
	}
	return nil, false
}

// dictationWindow is how recently text must have reached the clipboard for a
// Ctrl-V to be taken as a dictation tool pasting it. The tools press the key
// tens of milliseconds after writing; a second is far past that, and short
// enough that a Ctrl-V pressed by hand is not suddenly a paste of whatever
// was last copied.
const dictationWindow = time.Second

// dictationWait is how long a Ctrl-V waits for the clipboard watch to report a
// change it has not reported yet: the key can overtake the notice of the write
// that came before it.
const dictationWait = 150 * time.Millisecond

// readKey holds the pane's input and works out what an image key meant.
//
// Where the Windows clipboard is watched (clipwatch), the watch answers first,
// from what it saw when the clipboard last changed. Text that arrived just
// before the key is a dictation tool's, pasted by pressing Ctrl-V, and it is
// pasted as text: the tool puts the old clipboard back within half a second,
// so a read started now would find that instead. An image goes on to be read
// and carried in. Anything else is a key pressed by hand, which goes through
// at once — without the PowerShell read that would find nothing.
func (m *Model) readKey(p *pane, original tea.KeyPressMsg) tea.Cmd {
	p.imageBusy = true
	ctx, id, watch, readClipboard, pressed := m.ctx, p.id, m.clipWatch, m.readClipboard, m.now()
	return func() tea.Msg {
		if watch.Live() {
			snap, fresh := watch.Await(ctx, pressed.Add(-dictationWindow), dictationWait)
			switch {
			case snap.Image:
			case fresh && snap.Text != "":
				return imageReadMsg{pane: id, original: original, text: snap.Text}
			default:
				return imageReadMsg{pane: id, original: original}
			}
		}
		var watchErr error
		if err := watch.Err(); !errors.Is(err, clipwatch.ErrUnsupported) {
			watchErr = err
		}
		images, err := readClipboard(ctx)
		return imageReadMsg{pane: id, original: original, images: images, watchErr: watchErr, err: err}
	}
}

// readImages holds the pane's input and asks read what original meant.
func (m *Model) readImages(p *pane, original tea.Msg, read func(context.Context) ([]clipimage.Image, error)) tea.Cmd {
	p.imageBusy = true
	ctx, id := m.ctx, p.id
	return func() tea.Msg {
		images, err := read(ctx)
		return imageReadMsg{pane: id, original: original, images: images, err: err}
	}
}

// imagesRead carries what a paste meant into the discobox, or lets the paste
// through as it came when it meant no image.
func (m *Model) imagesRead(msg imageReadMsg) tea.Cmd {
	p := m.heldPane(msg.pane)
	if p == nil {
		return nil
	}
	if msg.watchErr != nil && !m.clipWatchReported {
		m.clipWatchReported = true
		cmd := m.imagesRead(imageReadMsg{pane: msg.pane, original: msg.original, images: msg.images, text: msg.text, err: msg.err})
		return tea.Batch(cmd, m.report(true, "dictation through Ctrl-V is off: %v", msg.watchErr))
	}
	if msg.text != "" {
		return m.releaseImageInput(p, tea.PasteMsg{Content: msg.text})
	}
	if msg.err != nil || len(msg.images) == 0 {
		cmd := m.releaseImageInput(p, msg.original)
		switch {
		case errors.Is(msg.err, clipimage.ErrUnavailable):
			// Only the image keys ask a clipboard there is no reader for,
			// and they still went through; this says why nothing else did.
			return tea.Batch(cmd, status("no image pasted: reading the clipboard here needs wl-paste or xclip"))
		case msg.err != nil:
			return tea.Batch(cmd, m.report(true, "cannot read the pasted image: %v", msg.err))
		}
		return cmd
	}
	ds, sandboxID, id := m.ds, p.sandbox.ID, p.id
	ctx, cancel := context.WithTimeout(m.ctx, imagePutTimeout)
	images, now := msg.images, m.now()
	put := func() tea.Msg {
		defer cancel()
		paths := make([]string, 0, len(images))
		for i, image := range images {
			path, err := ds.PutImage(ctx, sandboxID, imageName(image.Name, now, i), image.Data)
			if err != nil {
				return imagePutMsg{pane: id, err: err}
			}
			paths = append(paths, path)
		}
		return imagePutMsg{pane: id, paths: paths}
	}
	return tea.Batch(put, status("copying %s into %s…", imageCount(len(images)), p.sandbox.Name))
}

// imagesPut pastes the paths the images were written to, in place of the
// paste that named them. One that could not be carried in is reported and
// nothing is pasted: the key behind it would only have the agent read the
// discobox's own empty clipboard.
func (m *Model) imagesPut(msg imagePutMsg) tea.Cmd {
	p := m.heldPane(msg.pane)
	if p == nil {
		return nil
	}
	if msg.err != nil {
		return tea.Batch(m.releaseImageInput(p, nil), m.report(true, "%v", msg.err))
	}
	paste := tea.PasteMsg{Content: strings.Join(msg.paths, " ")}
	return tea.Batch(m.releaseImageInput(p, paste), status("pasted %s", imageCount(len(msg.paths))))
}

// releaseImageInput delivers first to the pane's terminal, then everything that
// was held behind it, in the order it came. A held key that is itself an
// image paste starts another read, and holds the rest again behind it.
func (m *Model) releaseImageInput(p *pane, first tea.Msg) tea.Cmd {
	p.imageBusy = false
	held := p.imageHeld
	p.imageHeld = nil
	if p.exited {
		return nil
	}
	var cmds []tea.Cmd
	if first != nil {
		cmds = append(cmds, m.toTerm(p, first))
	}
	for i, msg := range held {
		if p.imageBusy {
			p.imageHeld = append(p.imageHeld, held[i:]...)
			break
		}
		cmds = append(cmds, m.paneInput(p, msg))
	}
	return tea.Batch(cmds...)
}

// imageName is the name an image is written under in the discobox: when it was
// pasted, which keeps two pastes of clipboard.png apart, and its own name cut
// to an alphabet that needs no quoting wherever the path is pasted.
func imageName(name string, now time.Time, i int) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '-'
	}, name)
	safe = strings.Trim(safe, ".-")
	if safe == "" {
		safe = "image.png"
	}
	return fmt.Sprintf("%s-%d-%s", now.Format("20060102-150405.000"), i, safe)
}

func imageCount(n int) string {
	if n == 1 {
		return "the image"
	}
	return fmt.Sprintf("%d images", n)
}
