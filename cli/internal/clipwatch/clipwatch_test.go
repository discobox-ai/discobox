package clipwatch

import (
	"context"
	"testing"
	"time"
)

func TestALineCarriesAnyText(t *testing.T) {
	for _, tc := range []struct {
		text  string
		image bool
	}{
		{"", true},
		{"hello", false},
		{"two\nlines ● and a tab\t", false},
	} {
		text, image, err := decodeLine(encodeLine(7, tc.text, tc.image))
		if err != nil || text != tc.text || image != tc.image {
			t.Errorf("round trip of %q, %v = %q, %v, %v", tc.text, tc.image, text, image, err)
		}
	}
	for _, line := range []string{"", "7 0", "x 0 -", "7 0 !!!"} {
		if _, _, err := decodeLine(line); err == nil {
			t.Errorf("decodeLine(%q) accepted a malformed report", line)
		}
	}
}

func TestWhatTheClipboardHeldAtTheStartIsNotFresh(t *testing.T) {
	w := NewWatcher()
	since := time.Now()
	w.Record("copied an hour ago", false)
	if !w.Live() {
		t.Fatal("the first report did not make the watch live")
	}
	snap, fresh := w.Await(context.Background(), since, 10*time.Millisecond)
	if fresh || snap.Text != "copied an hour ago" {
		t.Fatalf("Await = %+v, %v; want the starting clipboard, not fresh", snap, fresh)
	}
}

func TestAwaitAnswersAChangeSinceAtOnce(t *testing.T) {
	w := NewWatcher()
	w.Record("old", false)
	before := time.Now()
	w.Record("said", false)
	snap, fresh := w.Await(context.Background(), before, time.Hour)
	if !fresh || snap.Text != "said" {
		t.Fatalf("Await = %+v, %v; want the change just recorded", snap, fresh)
	}
}

func TestAwaitWaitsForAChangeStillToCome(t *testing.T) {
	w := NewWatcher()
	w.Record("old", false)
	since := time.Now().Add(time.Millisecond)
	go func() {
		time.Sleep(20 * time.Millisecond)
		w.Record("new", false)
	}()
	snap, fresh := w.Await(context.Background(), since, time.Second)
	if !fresh || snap.Text != "new" {
		t.Fatalf("Await = %+v, %v; want the change that came while it waited", snap, fresh)
	}
}

func TestAwaitGivesUpWithTheLatest(t *testing.T) {
	w := NewWatcher()
	w.Record("old", false)
	snap, fresh := w.Await(context.Background(), time.Now().Add(time.Hour), 10*time.Millisecond)
	if fresh || snap.Text != "old" {
		t.Fatalf("Await = %+v, %v; want the stale snapshot, not fresh", snap, fresh)
	}
}

func TestANilWatcherIsNeverLive(t *testing.T) {
	var w *Watcher
	if w.Live() {
		t.Fatal("a nil watcher is live")
	}
}
