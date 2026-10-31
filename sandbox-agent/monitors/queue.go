package monitors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/sharedfile"
	"github.com/fsnotify/fsnotify"
)

const (
	// PendingCap is the most events one instance may have pending. Past it
	// the oldest are dropped, and one monitor.dropped event says how many:
	// it is what bounds a monitor that floods.
	PendingCap = 200
	// ReadKeep and ReadMaxAge bound what stays readable after a pop.
	ReadKeep   = 1000
	ReadMaxAge = 7 * 24 * time.Hour
)

// seqFile records the highest id ever allocated, so an id is not handed out
// again after the event that had it was pruned.
const seqFile = ".seq"

// Queue is the event queue: one file per event, `e<N>.json`, in Pending until
// a pop claims it into Read.
//
// It needs no lock and no server. An id is taken by hard-linking a written
// file to its name, which fails when the name exists, so two writers can never
// take one id; a pop claims an event the same way, linking it into Read, so
// only one of two pops can take it.
type Queue struct {
	Pending string
	Read    string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

func (q *Queue) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// Filter selects events by instance; empty selects all.
type Filter struct {
	Instance string
}

func (f Filter) match(ev Event) bool {
	return f.Instance == "" || ev.Instance == f.Instance
}

// Add queues ev for its instance, giving it an id and a time.
//
// An event with a key replaces a pending one of the same instance and key,
// and stands for both in its Count. When the instance already has PendingCap
// events pending, the oldest are dropped first and a monitor.dropped event,
// itself keyed, says how many have been.
func (q *Queue) Add(ev Event) (Event, error) {
	return q.add(ev, true)
}

func (q *Queue) add(ev Event, capped bool) (Event, error) {
	if err := os.MkdirAll(q.Pending, 0o755); err != nil {
		return Event{}, err
	}
	pending, err := q.list(q.Pending)
	if err != nil {
		return Event{}, err
	}
	if ev.Count < 1 {
		ev.Count = 1
	}
	var mine []Event
	for _, other := range pending {
		if other.Instance != ev.Instance {
			continue
		}
		if ev.Key != "" && other.Key == ev.Key {
			// A pop that took it first leaves nothing to replace — or only the
			// pending name of what it has already claimed into Read — and then
			// it does not count: it was delivered.
			err := sharedfile.Retry(func() error { return os.Remove(q.path(q.Pending, other.ID)) }, 0)
			if _, claimed := os.Stat(q.path(q.Read, other.ID)); err == nil && claimed != nil {
				ev.Count += max(other.Count, 1)
			}
			continue
		}
		mine = append(mine, other)
	}
	if capped && len(mine) >= PendingCap {
		drop := mine[:len(mine)-PendingCap+1]
		dropped := 0
		for _, old := range drop {
			if os.Remove(q.path(q.Pending, old.ID)) == nil {
				dropped += max(old.Count, 1)
			}
		}
		if dropped > 0 {
			notice := Event{
				Instance: ev.Instance,
				Monitor:  ev.Monitor,
				Kind:     KindDropped,
				Key:      KindDropped,
				Count:    dropped,
				Title:    fmt.Sprintf("%s: older events dropped; it had %d pending", ev.Instance, PendingCap),
			}
			if _, err := q.add(notice, false); err != nil {
				return Event{}, err
			}
		}
	}
	ev.Time = q.now().UTC()
	return q.write(ev)
}

// write gives ev the next free id and links it into Pending under it.
func (q *Queue) write(ev Event) (Event, error) {
	seq := q.highestID() + 1
	tmp, err := os.CreateTemp(q.Pending, ".event-*")
	if err != nil {
		return Event{}, err
	}
	defer os.Remove(tmp.Name())
	for {
		ev.ID = "e" + strconv.FormatUint(seq, 10)
		// An id a pop has claimed into Read is taken too: its pending name is
		// gone, and a second event under it would be taken for the first. A
		// claim landing between this look and the link below can still give
		// two events one id; a pop that meets that settles it.

		if _, err := os.Lstat(q.path(q.Read, ev.ID)); err == nil {
			seq++
			continue
		}
		data, err := json.Marshal(ev)
		if err != nil {
			tmp.Close()
			return Event{}, err
		}
		if err := tmp.Truncate(0); err != nil {
			tmp.Close()
			return Event{}, err
		}
		if _, err := tmp.WriteAt(append(data, '\n'), 0); err != nil {
			tmp.Close()
			return Event{}, err
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return Event{}, err
		}
		err = os.Link(tmp.Name(), q.path(q.Pending, ev.ID))
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			tmp.Close()
			return Event{}, err
		}
		seq++
	}
	tmp.Close()
	// Best effort: the directories are the authority, and this only keeps an
	// id from coming back once the events above it are pruned.
	_ = writeFileAtomic(filepath.Join(q.Pending, seqFile), []byte(strconv.FormatUint(seq, 10)+"\n"))
	return ev, nil
}

// highestID is the highest id the queue has given out that it can still see.
func (q *Queue) highestID() uint64 {
	var high uint64
	if data, err := os.ReadFile(filepath.Join(q.Pending, seqFile)); err == nil {
		high, _ = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	}
	for _, dir := range []string{q.Pending, q.Read} {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if n, ok := idNumber(strings.TrimSuffix(entry.Name(), ".json")); ok && n > high {
				high = n
			}
		}
	}
	return high
}

// idNumber is the N of `eN`.
func idNumber(id string) (uint64, bool) {
	if !strings.HasPrefix(id, "e") {
		return 0, false
	}
	n, err := strconv.ParseUint(id[1:], 10, 64)
	return n, err == nil
}

// ValidID reports whether id has the shape the queue gives out.
func ValidID(id string) bool {
	_, ok := idNumber(id)
	return ok
}

func (q *Queue) path(dir, id string) string { return filepath.Join(dir, id+".json") }

// list is every event in dir, oldest first.
func (q *Queue) list(dir string) ([]Event, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	type numbered struct {
		n  uint64
		id string
	}
	var ids []numbered
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if n, ok := idNumber(id); ok {
			ids = append(ids, numbered{n, id})
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].n < ids[j].n })
	out := make([]Event, 0, len(ids))
	for _, id := range ids {
		ev, err := readEvent(q.path(dir, id.id))
		if err != nil {
			// Popped or replaced between the listing and the read.
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

func readEvent(path string) (Event, error) {
	var data []byte
	err := sharedfile.Retry(func() (err error) {
		data, err = os.ReadFile(path)
		return err
	}, 0)
	if err != nil {
		return Event{}, err
	}
	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return Event{}, fmt.Errorf("%s: %w", path, err)
	}
	return ev, nil
}

// Peek is the pending events f selects, oldest first, at most limit of them
// (none is no limit), leaving them pending.
func (q *Queue) Peek(f Filter, limit int) ([]Event, error) {
	pending, err := q.list(q.Pending)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, ev := range pending {
		if !f.match(ev) {
			continue
		}
		out = append(out, ev)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// Pop is Peek, taking what it returns: each event is claimed into Read, and
// one another pop claimed first is not returned.
func (q *Queue) Pop(f Filter, limit int) ([]Event, error) {
	candidates, err := q.Peek(f, 0)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(q.Read, 0o755); err != nil {
		return nil, err
	}
	var out []Event
	for _, ev := range candidates {
		if limit > 0 && len(out) == limit {
			break
		}
		// Claimed by hard-linking it into Read, which fails when the name is
		// there already: of two pops, one wins on every platform. A rename
		// would not do — on Windows it replaces the name it moves onto, and
		// both pops would take the event.
		pending, read := q.path(q.Pending, ev.ID), q.path(q.Read, ev.ID)
		if err := sharedfile.Retry(func() error { return os.Link(pending, read) }, 0); err != nil {
			if errors.Is(err, fs.ErrExist) {
				q.settle(pending, read)
				continue
			}
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return out, err
		}
		if err := sharedfile.Retry(func() error { return os.Remove(pending) }, 0); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return out, err
		}
		// Read back what was taken: a newer event with its key may have
		// replaced the one listed, under another id, in between.
		taken, err := readEvent(q.path(q.Read, ev.ID))
		if err != nil {
			return out, err
		}
		out = append(out, taken)
	}
	q.prune()
	return out, nil
}

// settle resolves a pending event whose id is already in Read. Either it is
// the same file — claimed, and the claimer died before removing its pending
// name, which would leave it pending for good and a waiting pop woken by it
// forever — and the name goes; or it is another event that was given the id
// while the claim was being made, and it is queued again under a new id
// rather than removed: never lost, only renumbered.
func (q *Queue) settle(pending, read string) {
	pendingInfo, err := os.Lstat(pending)
	if err != nil {
		return
	}
	if readInfo, err := os.Lstat(read); err == nil && os.SameFile(pendingInfo, readInfo) {
		_ = sharedfile.Retry(func() error { return os.Remove(pending) }, 0)
		return
	}
	// Two pops can meet the same collision. Only the one whose remove of the
	// pending name succeeds queues the event again, from a name of its own
	// taken first; the other finds the name gone and leaves it.
	held, err := os.CreateTemp(q.Pending, ".settle-*")
	if err != nil {
		return
	}
	heldName := held.Name()
	held.Close()
	_ = os.Remove(heldName)
	defer os.Remove(heldName)
	if os.Link(pending, heldName) != nil {
		return
	}
	if sharedfile.Retry(func() error { return os.Remove(pending) }, 0) != nil {
		return
	}
	if ev, err := readEvent(heldName); err == nil {
		_, _ = q.write(ev)
	}
}

// Count is how many events f selects are pending.
func (q *Queue) Count(f Filter) (int, error) {
	pending, err := q.Peek(f, 0)
	return len(pending), err
}

// Get is the event with id, pending or already popped.
func (q *Queue) Get(id string) (Event, error) {
	if !ValidID(id) {
		return Event{}, fmt.Errorf("%q is not an event id (e17)", id)
	}
	for _, dir := range []string{q.Pending, q.Read} {
		ev, err := readEvent(q.path(dir, id))
		if err == nil {
			return ev, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return Event{}, err
		}
	}
	return Event{}, fmt.Errorf("no event %s: it was never queued, or was pruned", id)
}

// Wait returns once an event f selects is pending, or ctx ends.
//
// It watches Pending, and also looks again every few seconds: a watch can miss
// a directory created after it began, and a missed wake must cost seconds
// rather than the whole wait.
func (q *Queue) Wait(ctx context.Context, f Filter) error {
	if err := os.MkdirAll(q.Pending, 0o755); err != nil {
		return err
	}
	watcher, err := fsnotify.NewWatcher()
	if err == nil {
		defer watcher.Close()
		if watcher.Add(q.Pending) != nil {
			watcher.Close()
			watcher = nil
		}
	} else {
		watcher = nil
	}
	var events <-chan fsnotify.Event
	if watcher != nil {
		events = watcher.Events
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		n, err := q.Count(f)
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-events:
		case <-ticker.C:
		}
	}
}

// prune drops popped events past ReadKeep, and any older than ReadMaxAge.
func (q *Queue) prune() {
	read, err := q.list(q.Read)
	if err != nil {
		return
	}
	cutoff := q.now().Add(-ReadMaxAge)
	for i, ev := range read {
		if i < len(read)-ReadKeep || ev.Time.Before(cutoff) {
			_ = os.Remove(q.path(q.Read, ev.ID))
		}
	}
}
