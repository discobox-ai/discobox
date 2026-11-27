package handover

import (
	"context"
	"net/http"
	"sync"
)

// InFlight counts the requests a server is handling, an upgraded stream's
// whole life included: net/http's Shutdown forgets a connection once it is
// hijacked, and an exec, a terminal or the create socket is a handler that
// runs for as long as its stream does.
type InFlight struct {
	mu    sync.Mutex
	count int
	idle  chan struct{}
}

// Wrap counts each request next handles, until its handler returns.
func (f *InFlight) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done := f.Begin()
		defer done()
		next.ServeHTTP(w, r)
	})
}

// Begin counts one piece of work, until the func it returns is called.
func (f *InFlight) Begin() func() {
	f.mu.Lock()
	f.count++
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.count--
			if f.count == 0 && f.idle != nil {
				close(f.idle)
				f.idle = nil
			}
		})
	}
}

// Wait returns once nothing is in flight, or with ctx's error once ctx ends.
func (f *InFlight) Wait(ctx context.Context) error {
	f.mu.Lock()
	if f.count == 0 {
		f.mu.Unlock()
		return nil
	}
	if f.idle == nil {
		f.idle = make(chan struct{})
	}
	idle := f.idle
	f.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
