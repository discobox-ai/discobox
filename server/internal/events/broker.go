// Package events provides in-process project event fanout.
package events

import (
	"context"
	"sync"

	"github.com/obot-platform/discobox/model"
)

// Broker fans out committed project events to active subscribers.
type Broker struct {
	mu          sync.Mutex
	subscribers map[string]map[chan model.ProjectEvent]struct{}
}

func NewBroker() *Broker {
	return &Broker{
		subscribers: make(map[string]map[chan model.ProjectEvent]struct{}),
	}
}

func (b *Broker) Subscribe(ctx context.Context, projectID string) (<-chan model.ProjectEvent, func()) {
	ch := make(chan model.ProjectEvent, 64)

	b.mu.Lock()
	if b.subscribers[projectID] == nil {
		b.subscribers[projectID] = make(map[chan model.ProjectEvent]struct{})
	}
	b.subscribers[projectID][ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			if subs := b.subscribers[projectID]; subs != nil {
				delete(subs, ch)
				if len(subs) == 0 {
					delete(b.subscribers, projectID)
				}
			}
			b.mu.Unlock()
			close(ch)
		})
	}

	go func() {
		<-ctx.Done()
		unsubscribe()
	}()

	return ch, unsubscribe
}

func (b *Broker) PublishProjectEvent(_ context.Context, event model.ProjectEvent) {
	b.mu.Lock()
	subs := make([]chan model.ProjectEvent, 0, len(b.subscribers[event.ProjectID]))
	for ch := range b.subscribers[event.ProjectID] {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- event:
		default:
		}
	}
}
