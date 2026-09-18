package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAuditRow struct {
	id string
	at time.Time
	// rowID, when set, is this row's place in the trail's write order, the way
	// a pool numbers its audit rows.
	rowID int64
}

// fakeAuditTrail is a trail whose rows become readable when a test says so,
// in any order, and which records every read it was asked for.
type fakeAuditTrail struct {
	mu    sync.Mutex
	rows  []fakeAuditRow
	reads []fakeAuditRead
	// failFrom makes every read from the nth on fail, as a pool that stopped
	// answering mid-poll does.
	failFrom int
}

type fakeAuditRead struct {
	cursor auditReadCursor
}

func (f *fakeAuditTrail) add(rows ...fakeAuditRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, rows...)
}

// source reads the trail by time. ordered makes it a trail with a write-order
// cursor, read by row id, the way the pool trail is.
func (f *fakeAuditTrail) source(ordered bool) auditSource[fakeAuditRow] {
	source := auditSource[fakeAuditRow]{
		read: func(_ context.Context, cursor auditReadCursor, limit int) ([]fakeAuditRow, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.reads = append(f.reads, fakeAuditRead{cursor: cursor})
			if f.failFrom > 0 && len(f.reads) >= f.failFrom {
				return nil, errors.New("pool is not reachable")
			}
			var out []fakeAuditRow
			for _, row := range f.rows {
				switch after, cursored := cursor.After["trail"]; {
				case ordered && cursored:
					if row.rowID > after {
						out = append(out, row)
					}
				case ordered && !cursored, !ordered:
					if cursor.Since.IsZero() || !row.at.Before(cursor.Since) {
						out = append(out, row)
					}
				}
			}
			// Write order only when read from a cursor, which is what the
			// pool trail does: without one the answer is ordered by time, and
			// the two orders differ exactly where this matters.
			if _, cursored := cursor.After["trail"]; ordered && cursored {
				slices.SortStableFunc(out, func(a, b fakeAuditRow) int { return cmp.Compare(a.rowID, b.rowID) })
			} else {
				slices.SortStableFunc(out, func(a, b fakeAuditRow) int { return a.at.Compare(b.at) })
			}
			if !cursor.Forward {
				slices.Reverse(out)
			}
			if len(out) > limit {
				out = out[:limit]
			}
			return out, nil
		},
		key:      func(r fakeAuditRow) string { return r.id },
		at:       func(r fakeAuditRow) time.Time { return r.at },
		lookback: auditQueuedLookback,
	}
	if ordered {
		source.rowID = func(r fakeAuditRow) (string, int64, bool) { return "trail", r.rowID, true }
	}
	return source
}

// followAuditFor follows trail until stop returns true for what it printed,
// or a second passes.
func followAuditFor(t *testing.T, trail *fakeAuditTrail, limit int, during func(printed []string), stop func(printed []string) bool) []string {
	t.Helper()
	interval := auditFollowInterval
	auditFollowInterval = time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var mu sync.Mutex
	var printed []string
	done := make(chan error, 1)
	go func() {
		done <- readAudit(ctx, []auditSource[fakeAuditRow]{trail.source(false)}, auditReadOptions{limit: limit, follow: true}, func(rows []fakeAuditRow) error {
			mu.Lock()
			defer mu.Unlock()
			for _, row := range rows {
				printed = append(printed, row.id)
			}
			if stop(printed) {
				cancel()
			}
			return nil
		})
	}()
	if during != nil {
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		snapshot := append([]string{}, printed...)
		mu.Unlock()
		during(snapshot)
	}
	if err := <-done; err != nil {
		t.Fatalf("readAudit: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return printed
}

// A follower prints the backlog oldest first, then each record once — including
// one that became readable after a newer one, as a slow pool write does.
func TestFollowPrintsEachRecordOnceIncludingOneThatArrivedLate(t *testing.T) {
	base := time.Now().Add(-10 * time.Second)
	trail := &fakeAuditTrail{}
	trail.add(fakeAuditRow{id: "b", at: base.Add(2 * time.Second)}, fakeAuditRow{id: "a", at: base.Add(time.Second)})
	printed := followAuditFor(t, trail, 10, func([]string) {
		trail.add(fakeAuditRow{id: "d", at: base.Add(4 * time.Second)})
		// Written before d, readable only once d has been printed.
		time.Sleep(20 * time.Millisecond)
		trail.add(fakeAuditRow{id: "c", at: base.Add(3 * time.Second)})
	}, func(printed []string) bool { return len(printed) >= 4 })
	if want := []string{"a", "b"}; !slices.Equal(printed[:2], want) {
		t.Fatalf("backlog printed %v, want oldest first %v", printed[:2], want)
	}
	slices.Sort(printed)
	if want := []string{"a", "b", "c", "d"}; !slices.Equal(printed, want) {
		t.Fatalf("printed %v, want every record exactly once %v", printed, want)
	}
}

// A full page means more is waiting: the follower reads on from the page's
// last record rather than asking the same page again.
func TestFollowPagesPastAFullPage(t *testing.T) {
	base := time.Now().Add(-30 * time.Second)
	trail := &fakeAuditTrail{}
	printed := followAuditFor(t, trail, 2, func([]string) {
		for i := range 7 {
			trail.add(fakeAuditRow{id: string(rune('a' + i)), at: base.Add(time.Duration(i) * time.Second)})
		}
	}, func(printed []string) bool { return len(printed) >= 7 })
	if got := strings.Join(printed, ""); got != "abcdefg" {
		t.Fatalf("printed %q, want every record in order", got)
	}
}

// More records in one instant than a page holds cannot be paged past by time.
// The follower must wait between reads rather than spin on the same page.
func TestFollowDoesNotSpinOnAPageOfOneInstant(t *testing.T) {
	at := time.Now()
	trail := &fakeAuditTrail{}
	trail.add(fakeAuditRow{id: "a", at: at}, fakeAuditRow{id: "b", at: at}, fakeAuditRow{id: "c", at: at})
	interval := auditFollowInterval
	auditFollowInterval = 20 * time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := readAudit(ctx, []auditSource[fakeAuditRow]{trail.source(false)}, auditReadOptions{since: at, limit: 2, follow: true}, func([]fakeAuditRow) error { return nil }); err != nil {
		t.Fatalf("readAudit: %v", err)
	}
	if len(trail.reads) > 10 {
		t.Fatalf("%d reads in 100ms at a 20ms interval: the follower is spinning", len(trail.reads))
	}
}

func TestParseStatusFilter(t *testing.T) {
	for _, tc := range []struct {
		in        string
		low, high int
		err       bool
	}{
		{in: "404", low: 404, high: 404},
		{in: "5xx", low: 500, high: 599},
		{in: "4XX", low: 400, high: 499},
		{in: "400-403", low: 400, high: 403},
		{in: "6xx", err: true},
		{in: "403-400", err: true},
		{in: "ok", err: true},
		{in: "0", err: true},
	} {
		low, high, err := parseStatusFilter(tc.in)
		if tc.err != (err != nil) || low != tc.low || high != tc.high {
			t.Fatalf("parseStatusFilter(%q) = %d, %d, %v", tc.in, low, high, err)
		}
	}
}

// A body printed to a terminal is escaped without being held whole, and a rune
// split across two reads is not mistaken for two invalid bytes.
func TestCopyTerminalSafeCarriesARuneAcrossReads(t *testing.T) {
	var out strings.Builder
	src := &splitReader{chunks: []string{"héllo \x1b[2J caf\xc3", "\xa9 \xff done"}}
	if err := copyTerminalSafe(&out, src); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), `héllo \x1b[2J café \xff done`; got != want {
		t.Fatalf("copyTerminalSafe = %q, want %q", got, want)
	}
}

type splitReader struct{ chunks []string }

func (r *splitReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks = r.chunks[1:]
	return n, nil
}

// A trail whose IDs are its write order is followed by cursor: every poll asks
// for what was written after the last row printed, and nothing is re-read.
func TestFollowReadsAWriteOrderedTrailByCursor(t *testing.T) {
	interval := auditFollowInterval
	auditFollowInterval = time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })
	base := time.Now().Add(-time.Minute)
	trail := &fakeAuditTrail{}
	trail.add(
		fakeAuditRow{id: "a", at: base.Add(time.Second), rowID: 1},
		fakeAuditRow{id: "b", at: base.Add(2 * time.Second), rowID: 2},
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var printed []string
	err := readAudit(ctx, []auditSource[fakeAuditRow]{trail.source(true)}, auditReadOptions{limit: 10, follow: true}, func(rows []fakeAuditRow) error {
		for _, row := range rows {
			printed = append(printed, row.id)
		}
		if len(printed) >= 3 {
			cancel()
			return nil
		}
		if len(printed) == 2 {
			// Written next, and stamped before what is already printed: a
			// cursor read is the only one that returns it.
			trail.add(fakeAuditRow{id: "late", at: base, rowID: 3})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("readAudit: %v", err)
	}
	if !slices.Equal(printed, []string{"a", "b", "late"}) {
		t.Fatalf("printed %v, want the backlog then the row written after it", printed)
	}
	trail.mu.Lock()
	defer trail.mu.Unlock()
	for i, read := range trail.reads {
		if i == 0 {
			continue // the backlog, which has no cursor yet
		}
		if read.cursor.After["trail"] == 0 {
			t.Fatalf("read %d asked without a cursor: %+v", i, read.cursor)
		}
	}
}

// Paced printing spreads a batch across the poll interval, so records appear
// one at a time instead of in a burst. A reader that is behind is never paced,
// and neither is one whose output is not a terminal.
func TestFollowPacesABatchAcrossTheInterval(t *testing.T) {
	interval := auditFollowInterval
	auditFollowInterval = 200 * time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })
	at := time.Now().Add(-time.Second)
	trail := &fakeAuditTrail{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var gaps []time.Duration
	last := time.Time{}
	go func() {
		time.Sleep(20 * time.Millisecond)
		trail.add(
			fakeAuditRow{id: "a", at: at.Add(time.Millisecond)},
			fakeAuditRow{id: "b", at: at.Add(2 * time.Millisecond)},
			fakeAuditRow{id: "c", at: at.Add(3 * time.Millisecond)},
		)
	}()
	err := readAudit(ctx, []auditSource[fakeAuditRow]{trail.source(false)}, auditReadOptions{since: at, limit: 10, follow: true, paced: true}, func(rows []fakeAuditRow) error {
		if len(rows) != 1 {
			t.Errorf("paced emit got %d records at once, want one", len(rows))
		}
		if !last.IsZero() {
			gaps = append(gaps, time.Since(last))
		}
		last = time.Now()
		if len(gaps) == 2 {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("readAudit: %v", err)
	}
	if len(gaps) != 2 {
		t.Fatalf("gaps = %v, want two between three records", gaps)
	}
	for _, gap := range gaps {
		if gap < auditFollowInterval/8 || gap > auditFollowInterval {
			t.Fatalf("gap %s, want about a quarter of the %s interval", gap, auditFollowInterval)
		}
	}

	// The same batch, unpaced, arrives as one write.
	trail = &fakeAuditTrail{}
	trail.add(fakeAuditRow{id: "a", at: at}, fakeAuditRow{id: "b", at: at.Add(time.Millisecond)})
	unpaced, cancelUnpaced := context.WithTimeout(context.Background(), time.Second)
	defer cancelUnpaced()
	batches := 0
	if err := readAudit(unpaced, []auditSource[fakeAuditRow]{trail.source(false)}, auditReadOptions{since: at, limit: 10, follow: true}, func(rows []fakeAuditRow) error {
		if len(rows) > 0 {
			batches++
			if len(rows) != 2 {
				t.Errorf("unpaced batch = %d records, want both at once", len(rows))
			}
			cancelUnpaced()
		}
		return nil
	}); err != nil {
		t.Fatalf("readAudit: %v", err)
	}
	if batches != 1 {
		t.Fatalf("unpaced batches = %d, want one", batches)
	}
}

// The trails are stamped by three different machines, so one trail's clock must
// never set where another is read from. A pool running minutes ahead used to
// push the shared position into the control plane's future, and the verdict
// trail then matched nothing for as long as the follow ran.
func TestFollowKeepsEachTrailOnItsOwnClock(t *testing.T) {
	interval := auditFollowInterval
	auditFollowInterval = time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })

	now := time.Now()
	ahead := &fakeAuditTrail{}  // a pool five minutes fast
	behind := &fakeAuditTrail{} // the control plane
	ahead.add(fakeAuditRow{id: "fast", at: now.Add(5 * time.Minute)})
	behind.add(fakeAuditRow{id: "slow-1", at: now})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var printed []string
	err := readAudit(ctx, []auditSource[fakeAuditRow]{ahead.source(false), behind.source(false)},
		auditReadOptions{limit: 10, follow: true}, func(rows []fakeAuditRow) error {
			for _, row := range rows {
				printed = append(printed, row.id)
			}
			if len(printed) == 2 {
				// Recorded by the slow machine after the follow began, and
				// still minutes behind what the fast one has already stamped.
				behind.add(fakeAuditRow{id: "slow-2", at: now.Add(time.Second)})
			}
			if slices.Contains(printed, "slow-2") {
				cancel()
			}
			return nil
		})
	if err != nil {
		t.Fatalf("readAudit: %v", err)
	}
	if !slices.Contains(printed, "slow-2") {
		t.Fatalf("printed %v, want the record from the trail whose clock is behind", printed)
	}
	// The trail behind is never asked for records from the other's future.
	behind.mu.Lock()
	defer behind.mu.Unlock()
	for _, read := range behind.reads {
		if read.cursor.Since.After(now.Add(time.Minute)) {
			t.Fatalf("the slow trail was read from %s, which is the fast trail's clock", read.cursor.Since)
		}
	}
}

// A record under a write-ordered cursor is recognized by its row ID, so
// following a busy pool must not grow the key set the other trails need.
func TestFollowRemembersNoKeysForACursoredTrail(t *testing.T) {
	interval := auditFollowInterval
	auditFollowInterval = time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })
	base := time.Now().Add(-time.Minute)
	trail := &fakeAuditTrail{}
	for i := range 40 {
		trail.add(fakeAuditRow{id: fmt.Sprintf("r%d", i), at: base.Add(time.Duration(i) * time.Second), rowID: int64(i + 1)})
	}
	source := trail.source(true)
	position := newAuditPosition(source.lookback)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// One read with no cursor, as a follower's first forward read is, then one
	// on the cursor that read established.
	cursor := position.read(time.Time{})
	page, err := source.read(ctx, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range page {
		id := auditRowID(source, row)
		position.admit(source.key(row), source.at(row), id, cursor.After[id.partition] > 0)
		if id.ordered && len(page) < 100 {
			position.bootstrap(id.partition, id.id)
		}
	}
	if position.after["trail"] != 40 {
		t.Fatalf("cursor = %d, want the last row of a page that held everything", position.after["trail"])
	}
	keysAfterFirst := len(position.seen)

	trail.add(fakeAuditRow{id: "r40", at: base.Add(40 * time.Second), rowID: 41})
	cursor = position.read(time.Time{})
	page, err = source.read(ctx, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range page {
		id := auditRowID(source, row)
		if !position.admit(source.key(row), source.at(row), id, cursor.After[id.partition] > 0) {
			t.Fatalf("%s was read again under a cursor", source.key(row))
		}
	}
	if len(position.seen) != keysAfterFirst {
		t.Fatalf("keys grew from %d to %d while reading under a cursor", keysAfterFirst, len(position.seen))
	}
}

// A poll pages through its window, and the cursor it ends up with has to cover
// every page of it. Judged one page at a time, the cursor lands on the boundary
// record of the last sub-window and leaves behind a record from an earlier page
// that the recorder's queue gave a higher ID — which comes back as new, because
// a cursored record is recognized by its ID and never by a key.
func TestFollowTakesOneCursorForTheWholePoll(t *testing.T) {
	interval := auditFollowInterval
	auditFollowInterval = time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })
	base := time.Now().Add(-time.Minute)
	trail := &fakeAuditTrail{}
	// Two requests that finished together and inverted on the way into the
	// recorder's queue: the earlier-stamped one was written second.
	trail.add(
		fakeAuditRow{id: "inverted", at: base, rowID: 5},
		fakeAuditRow{id: "boundary", at: base.Add(time.Second), rowID: 1},
	)

	// Long enough for several polls: the duplicate this guards against is not
	// printed by the poll that sets the cursor, but by the next one that reads
	// on it.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var printed []string
	// limit 2 makes the first page full, so the poll pages once more and the
	// second page holds only the boundary record.
	err := readAudit(ctx, []auditSource[fakeAuditRow]{trail.source(true)},
		auditReadOptions{since: base.Add(-time.Second), limit: 2, follow: true}, func(rows []fakeAuditRow) error {
			for _, row := range rows {
				printed = append(printed, row.id)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("readAudit: %v", err)
	}
	slices.Sort(printed)
	if !slices.Equal(printed, []string{"boundary", "inverted"}) {
		t.Fatalf("printed %v, want each record exactly once", printed)
	}
}

// A trail that stops answering mid-poll has said nothing about where its window
// ends. Read as a short page — which is what a failed read looks like, an empty
// one — it would flush the cursor the poll had accumulated and put it past
// whatever the last full page cut, losing those records for good while the
// trail was named unreadable for only one poll.
func TestFollowTakesNoCursorFromATrailThatStoppedAnswering(t *testing.T) {
	interval := auditFollowInterval
	auditFollowInterval = time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })
	base := time.Now().Add(-time.Minute)
	trail := &fakeAuditTrail{failFrom: 2}
	// One page's worth, plus a record the cut leaves for the next page whose
	// ID is below the page's highest — the queue inversion again.
	trail.add(
		fakeAuditRow{id: "a", at: base, rowID: 9},
		fakeAuditRow{id: "b", at: base.Add(time.Second), rowID: 10},
		fakeAuditRow{id: "left-behind", at: base.Add(2 * time.Second), rowID: 3},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	var missing []string
	err := readAudit(ctx, []auditSource[fakeAuditRow]{trail.source(true)}, auditReadOptions{
		since: base.Add(-time.Second), limit: 2, follow: true,
		unavailable: func(trail string, _ error) { missing = append(missing, trail) },
	}, func([]fakeAuditRow) error { return nil })
	if err != nil {
		t.Fatalf("readAudit: %v", err)
	}
	if len(missing) == 0 {
		t.Fatal("the trail that stopped answering was not reported")
	}
	// Nothing may be under a cursor: the poll never reached the end of its
	// window, so the record the cut left behind is still to come.
	trail.mu.Lock()
	defer trail.mu.Unlock()
	for i, read := range trail.reads {
		if len(read.cursor.After) != 0 {
			t.Fatalf("read %d asked from cursor %v after a failed read; the record cut from page 1 is now unreachable", i, read.cursor.After)
		}
	}
}

// The backlog is the newest records across every trail, so a busy trail can
// fill the whole cut on its own. A trail left out of it has no position, and
// without a floor its first forward read asks for the oldest records it holds
// and pages forward through the entire history — inside the first poll.
func TestFollowNeverReadsATrailFromTheBeginning(t *testing.T) {
	interval := auditFollowInterval
	auditFollowInterval = time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })
	now := time.Now()
	busy, quiet := &fakeAuditTrail{}, &fakeAuditTrail{}
	// The busy trail fills the whole backlog; the quiet trail's records are all
	// older than the cut, so none of them survive it.
	for i := range 4 {
		busy.add(fakeAuditRow{id: fmt.Sprintf("busy%d", i), at: now.Add(-time.Duration(i) * time.Second)})
	}
	for i := range 3 {
		quiet.add(fakeAuditRow{id: fmt.Sprintf("ancient%d", i), at: now.Add(-time.Duration(i+1) * time.Hour)})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var printed []string
	err := readAudit(ctx, []auditSource[fakeAuditRow]{busy.source(false), quiet.source(false)},
		auditReadOptions{limit: 2, follow: true}, func(rows []fakeAuditRow) error {
			for _, row := range rows {
				printed = append(printed, row.id)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("readAudit: %v", err)
	}
	// Hours-old records are not part of the tail the caller asked for, and
	// printing them means the trail was read from its beginning.
	for _, id := range printed {
		if strings.HasPrefix(id, "ancient") {
			t.Fatalf("printed %v: the quiet trail was replayed from the beginning", printed)
		}
	}
	quiet.mu.Lock()
	defer quiet.mu.Unlock()
	for i, read := range quiet.reads[1:] {
		if read.cursor.Since.IsZero() {
			t.Fatalf("forward read %d of the quiet trail had no lower bound", i)
		}
	}
}

// The backlog floor is one time for every trail, so it belongs to whichever
// machine stamped it. A trail the cut left out is anchored on its own newest
// record instead: floored on a fast pool's clock, it would show nothing it
// recorded below that, for as long as the skew lasts.
func TestFollowDoesNotFloorATrailOnAnotherMachinesClock(t *testing.T) {
	interval := auditFollowInterval
	auditFollowInterval = time.Millisecond
	t.Cleanup(func() { auditFollowInterval = interval })
	now := time.Now()
	fast, slow := &fakeAuditTrail{}, &fakeAuditTrail{}
	// A pool five minutes ahead fills the whole backlog.
	for i := range 3 {
		fast.add(fakeAuditRow{id: fmt.Sprintf("pool%d", i), at: now.Add(5*time.Minute - time.Duration(i)*time.Second)})
	}
	// The control plane has one older verdict, cut from the backlog.
	slow.add(fakeAuditRow{id: "old-verdict", at: now.Add(-time.Minute)})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var printed []string
	err := readAudit(ctx, []auditSource[fakeAuditRow]{fast.source(false), slow.source(false)},
		auditReadOptions{limit: 2, follow: true}, func(rows []fakeAuditRow) error {
			for _, row := range rows {
				printed = append(printed, row.id)
			}
			if len(printed) == 2 {
				// Recorded now on the slow machine, which is minutes below
				// anything the fast one has stamped.
				slow.add(fakeAuditRow{id: "new-verdict", at: now})
			}
			if slices.Contains(printed, "new-verdict") {
				cancel()
			}
			return nil
		})
	if err != nil {
		t.Fatalf("readAudit: %v", err)
	}
	if !slices.Contains(printed, "new-verdict") {
		t.Fatalf("printed %v, want the record the slow machine made after the follow began", printed)
	}
	// And the record the cut dropped is recognized, not replayed as new.
	if slices.Contains(printed, "old-verdict") {
		t.Fatalf("printed %v, want the cut record left out of the tail", printed)
	}
}
