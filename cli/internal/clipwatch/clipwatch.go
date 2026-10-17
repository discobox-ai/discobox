// Package clipwatch keeps up with what is on the Windows clipboard, for as long
// as the console is open.
//
// It exists for dictation. A dictation tool such as Wispr Flow puts the text
// it heard on the clipboard, presses Ctrl-V, and puts the clipboard back about
// half a second later. Inside a discobox that Ctrl-V reaches an agent with no
// clipboard of its own, so the console has to supply the text — and a read
// started when the key arrives loses the race on WSL, where reaching Windows
// takes longer than the tool waits. So the clipboard is watched instead, and
// the key is answered from what the watch already saw.
//
// The watch is Windows' own clipboard notification (AddClipboardFormatListener
// on a message-only window): it sleeps until something is copied, and costs
// nothing while nothing is. On Windows it runs in this process. On WSL the
// Linux binary cannot call Win32, so it runs as a small Windows helper carried
// inside the binary (cmd/discobox-clipwatch), installed under the Windows
// user's local app data by its own digest. Everywhere else
// there is nothing to watch: reading the clipboard when the key arrives is
// fast enough there.
package clipwatch

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUnsupported is a platform with no Windows clipboard to watch.
var ErrUnsupported = errors.New("clipwatch: no Windows clipboard on this platform")

// MaxText is the most clipboard text carried, in UTF-16 code units. Past it
// the text is reported as absent: nobody dictated a megabyte.
const MaxText = 1 << 20

// Snapshot is what the clipboard held when it last changed.
type Snapshot struct {
	// Text is its text, empty when it holds none (or more than MaxText).
	Text string
	// Image is whether it may hold an image: pixels, or files copied in a
	// file manager, which reading the clipboard then decides among.
	Image bool
	// Changed is when this side saw it change, and zero for what the clipboard
	// already held when the watch started: that was not a change anybody just
	// made.
	Changed time.Time
}

// Watcher holds the latest Snapshot. Its zero value is not live; Start returns
// one that becomes live once the watch reports for the first time.
type Watcher struct {
	mu      sync.Mutex
	live    bool
	err     error
	snap    Snapshot
	changed chan struct{}
	now     func() time.Time
}

// NewWatcher returns a Watcher nothing has reported to yet.
func NewWatcher() *Watcher {
	return &Watcher{changed: make(chan struct{}), now: time.Now}
}

// Record is the clipboard changing to hold text and, or, an image. The first
// record is what it held when the watch started, and is not a change.
func (w *Watcher) Record(text string, image bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var changed time.Time
	if w.live {
		changed = w.now()
	}
	w.live = true
	w.snap = Snapshot{Text: text, Image: image, Changed: changed}
	close(w.changed)
	w.changed = make(chan struct{})
}

// fail records why the watch is not running.
func (w *Watcher) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.live = false
	w.err = err
}

// Err is why the watch is not running, nil while it is or before it has
// started.
func (w *Watcher) Err() error {
	if w == nil {
		return ErrUnsupported
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Live reports whether the watch is running and has reported: until it has,
// nothing it says about the clipboard can be trusted.
func (w *Watcher) Live() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.live
}

// Await returns the latest snapshot, and whether it changed at or after since.
// When it has not yet, it waits up to wait for a change: the key that a
// dictation tool presses can arrive before the notice of the write that
// preceded it.
func (w *Watcher) Await(ctx context.Context, since time.Time, wait time.Duration) (Snapshot, bool) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		w.mu.Lock()
		snap, changed := w.snap, w.changed
		w.mu.Unlock()
		if !snap.Changed.Before(since) {
			return snap, true
		}
		select {
		case <-changed:
		case <-timer.C:
			return snap, false
		case <-ctx.Done():
			return snap, false
		}
	}
}

// A report crosses from the helper as one line: the sequence number, 1 or 0
// for an image, and the text in base64 — "-" for none. Base64 because the text
// is whatever the clipboard held, newlines included, and the line has to stay
// one line.

// encodeLine is one report.
func encodeLine(seq uint32, text string, image bool) string {
	payload := "-"
	if text != "" {
		payload = base64.StdEncoding.EncodeToString([]byte(text))
	}
	flag := "0"
	if image {
		flag = "1"
	}
	return fmt.Sprintf("%d %s %s\n", seq, flag, payload)
}

// decodeLine reads one report.
func decodeLine(line string) (text string, image bool, err error) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return "", false, fmt.Errorf("clipwatch: malformed report %q", line)
	}
	if _, err := strconv.ParseUint(fields[0], 10, 32); err != nil {
		return "", false, fmt.Errorf("clipwatch: malformed report %q", line)
	}
	if fields[2] != "-" {
		data, err := base64.StdEncoding.DecodeString(fields[2])
		if err != nil {
			return "", false, fmt.Errorf("clipwatch: malformed report %q", line)
		}
		text = string(data)
	}
	return text, fields[1] == "1", nil
}
