package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// auditFollowInterval is how long a follower waits between polls. A variable so
// a test can shorten it.
var auditFollowInterval = 2 * time.Second

// defaultAuditLimit is how many records a read returns when --limit is not
// positive. Every trail interleaves into one timeline, so a small default
// leaves a busy sandbox's history only a minute or two deep.
const defaultAuditLimit = 1000

// auditPageLimit is the most records one request to a trail asks for: the
// audit endpoints' ceiling. A larger --limit is read as several pages of it,
// and a follower pages by it.
const auditPageLimit = 1000

// How far behind its newest record a follower re-reads a trail that has no
// write-ordered cursor, because a record does not become readable in the order
// of its time.
//
// Only the pool trail has a cursor — its row IDs are the write order — and the
// other three number their records at random, so time is all they have. They
// need far less of it: each is written by one process that commits as it
// records, so only two commits interleaving can reorder them. The pool proxy
// stamps an exchange when it ends and writes it from a queue, and pools are
// separate hosts with separate clocks, so a pool not yet under a cursor needs a
// wide window.
const (
	auditWriterLookback = 5 * time.Second
	auditQueuedLookback = time.Minute
)

// auditRememberedKeys is how many printed records a follower recognizes by key
// before it starts forgetting the oldest of them. Only a trail read by time
// reaches it: a record under a write-ordered cursor is recognized by its row ID
// and never stored here, so the busiest trail is the one that costs nothing.
const auditRememberedKeys = 10_000

// auditReadCursor is where a read starts.
type auditReadCursor struct {
	// Since keeps records at or after it, inclusively, and is what a trail
	// with no cursor reads forward from.
	Since time.Time
	// Until keeps records at or before it, inclusively, and is how a one-off
	// read pages back past the first page: each page is read up to the oldest
	// record of the one before, and the records sharing that instant are read
	// again and recognized by key.
	Until time.Time
	// Forward reads oldest first, which is how a follower reads; a one-off
	// read takes the newest instead.
	Forward bool
	// After is a write-ordered cursor per partition — for the pool trail, per
	// pool, since its row IDs are only ordered within one. A partition with no
	// entry is read by Since.
	After map[string]int64
}

// auditSource is one trail as the audit commands read it.
type auditSource[T any] struct {
	// name is the trail, for saying which one could not be read.
	name string
	read func(ctx context.Context, cursor auditReadCursor, limit int) ([]T, error)
	key  func(T) string
	at   func(T) time.Time
	// rowID is a record's write-ordered cursor, where its trail has one: the
	// partition its IDs are ordered within, and the ID. ok is false for a trail
	// whose IDs are random, which is followed by time instead.
	rowID func(T) (partition string, id int64, ok bool)
	// lookback is how far back the trail is re-read where it has no cursor.
	lookback time.Duration
}

// auditPosition is a follower's position in ONE trail: a cursor per partition
// where the trail has one, and otherwise the newest time it has printed with
// the keys it printed around that time.
//
// One per trail, never one shared by a merged read. The four trails are stamped
// by three different machines — the pool proxy, the control plane and the
// sandbox agent — so a shared time position applies the clock of whichever
// trail is ahead to the ones behind it: a pool five minutes fast would leave
// the verdict trail permanently unread, with nothing printed to say so.
type auditPosition struct {
	lookback time.Duration
	newest   time.Time
	// floor is where the backlog began for this trail, and bounds its reads
	// from below. Without it the first poll reads back a whole lookback from
	// the trail's newest record, and on a busy trail everything in that window
	// older than the backlog prints as new — past --limit, and out of order
	// with the backlog it follows.
	//
	// A trail that answered the backlog is floored at its own oldest record
	// read, cut or printed, which is a time on its own clock. One time for
	// every trail is a foreign clock to all but the one that stamped it, and a
	// position on a foreign clock is what the per-trail split took out: a
	// trail floored above its own present skips whatever it records below
	// that.
	//
	// A trail that answered with nothing at all has no time of its own, so it
	// is floored at the oldest record the backlog printed. Without that it has
	// no lower bound, and every trail spells that as "the oldest limit records
	// I hold" — a whole history, paged through inside the first poll. Against
	// a clock behind it the foreign floor misses what it records below —
	// bounded by the skew, and over as soon as it has one record of its own.
	floor time.Time
	seen  map[string]time.Time
	after map[string]int64
	// moved records that the last read advanced the position, so a full page
	// is read past rather than asked for again.
	moved bool
}

func newAuditPosition(lookback time.Duration) *auditPosition {
	if lookback <= 0 {
		lookback = auditWriterLookback
	}
	return &auditPosition{lookback: lookback, seen: map[string]time.Time{}, after: map[string]int64{}}
}

// admit reports whether a record is new, and moves the position past it.
//
// cursored says this partition was read in its own write order, which is what
// makes moving the cursor over the record safe: the page came back in the order
// the pool wrote it, so nothing below the last record is unread. A page ordered
// by time cannot move a cursor — the next page may hold a record written
// earlier and stamped later, and a cursor past it would never ask again.
func (p *auditPosition) admit(key string, at time.Time, cursor rowCursor, cursored bool) bool {
	if cursored && cursor.ordered {
		// Exact, and free: the pool will not answer with this record again, so
		// it needs no key of its own to be recognized by.
		if cursor.id <= p.after[cursor.partition] {
			return false
		}
		p.after[cursor.partition] = cursor.id
		p.note(at)
		return true
	}
	if _, printed := p.seen[key]; printed {
		return false
	}
	p.seen[key] = at
	p.note(at)
	return true
}

func (p *auditPosition) note(at time.Time) {
	if at.After(p.newest) {
		p.newest = at
	}
	p.moved = true
}

// bootstrap takes a first cursor from records read by time.
//
// The caller decides when it is safe, and the unit it decides over is the whole
// poll, not one page: a poll pages through its window, so only the last page
// being short says the window has been read to the end. Judging a page on its
// own sets the cursor to the boundary record of a sub-window and leaves behind
// any record of an earlier page that the recorder's queue gave a higher ID —
// which the next read then hands back as new, because a cursored record is
// recognized by its ID and never by a key.
//
// Given a complete window, the highest ID in it has nothing unread behind it
// that the lookback does not already cover.
func (p *auditPosition) bootstrap(partition string, id int64) {
	if id > p.after[partition] {
		p.after[partition] = id
	}
}

// read is where the next poll of this trail starts, forgetting the keys it can
// no longer be given.
func (p *auditPosition) read(since time.Time) auditReadCursor {
	from := since
	if p.floor.After(from) {
		from = p.floor
	}
	if !p.newest.IsZero() {
		if behind := p.newest.Add(-p.lookback); behind.After(from) {
			from = behind
		}
	}
	// Forgetting is memory management, not part of the cursor: a key is what
	// keeps a record from being printed twice, so while the set is small
	// nothing is dropped, and a trail answering with a little more than it was
	// asked for cannot turn that into a duplicate.
	if len(p.seen) > auditRememberedKeys {
		for key, at := range p.seen {
			if at.Before(from.Add(-p.lookback)) {
				delete(p.seen, key)
			}
		}
	}
	after := make(map[string]int64, len(p.after))
	for partition, id := range p.after {
		after[partition] = id
	}
	return auditReadCursor{Since: from, Forward: true, After: after}
}

// auditReadOptions is how a command reads its trails.
type auditReadOptions struct {
	since time.Time
	limit int
	// follow keeps reading after the first page.
	follow bool
	// paced spreads a followed batch across the poll interval, so records
	// appear at something like the rate they were recorded rather than in a
	// burst every interval. Only for a terminal: a pipe gets each record as
	// soon as it is read, and so does a follower still catching up.
	paced bool
	// unavailable, when set, is told which trail could not be read and keeps
	// what the others answered. Without it one trail's failure fails the read,
	// which is what a single-trail command wants.
	//
	// Once per failed read, not once per poll: a poll pages, so a trail failing
	// twice while catching up says so twice. A caller that prints it collapses
	// the repeat (auditOnce), and a trail whose error changes between two
	// reads of one poll is worth hearing twice.
	unavailable func(trail string, err error)
	// polled, when set, runs after every poll, including one that printed
	// nothing. A follower that has gone quiet because a trail stopped
	// answering has to be able to say so, and it has no records to say it
	// alongside.
	polled func()
}

// readAudit reads the trails once, newest first, or follows them: the last
// limit records oldest first — or everything since opts.since — and then every
// record written after them as it arrives, until ctx ends. emit is given each
// batch in the order it is to be printed.
//
// Every trail keeps its own position, so one trail's clock, cursor or silence
// never moves another's.
func readAudit[T any](ctx context.Context, sources []auditSource[T], opts auditReadOptions, emit func([]T) error) error {
	limit := opts.limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	pageSize := min(limit, auditPageLimit)
	if len(sources) == 0 {
		return emit(nil)
	}
	positions := make([]*auditPosition, len(sources))
	for i, source := range sources {
		positions[i] = newAuditPosition(source.lookback)
	}
	if !opts.follow {
		pages, _, err := readAuditBack(ctx, sources, opts, limit)
		if err != nil {
			return err
		}
		if err := emit(mergeAuditBatches(sources, pages, false, limit)); err != nil {
			return err
		}
		if opts.polled != nil {
			opts.polled()
		}
		return nil
	}
	if opts.since.IsZero() {
		// The backlog: the newest records across the trails, printed oldest
		// first. What the cut drops is admitted too but not printed, which is
		// what leaves each trail a position of its own; admitBacklog has the
		// argument.
		pages, exhausted, err := readAuditBack(ctx, sources, opts, limit)
		if err != nil {
			return err
		}
		backlog := mergeAuditBatches(sources, pages, false, limit)
		slices.Reverse(backlog)
		admitBacklog(sources, positions, pages, exhausted)
		if len(backlog) > 0 {
			// Where the printed tail begins, for the trails with no record of
			// their own to start from.
			//
			// Nothing is floored when the backlog is empty, which includes
			// every trail having failed to answer — a control plane restarting
			// under a follow. The first poll that succeeds then reads each
			// trail from no bound at all, which is its whole history. It is the
			// loud failure rather than the silent one: every trail was named
			// unavailable first, and it corrects itself. Closing it properly
			// means retrying the backlog until one is established rather than
			// switching to forward reads, which is more than a line.
			floor := sources[0].at(backlog[0])
			for i := range positions {
				if len(pages[i]) == 0 {
					positions[i].floor = floor
				}
			}
		}
		if err := emit(backlog); err != nil {
			return err
		}
	}
	for {
		started := time.Now()
		// A trail read by time needs somewhere to page to within one poll: its
		// position only moves to the newest record printed, and the lookback
		// puts the next read back behind that, so without this a window holding
		// more records than a page would be read from the top forever. A trail
		// under a cursor pages on the cursor and never needs it.
		pageFrom := make([]time.Time, len(sources))
		// The highest write-ordered ID each trail has shown this poll, per
		// partition, held back until its window has been read to the end.
		pending := make([]map[string]int64, len(sources))
		for {
			catchingUp := false
			fresh := make([][]T, len(sources))
			for i, source := range sources {
				cursor := positions[i].read(opts.since)
				if pageFrom[i].After(cursor.Since) {
					cursor.Since = pageFrom[i]
				}
				page, read, err := readAuditPage(ctx, source, opts, cursor, pageSize)
				if err != nil {
					return followEnded(ctx, err)
				}
				if !read {
					// A trail that did not answer says nothing about where its
					// window ends, so the cursor it has been accumulating
					// stays unset. Taking a short read for a complete one
					// would put the cursor past whatever the last full page
					// cut, and those records would never be offered again —
					// the trail was named unreadable for one poll and would be
					// quietly short for good.
					continue
				}
				positions[i].moved = false
				kept := page[:0:0]
				for _, row := range page {
					id := auditRowID(source, row)
					if positions[i].admit(source.key(row), source.at(row), id, cursor.After[id.partition] > 0) {
						kept = append(kept, row)
					}
					if id.ordered && id.id > pending[i][id.partition] {
						if pending[i] == nil {
							pending[i] = map[string]int64{}
						}
						pending[i][id.partition] = id.id
					}
				}
				fresh[i] = kept
				// A full page means more may be waiting behind it, so it is
				// read again at once, from its last record. A full page that
				// moves neither the position nor the page bound cannot be read
				// past — more records share one instant than a page holds — so
				// it waits like a caught-up trail rather than spinning.
				if len(page) < pageSize {
					// The window has been read to its end, so every record in
					// it — including the ones on this poll's earlier pages —
					// is printed, and the highest ID among them is a cursor
					// with nothing behind it.
					for partition, id := range pending[i] {
						positions[i].bootstrap(partition, id)
					}
					pending[i] = nil
					continue
				}
				if last := source.at(page[len(page)-1]); last.After(cursor.Since) {
					pageFrom[i] = last
					catchingUp = true
				} else if positions[i].moved {
					catchingUp = true
				}
			}
			// Not cut to the limit: every record here has already moved its own
			// trail's position, so dropping one would lose it for good. Each
			// trail is bounded by a page on its own.
			batch := mergeAuditBatches(sources, fresh, true, 0)
			if err := emitAudit(ctx, emit, batch, pace(opts, catchingUp, len(batch))); err != nil {
				return followEnded(ctx, err)
			}
			if opts.polled != nil {
				opts.polled()
			}
			if !catchingUp {
				break
			}
		}
		// The interval is the cadence of the poll, not a gap after the
		// printing: pacing a batch has already spent part of it.
		remaining := auditFollowInterval - time.Since(started)
		if remaining <= 0 {
			remaining = time.Millisecond
		}
		if !sleepContext(ctx, remaining) {
			return nil
		}
	}
}

// readAuditAll is a one-off read of one trail collected whole, for -o json,
// which writes the list as one document. It pages back as any one-off read
// does, so a --limit past one page reaches past it here too.
func readAuditAll[T any](ctx context.Context, source auditSource[T], opts auditReadOptions) ([]T, error) {
	opts.follow = false
	rows := []T{}
	err := readAudit(ctx, []auditSource[T]{source}, opts, func(batch []T) error {
		rows = append(rows, batch...)
		return nil
	})
	return rows, err
}

// readAuditBack reads each trail's newest limit records since opts.since,
// newest first, for the reads that have no position yet. Every trail is read
// for the whole limit, because the newest limit across trails can all come
// from one of them.
//
// A limit past one page is read back a page at a time, each up to the oldest
// record of the page before. The bound is inclusive, so the records sharing
// that instant are read again and recognized by key rather than skipped. A
// full page that adds nothing new cannot be read past — more records share one
// instant than a page holds — so the trail ends there rather than looping.
//
// A page holding a record newer than its until was answered by a peer that
// predates until (a sandbox agent, say, that the control plane passes the read
// through to): it ignored the bound and answered its newest page again. That
// page is dropped and the trail ends, because appending it would put records
// newer than everything read so far at the end of a newest-first page, and
// on a trail still being written each such page adds a few, so it would not
// stop either.
//
// exhausted reports, per trail, that its last page came back short: the trail
// holds nothing older in the window, which is what lets a follower take a
// cursor from what was read.
func readAuditBack[T any](ctx context.Context, sources []auditSource[T], opts auditReadOptions, limit int) (pages [][]T, exhausted []bool, err error) {
	pageSize := min(limit, auditPageLimit)
	pages = make([][]T, len(sources))
	exhausted = make([]bool, len(sources))
	for i, source := range sources {
		cursor := auditReadCursor{Since: opts.since}
		seen := map[string]struct{}{}
		for len(pages[i]) < limit {
			page, read, err := readAuditPage(ctx, source, opts, cursor, pageSize)
			if err != nil {
				return nil, nil, err
			}
			if !read {
				break
			}
			if !cursor.Until.IsZero() && slices.ContainsFunc(page, func(row T) bool { return source.at(row).After(cursor.Until) }) {
				break
			}
			added := false
			for _, row := range page {
				key := source.key(row)
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				pages[i] = append(pages[i], row)
				added = true
			}
			if len(page) < pageSize {
				exhausted[i] = true
				break
			}
			if !added {
				break
			}
			cursor.Until = source.at(page[len(page)-1])
		}
		if len(pages[i]) > limit {
			pages[i] = pages[i][:limit]
		}
	}
	return pages, exhausted, nil
}

// readAuditPage reads one trail, naming a trail that could not be read rather
// than failing the whole answer when the caller allows it (ADR 0130 §1).
//
// read says whether the trail answered at all, which is not the same question
// as whether the page was short. A caller that reads them as the same thing
// takes a trail's silence for the end of its window.
func readAuditPage[T any](ctx context.Context, source auditSource[T], opts auditReadOptions, cursor auditReadCursor, limit int) (page []T, read bool, err error) {
	page, err = source.read(ctx, cursor, limit)
	if err == nil {
		return page, true, nil
	}
	if opts.unavailable == nil || ctx.Err() != nil {
		return nil, false, err
	}
	opts.unavailable(source.name, err)
	return nil, false, nil
}

// admitBacklog moves each trail's position over its whole backlog page, printed
// or cut.
//
// Over the cut records too, and that is the point: a record the cut dropped is
// older than the tail the caller asked for, so it was never going to print, and
// admitting it leaves the trail anchored on a record of its own — on its own
// machine's clock — rather than on whichever trail filled the cut. It also
// means those records are recognized rather than printed later as new.
//
// By key, because the backlog is read by time, and floored at the trail's own
// oldest record (auditPosition.floor). A trail gets its first cursor here only
// when it was read to its end: then it holds nothing older that a later read
// could turn up, and its highest ID has nothing behind it.
func admitBacklog[T any](sources []auditSource[T], positions []*auditPosition, pages [][]T, exhausted []bool) {
	for i, source := range sources {
		for _, row := range pages[i] {
			at := source.at(row)
			positions[i].admit(source.key(row), at, rowCursor{}, false)
			if positions[i].floor.IsZero() || at.Before(positions[i].floor) {
				positions[i].floor = at
			}
		}
		if !exhausted[i] {
			continue
		}
		for _, row := range pages[i] {
			if id := auditRowID(source, row); id.ordered {
				positions[i].bootstrap(id.partition, id.id)
			}
		}
	}
}

// mergeAuditBatches interleaves the trails' pages into one batch, newest first
// unless forward, cutting to limit when one is given.
//
// It merges by taking heads rather than sorting everything and slicing, for the
// reason the server merges its pools that way: a page read from a cursor is in
// write order, which is not time order, so a cut made after sorting by time can
// fall inside a page and drop a record the position is about to move past.
// Taking heads keeps whatever survives a cut a prefix of every page.
func mergeAuditBatches[T any](sources []auditSource[T], pages [][]T, forward bool, limit int) []T {
	total := 0
	for _, page := range pages {
		total += len(page)
	}
	if limit > 0 && total > limit {
		total = limit
	}
	merged := make([]T, 0, total)
	heads := make([]int, len(pages))
	for len(merged) < total {
		next := -1
		for i, page := range pages {
			if heads[i] >= len(page) {
				continue
			}
			if next < 0 {
				next = i
				continue
			}
			at, best := sources[i].at(page[heads[i]]), sources[next].at(pages[next][heads[next]])
			if at.Equal(best) {
				continue
			}
			if at.Before(best) == forward {
				next = i
			}
		}
		if next < 0 {
			break
		}
		merged = append(merged, pages[next][heads[next]])
		heads[next]++
	}
	return merged
}

// rowCursor is a record's place in its trail's write order, where it has one.
type rowCursor struct {
	partition string
	id        int64
	ordered   bool
}

func auditRowID[T any](source auditSource[T], row T) rowCursor {
	if source.rowID == nil {
		return rowCursor{}
	}
	partition, id, ordered := source.rowID(row)
	return rowCursor{partition: partition, id: id, ordered: ordered}
}

// pace is the gap to leave between the records of one batch. Spread over the
// poll interval, a batch reads as a trail arriving rather than a block of rows
// appearing every two seconds.
func pace(opts auditReadOptions, full bool, records int) time.Duration {
	// Never while catching up: a full page means the trail is ahead of the
	// reader, and slowing that down would keep it behind.
	if !opts.paced || full || records < 2 {
		return 0
	}
	// One gap short of the interval, so a batch is finished before the next
	// read rather than colliding with it.
	return auditFollowInterval / time.Duration(records+1)
}

// emitAudit prints a batch, one record at a time when there is a gap to leave
// between them.
func emitAudit[T any](ctx context.Context, emit func([]T) error, rows []T, gap time.Duration) error {
	if gap <= 0 {
		if len(rows) == 0 {
			return nil
		}
		return emit(rows)
	}
	for i := range rows {
		if err := emit(rows[i : i+1]); err != nil {
			return err
		}
		if i == len(rows)-1 {
			break
		}
		if !sleepContext(ctx, gap) {
			return ctx.Err()
		}
	}
	return nil
}

// followEnded is a read's error, unless the read failed because the follow was
// stopped: that ends it, and is not a failure.
func followEnded(ctx context.Context, err error) error {
	select {
	case <-ctx.Done():
		return nil
	default:
		return err
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// auditTime is how a record's time is printed: relative for a one-off read,
// where "3m ago" is what a person wants, and as a clock time while following,
// where a relative time is stale the moment it is printed.
func auditTime(at time.Time, follow bool) string {
	if follow {
		return at.Local().Format("15:04:05")
	}
	return formatTime(at)
}

// auditColumn is one column of an audit table. A zero width is the last
// column, which takes whatever is left.
type auditColumn struct {
	name  string
	width int
}

// auditTable prints audit records as columns.
//
// A followed trail prints a record at a time — that is what pacing is — and a
// tabwriter aligns only the rows of one flush, so following through one would
// re-size the columns on every record. Declared widths are what make a record
// printed now line up with one printed a minute ago; a one-off read has all its
// rows in hand and lets the tabwriter size them.
type auditTable[T any] struct {
	columns []auditColumn
	row     func(T) []string
}

func (t auditTable[T]) names() []string {
	names := make([]string, 0, len(t.columns))
	for _, column := range t.columns {
		names = append(names, column.name)
	}
	return names
}

func (t auditTable[T]) write(out io.Writer, rows []T, header, fixed bool) error {
	if fixed {
		return t.writeFixed(out, rows, header)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if header {
		_, _ = fmt.Fprintln(tw, strings.Join(t.names(), "\t"))
	}
	for _, record := range rows {
		_, _ = fmt.Fprintln(tw, strings.Join(t.row(record), "\t"))
	}
	return tw.Flush()
}

func (t auditTable[T]) writeFixed(out io.Writer, rows []T, header bool) error {
	if header {
		if _, err := fmt.Fprintln(out, t.fixedLine(t.names())); err != nil {
			return err
		}
	}
	for _, record := range rows {
		if _, err := fmt.Fprintln(out, t.fixedLine(t.row(record))); err != nil {
			return err
		}
	}
	return nil
}

func (t auditTable[T]) fixedLine(values []string) string {
	var b strings.Builder
	for i, column := range t.columns {
		if i >= len(values) {
			break
		}
		value := values[i]
		if column.width <= 0 {
			b.WriteString(value)
			continue
		}
		value = truncateTableValue(value, column.width)
		if pad := column.width - len([]rune(value)); pad > 0 {
			value += strings.Repeat(" ", pad)
		}
		b.WriteString(value)
		b.WriteString("  ")
	}
	return strings.TrimRight(b.String(), " ")
}

// auditOnce reports a condition about the answer — a pool that could not be
// read, a trail that is unavailable — once for as long as it holds, rather than
// on every poll of a follow.
type auditOnce struct {
	last string
}

func (o *auditOnce) changed(message string) bool {
	if message == o.last {
		return false
	}
	o.last = message
	return true
}

// writeTerminalSafeJSONLine writes value as one line of compact JSON, escaped
// as writeTerminalSafeJSON escapes it, for a follow's stream of records.
func writeTerminalSafeJSONLine(cmd *cobra.Command, value any) error {
	var indented bytes.Buffer
	if err := writeTerminalSafeJSON(&indented, value); err != nil {
		return err
	}
	var line bytes.Buffer
	if err := json.Compact(&line, indented.Bytes()); err != nil {
		return err
	}
	line.WriteByte('\n')
	_, err := cmd.OutOrStdout().Write(line.Bytes())
	return err
}

// writeTerminalSafeJSONLines writes each record as its own line.
func writeTerminalSafeJSONLines[T any](cmd *cobra.Command, records []T) error {
	for _, record := range records {
		if err := writeTerminalSafeJSONLine(cmd, &record); err != nil {
			return err
		}
	}
	return nil
}
