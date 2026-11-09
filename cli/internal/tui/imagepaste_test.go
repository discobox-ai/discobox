package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/discobox-ai/discobox/cli/internal/clipimage"
	"github.com/discobox-ai/discobox/cli/internal/clipwatch"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// input is everything the pane has typed at the terminal so far.
func (f *fakeTerminal) inputSoFar() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.input)
}

// Ctrl-V on a harness terminal with an image on the clipboard carries the
// image into the discobox and pastes where it put it, instead of the key —
// which would only have the agent read the discobox's own empty clipboard.
func TestCtrlVCarriesTheClipboardImageIntoTheBox(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"ctrl+v", "alt+v"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ds := newFakeSource(testSandboxes()...)
				d, m, term := openWorkspace(t, ds, "enter")
				m.readClipboard = func(context.Context) ([]clipimage.Image, error) {
					return []clipimage.Image{{Name: "clipboard.png", Data: []byte("pixels")}}, nil
				}

				d.key(key)
				d.wait("the path pasted", func() bool { return strings.Contains(term.inputSoFar(), "/tmp/discobox-paste/") })

				puts := ds.putImagesSoFar()
				if len(puts) != 1 || puts[0].sandboxID != m.Primary().sandbox.ID || string(puts[0].data) != "pixels" ||
					!strings.HasSuffix(puts[0].name, "-clipboard.png") {
					t.Fatalf("put %+v, want the clipboard's image into the discobox on screen", puts)
				}
				if got := term.inputSoFar(); got != "/tmp/discobox-paste/"+puts[0].name {
					t.Fatalf("typed %q, want only the path the image was put at", got)
				}
			})
		})
	}
}

// With no image on the clipboard the key is the key: Ctrl-V reaches the
// program as it was typed.
func TestCtrlVWithNoImageIsTheKey(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, _, term := openWorkspace(t, ds, "enter")

		d.key("ctrl+v")
		d.wait("the key", func() bool { return term.inputSoFar() == "\x16" })
		if puts := ds.putImagesSoFar(); len(puts) != 0 {
			t.Fatalf("put %+v with nothing on the clipboard", puts)
		}
	})
}

// A shell's Ctrl-V is quoted-insert, and vi's is visual block: the clipboard is
// not asked, and the key goes straight through.
func TestCtrlVInAShellIsNotAnImagePaste(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, _ := openWorkspace(t, ds, "s")
		d.wait("the shell", func() bool { return m.Shells.Len() == 1 })
		var asked atomic.Int32
		m.readClipboard = func(context.Context) ([]clipimage.Image, error) {
			asked.Add(1)
			return []clipimage.Image{{Name: "clipboard.png", Data: []byte("pixels")}}, nil
		}
		shell := ds.execTerm("exec_shell1")

		d.key("ctrl+v")
		d.wait("the key", func() bool { return shell.inputSoFar() == "\x16" })
		if asked.Load() != 0 {
			t.Fatal("a shell's Ctrl-V asked the clipboard")
		}
	})
}

// A file dragged onto the window arrives as its path on this machine, which
// the discobox cannot open. The file is carried in and its path there pasted
// instead.
func TestAPastedImagePathIsCarriedIntoTheBox(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, _, term := openWorkspace(t, ds, "enter")
		local := filepath.Join(t.TempDir(), "my shot.png")
		if err := os.WriteFile(local, []byte("\x89PNG\r\n\x1a\npixels"), 0o600); err != nil {
			t.Fatal(err)
		}

		d.dispatch(tea.PasteMsg{Content: "'" + local + "'"})
		d.wait("the path pasted", func() bool { return strings.Contains(term.inputSoFar(), "/tmp/discobox-paste/") })

		puts := ds.putImagesSoFar()
		if len(puts) != 1 || string(puts[0].data) != "\x89PNG\r\n\x1a\npixels" || !strings.HasSuffix(puts[0].name, "-my-shot.png") {
			t.Fatalf("put %+v, want the dragged file", puts)
		}
		if strings.Contains(term.inputSoFar(), local) {
			t.Fatalf("typed %q: the local path reached the discobox", term.inputSoFar())
		}
	})
}

// Text that only mentions an image goes through as it was pasted, at once.
func TestAPasteOfProseIsNotHeld(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, _, term := openWorkspace(t, ds, "enter")

		d.dispatch(tea.PasteMsg{Content: "see /tmp/a.png for the bug"})
		d.wait("the paste", func() bool { return term.inputSoFar() == "see /tmp/a.png for the bug" })
		if puts := ds.putImagesSoFar(); len(puts) != 0 {
			t.Fatalf("put %+v for a paste of prose", puts)
		}
	})
}

// What is typed while an image is on its way arrives after it, not in front of
// it.
func TestKeysTypedDuringAnImagePasteArriveAfterIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")
		gate := make(chan struct{})
		m.readClipboard = func(context.Context) ([]clipimage.Image, error) {
			<-gate
			return []clipimage.Image{{Name: "clipboard.png", Data: []byte("pixels")}}, nil
		}

		d.key("ctrl+v")
		d.key("x")
		d.dispatch(tea.PasteMsg{Content: "y"})
		if got := term.inputSoFar(); got != "" {
			t.Fatalf("typed %q while the image was still being read", got)
		}
		close(gate)
		d.wait("everything", func() bool { return strings.HasSuffix(term.inputSoFar(), "xy") })
		if got := term.inputSoFar(); !strings.HasPrefix(got, "/tmp/discobox-paste/") {
			t.Fatalf("typed %q, want the image's path first", got)
		}
	})
}

// The leader is the window's, not the pane's: a slow copy does not hold it, or
// there would be no way to quit, detach or move while one runs.
func TestTheLeaderIsNotHeldBehindAnImagePaste(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		ds.audit = make(chan console.AuditUpdate)
		d, m, term := openWorkspace(t, ds, "enter")
		gate := make(chan struct{})
		defer close(gate)
		m.readClipboard = func(context.Context) ([]clipimage.Image, error) {
			<-gate
			return nil, nil
		}

		d.key("ctrl+v")
		d.key("ctrl+a")
		d.key(auditKey)
		d.wait("the audit screen", func() bool { return m.audit != nil })
		if got := term.inputSoFar(); got != "" {
			t.Fatalf("typed %q: the leader chord reached the program", got)
		}
	})
}

// An image that could not be carried in is said, and nothing is pasted.
func TestAnImageThatCannotBeCarriedInIsReported(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		ds.putImageErr = errors.New("copy clipboard.png into the discobox: disk full")
		d, m, term := openWorkspace(t, ds, "enter")
		m.readClipboard = func(context.Context) ([]clipimage.Image, error) {
			return []clipimage.Image{{Name: "clipboard.png", Data: []byte("pixels")}}, nil
		}

		d.key("ctrl+v")
		d.wait("the failure", func() bool { return strings.Contains(frameText(m), "disk full") })
		d.key("z")
		d.wait("the next key", func() bool { return term.inputSoFar() == "z" })
	})
}

func TestImageNameIsSafeToPaste(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 15, 30, 12, 0, time.UTC)
	for name, want := range map[string]string{
		"clipboard.png":   "20261016-153012.000-0-clipboard.png",
		"my shot (1).PNG": "20261016-153012.000-0-my-shot--1-.PNG",
		"$(rm -rf ~).png": "20261016-153012.000-0-rm--rf---.png",
		"..":              "20261016-153012.000-0-image.png",
	} {
		if got := imageName(name, at, 0); got != want {
			t.Errorf("imageName(%q) = %q, want %q", name, got, want)
		}
	}
}

// A dictation tool writes what it heard to the clipboard, presses Ctrl-V, and
// puts the clipboard back half a second later. The text the watch saw land
// just before the key is what is pasted.
func TestCtrlVJustAfterTextLandsPastesTheText(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")
		var asked atomic.Int32
		m.readClipboard = func(context.Context) ([]clipimage.Image, error) {
			asked.Add(1)
			return nil, nil
		}
		m.clipWatch = clipwatch.NewWatcher()
		m.clipWatch.Record("old clipboard", false)
		time.Sleep(10 * time.Millisecond)
		m.clipWatch.Record("what I said ", false)

		d.key("ctrl+v")
		d.wait("the dictation", func() bool { return term.inputSoFar() == "what I said " })
		if asked.Load() != 0 || len(ds.putImagesSoFar()) != 0 {
			t.Fatal("a dictation was read as an image paste")
		}
	})
}

// The key can overtake the watch's notice of the write before it: a change
// that lands just after the press is still the one the press was for.
func TestCtrlVWaitsBrieflyForTheWatchToCatchUp(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")
		m.clipWatch = clipwatch.NewWatcher()
		m.clipWatch.Record("old clipboard", false)
		time.Sleep(dictationWindow + 50*time.Millisecond)

		d.dispatch(keyPress("ctrl+v"))
		// The notice lands once the key is waiting for it, not before.
		synctest.Wait()
		m.clipWatch.Record("late notice ", false)
		d.wait("the dictation", func() bool { return term.inputSoFar() == "late notice " })
	})
}

// A Ctrl-V pressed by hand, with nothing just copied and no image, goes through
// as the key — answered from the watch, without reading the clipboard.
func TestCtrlVWithAStaleClipboardIsTheKey(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")
		var asked atomic.Int32
		m.readClipboard = func(context.Context) ([]clipimage.Image, error) {
			asked.Add(1)
			return nil, nil
		}
		m.clipWatch = clipwatch.NewWatcher()
		m.clipWatch.Record("copied a while ago", false)
		m.now = func() time.Time { return time.Now().Add(2 * dictationWindow) }

		d.key("ctrl+v")
		d.wait("the key", func() bool { return term.inputSoFar() == "\x16" })
		if asked.Load() != 0 {
			t.Fatal("the clipboard was read with the watch saying there was nothing on it")
		}
	})
}

// An image on the clipboard is still an image paste with the watch running.
func TestCtrlVWithAnImageOnAWatchedClipboardCarriesItIn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")
		m.readClipboard = func(context.Context) ([]clipimage.Image, error) {
			return []clipimage.Image{{Name: "clipboard.png", Data: []byte("pixels")}}, nil
		}
		m.clipWatch = clipwatch.NewWatcher()
		m.clipWatch.Record("", true)

		d.key("ctrl+v")
		d.wait("the path pasted", func() bool { return strings.Contains(term.inputSoFar(), "/tmp/discobox-paste/") })
	})
}

// A leader on an image key is the leader: under alt+v, Alt-V in a harness
// terminal arms it rather than pasting, so leader-q still leaves the pane that
// matters most.
func TestALeaderOnAnImageKeyIsTheLeader(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		m := New(t.Context(), ds, WithLeader("alt+v"))
		m.logo = logo{}
		var asked atomic.Int32
		m.readClipboard = func(context.Context) ([]clipimage.Image, error) {
			asked.Add(1)
			return []clipimage.Image{{Name: "clipboard.png", Data: []byte("pixels")}}, nil
		}
		d := newDriver(t, m)
		d.start()
		d.wait("the list", func() bool { return m.focus == focusList })
		d.key("enter")
		d.wait("the pane", func() bool { return m.focus == focusPane })

		d.key("alt+v")
		d.key("q")
		if !m.quit {
			t.Fatal("alt+v q should quit the window under an alt+v leader")
		}
		if asked.Load() != 0 {
			t.Fatal("the leader was taken as an image paste")
		}
	})
}

// The report of a copy that failed at once is not overwritten by anything
// else the paste said: Bubble Tea runs a batch's commands on goroutines of
// their own, so the failure may arrive before any of them. Neither the copy's
// progress nor, on Windows, why the clipboard watch is off may land on top.
func TestAnImageCopyFailureOutlastsItsProgress(t *testing.T) {
	t.Parallel()
	for name, watchErr := range map[string]error{
		"progress":     nil,
		"watch is off": errors.New("clipwatch helper exited"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ds := newFakeSource(testSandboxes()...)
				ds.putImageErr = errors.New("copy clipboard.png into the discobox: disk full")
				_, m, _ := openWorkspace(t, ds, "enter")
				p := m.Primary()
				p.imageBusy = true

				cmd := m.imagesRead(imageReadMsg{
					pane: p.id, original: keyPress("ctrl+v"), watchErr: watchErr,
					images: []clipimage.Image{{Name: "clipboard.png", Data: []byte("pixels")}},
				})
				// The copy's failure, and its report, come first; then the rest
				// of the batch: an order a batch may run in. Only status lines
				// are delivered, as they are what is being raced.
				var put imagePutMsg
				var rest []tea.Msg
				for _, msg := range runAll(cmd) {
					if msg, ok := msg.(imagePutMsg); ok {
						put = msg
						continue
					}
					rest = append(rest, msg)
				}
				for _, msg := range append(runAll(m.imagesPut(put)), rest...) {
					if msg, ok := msg.(statusMsg); ok {
						m.Update(msg)
					}
				}
				if !m.statusE || !strings.Contains(m.status, "disk full") {
					t.Fatalf("status = %q (error %v), want the copy's failure", m.status, m.statusE)
				}
			})
		})
	}
}

// Why the clipboard watch is off is said once, on a press that has nothing
// else to say, and a press that has its own report leaves it to the next.
func TestTheWatchBeingOffIsSaidOnAQuietPress(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")
		watchErr := errors.New("clipwatch helper exited")
		p := m.Primary()

		p.imageBusy = true
		d.run(m.imagesRead(imageReadMsg{pane: p.id, original: keyPress("ctrl+v"), watchErr: watchErr, err: errors.New("clipboard locked")}))
		d.wait("the read's failure", func() bool { return strings.Contains(m.status, "clipboard locked") })
		if m.clipWatchReported {
			t.Fatal("the watch's notice was spent on a press that reported its own failure")
		}

		p.imageBusy = true
		d.run(m.imagesRead(imageReadMsg{pane: p.id, original: keyPress("ctrl+v"), watchErr: watchErr}))
		d.wait("the key", func() bool { return strings.Count(term.inputSoFar(), "\x16") == 2 })
		if !m.statusE || !strings.Contains(m.status, "dictation through Ctrl-V is off: clipwatch helper exited") {
			t.Fatalf("status = %q (error %v), want why dictation is off", m.status, m.statusE)
		}
	})
}

// runAll runs a command and every command its batches hold, and returns what
// each one returned.
func runAll(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var msgs []tea.Msg
	for _, sub := range batch {
		msgs = append(msgs, runAll(sub)...)
	}
	return msgs
}
