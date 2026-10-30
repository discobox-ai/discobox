package events

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// defaultPollInterval is how often a tailer reads the log while a stream is
// open: ADR 0081's attach-wait interval, which ADR 26-10-30-868 §7 takes.
const defaultPollInterval = 500 * time.Millisecond

// tailPage is how many events one poll reads. A poll that reads a full page
// reads again at once.
const tailPage = 1000

// subscriptionBuffer is how many events a stream may lag the log by before
// the tailer gives up on it.
const subscriptionBuffer = 256

// tailer is this node's one reader of the log's head (ADR 26-10-30-868 §7).
// It runs only while a stream is subscribed, polls for every event after the
// last it read, and hands each to the subscriptions of its project.
//
// A subscription that cannot take an event is closed, never thinned: its
// stream ends, and its client resumes from the last event it read, so nothing
// is lost. Dropping the event instead is what made the stream ADR 0061
// removed unreliable.
type tailer struct {
	store    *store.Store
	interval time.Duration
	// buffer is how many events a subscription may lag by.
	buffer int

	mu            sync.Mutex
	subscriptions map[*subscription]struct{}
	// delivered is the last ID the running loop has handed out, of any
	// project: every event at or below it reached each subscription of its
	// project, or was in the log before that subscription's stream read it.
	delivered int64
	// stop ends the running poll loop; nil when none runs.
	stop context.CancelFunc
}

// subscription is one stream's delivery from the tailer.
type subscription struct {
	projectID string
	events    chan model.ProjectEvent
	// behind is closed when the tailer gives up on the subscription.
	behind chan struct{}
}

func newTailer(st *store.Store, interval time.Duration, buffer int) *tailer {
	return &tailer{store: st, interval: interval, buffer: buffer, subscriptions: map[*subscription]struct{}{}}
}

// subscribe registers a subscription, starting the poll loop if it is the
// first. The loop starts after the log's head as it stands now, read before
// subscribe returns, so whatever the subscription's stream reads of the log
// after subscribing reaches at least that far, and nothing between is lost.
func (t *tailer) subscribe(ctx context.Context, projectID string) (*subscription, error) {
	sub := &subscription{
		projectID: projectID,
		events:    make(chan model.ProjectEvent, t.buffer),
		behind:    make(chan struct{}),
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stop == nil {
		cursor, _, err := t.store.ProjectEventLogBounds(ctx)
		if err != nil {
			return nil, err
		}
		loop, cancel := context.WithCancel(context.Background())
		t.stop = cancel
		t.delivered = cursor
		go t.run(loop, cursor)
	}
	t.subscriptions[sub] = struct{}{}
	return sub, nil
}

// unsubscribe removes a subscription, stopping the poll loop with the last.
func (t *tailer) unsubscribe(sub *subscription) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.subscriptions, sub)
	if len(t.subscriptions) == 0 && t.stop != nil {
		t.stop()
		t.stop = nil
	}
}

func (t *tailer) run(ctx context.Context, cursor int64) {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for {
			events, err := t.store.ListProjectEventsAfter(ctx, "", cursor, tailPage)
			if err != nil {
				if ctx.Err() == nil {
					slog.WarnContext(ctx, "the event tailer could not read the log", "error", err)
				}
				break
			}
			for _, event := range events {
				if !t.deliver(ctx, event) {
					return
				}
				cursor = event.ID
			}
			if len(events) < tailPage {
				break
			}
		}
	}
}

// deliver hands an event to every subscription of its project, giving up on
// any that cannot take it. It reports false, delivering nothing, once its
// loop has been stopped: the stop is made under the same lock, so a loop that
// lost its last subscription never hands a stale event to the subscriptions
// of the loop that replaced it.
func (t *tailer) deliver(loop context.Context, event model.ProjectEvent) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if loop.Err() != nil {
		return false
	}
	for sub := range t.subscriptions {
		if sub.projectID != event.ProjectID {
			continue
		}
		select {
		case sub.events <- event:
		default:
			delete(t.subscriptions, sub)
			close(sub.behind)
		}
	}
	t.delivered = event.ID
	return true
}

// deliveredThrough is the last ID handed out, when sub holds nothing of it
// unread: then every event of sub's project at or below it is one sub's
// stream has already passed on, or read from the log itself. It reports false
// while sub has events waiting or has been given up on.
func (t *tailer) deliveredThrough(sub *subscription) (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.subscriptions[sub]; !ok || len(sub.events) > 0 {
		return 0, false
	}
	return t.delivered, true
}
