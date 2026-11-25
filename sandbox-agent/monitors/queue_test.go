package monitors

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func testQueue(t *testing.T) *Queue {
	return HomeDirs(t.TempDir()).Queue()
}

func TestAddGivesIncreasingIDsAndPopTakesThemInOrder(t *testing.T) {
	q := testQueue(t)
	for i := 1; i <= 3; i++ {
		ev, err := q.Add(Event{Instance: "a", Title: fmt.Sprint("event ", i)})
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprint("e", i); ev.ID != want {
			t.Fatalf("id = %q, want %q", ev.ID, want)
		}
	}
	got, err := q.Pop(Filter{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "e1" || got[1].ID != "e2" {
		t.Fatalf("popped %+v", got)
	}
	if n, _ := q.Count(Filter{}); n != 1 {
		t.Fatalf("%d pending, want 1", n)
	}
	// Popped is still readable.
	if ev, err := q.Get("e1"); err != nil || ev.Title != "event 1" {
		t.Fatalf("read after pop = %+v, %v", ev, err)
	}
}

func TestAnIDIsNotGivenOutAgainAfterItsEventIsGone(t *testing.T) {
	q := testQueue(t)
	if _, err := q.Add(Event{Instance: "a", Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Pop(Filter{}, 0); err != nil {
		t.Fatal(err)
	}
	// Prune everything read.
	q.Now = func() time.Time { return time.Now().Add(2 * ReadMaxAge) }
	q.prune()
	q.Now = nil
	ev, err := q.Add(Event{Instance: "a", Title: "y"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ID != "e2" {
		t.Fatalf("id = %q, want e2", ev.ID)
	}
}

func TestAKeyedEventReplacesThePendingOneAndCountsIt(t *testing.T) {
	q := testQueue(t)
	for _, title := range []string{"checks failed", "checks failed again", "checks passed"} {
		if _, err := q.Add(Event{Instance: "pr", Key: "checks", Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	// Another instance's event with the same key is its own.
	if _, err := q.Add(Event{Instance: "other", Key: "checks", Title: "theirs"}); err != nil {
		t.Fatal(err)
	}
	got, err := q.Peek(Filter{Instance: "pr"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "checks passed" || got[0].Count != 3 {
		t.Fatalf("pending = %+v", got)
	}
	if n, _ := q.Count(Filter{}); n != 2 {
		t.Fatalf("%d pending, want 2", n)
	}
}

func TestAFloodingInstanceDropsItsOldestAndSaysHowMany(t *testing.T) {
	q := testQueue(t)
	for i := 0; i < PendingCap+5; i++ {
		if _, err := q.Add(Event{Instance: "flood", Title: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := q.Peek(Filter{Instance: "flood"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var dropped *Event
	var regular int
	for i := range got {
		if got[i].Kind == KindDropped {
			dropped = &got[i]
		} else {
			regular++
		}
	}
	if dropped == nil || dropped.Count != 6 {
		t.Fatalf("dropped notice = %+v", dropped)
	}
	if regular != PendingCap-1 {
		t.Fatalf("%d events kept, want %d", regular, PendingCap-1)
	}
	if last := got[len(got)-1]; last.Title != fmt.Sprint(PendingCap+4) {
		t.Fatalf("newest kept = %q", last.Title)
	}
}

func TestConcurrentPopsNeverReturnOneEventTwice(t *testing.T) {
	q := testQueue(t)
	for i := 0; i < 50; i++ {
		if _, err := q.Add(Event{Instance: "a", Title: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			got, err := q.Pop(Filter{}, 0)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			for _, ev := range got {
				seen[ev.ID]++
			}
			mu.Unlock()
		})
	}
	wg.Wait()
	if len(seen) != 50 {
		t.Fatalf("%d events popped, want 50", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s popped %d times", id, n)
		}
	}
}

func TestConcurrentAddsNeverShareAnID(t *testing.T) {
	q := testQueue(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 10 {
				if _, err := q.Add(Event{Instance: fmt.Sprint("i", i), Title: fmt.Sprint(j)}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if n, _ := q.Count(Filter{}); n != 80 {
		t.Fatalf("%d pending, want 80", n)
	}
}

func TestWaitReturnsWhenAnEventArrives(t *testing.T) {
	q := testQueue(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- q.Wait(ctx, Filter{Instance: "b"}) }()
	time.Sleep(100 * time.Millisecond)
	if _, err := q.Add(Event{Instance: "a", Title: "not for this wait"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("wait returned for another instance's event: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := q.Add(Event{Instance: "b", Title: "this one"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGetRefusesWhatIsNotAnID(t *testing.T) {
	q := testQueue(t)
	if _, err := q.Get("../../etc/passwd"); err == nil || !strings.Contains(err.Error(), "not an event id") {
		t.Fatalf("get = %v", err)
	}
}

func TestParseLineTakesJSONOrAPlainLine(t *testing.T) {
	ev, ok := ParseLine([]byte(`{"title":"t","kind":"k","key":"x","final":true,"data":{"a":1}}`))
	if !ok || ev.Title != "t" || ev.Kind != "k" || ev.Key != "x" || !ev.Final || string(ev.Data) != `{"a":1}` {
		t.Fatalf("json line = %+v", ev)
	}
	ev, ok = ParseLine([]byte("ERROR something broke\r"))
	if !ok || ev.Title != "ERROR something broke" {
		t.Fatalf("plain line = %+v", ev)
	}
	ev, _ = ParseLine([]byte(`{"kind":"untitled"}`))
	if ev.Title != "untitled" {
		t.Fatalf("untitled json line = %+v", ev)
	}
	if _, ok := ParseLine([]byte("   ")); ok {
		t.Fatal("a blank line is an event")
	}
	big := strings.Repeat("é", MaxBody)
	ev, _ = ParseLine([]byte(`{"title":"t","body":"` + big + `"}`))
	if !ev.BodyTruncated || len(ev.Body) > MaxBody || !strings.HasSuffix(ev.Body, "é") {
		t.Fatalf("body of %d bytes, truncated %v", len(ev.Body), ev.BodyTruncated)
	}
}

func TestAPopThatDiedMidClaimLeavesNothingPending(t *testing.T) {
	q := testQueue(t)
	if _, err := q.Add(Event{Instance: "a", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	// Claimed into Read, and the pending name never removed.
	if err := os.MkdirAll(q.Read, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(q.path(q.Pending, "e1"), q.path(q.Read, "e1")); err != nil {
		t.Fatal(err)
	}
	got, err := q.Pop(Filter{}, 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("pop = %+v, %v; want nothing, it was taken", got, err)
	}
	if n, _ := q.Count(Filter{}); n != 0 {
		t.Fatalf("%d pending after the pop; the stale name should be gone", n)
	}
}

func TestAnEventGivenAClaimedIDIsRenumberedNotLost(t *testing.T) {
	q := testQueue(t)
	if _, err := q.Add(Event{Instance: "a", Title: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Pop(Filter{}, 0); err != nil {
		t.Fatal(err)
	}
	// A writer that read the highest id before the pop claimed e1 links a
	// different event under it.
	other, err := json.Marshal(Event{ID: "e1", Instance: "a", Title: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(q.path(q.Pending, "e1"), other, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := q.Pop(Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		// Renumbered on this pop; the next one returns it.
		got, err = q.Pop(Filter{}, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 1 || got[0].Title != "second" || got[0].ID == "e1" {
		t.Fatalf("popped %+v; want the second event under a new id", got)
	}
	if first, err := q.Get("e1"); err != nil || first.Title != "first" {
		t.Fatalf("e1 = %+v, %v; want the first event still readable", first, err)
	}
}
