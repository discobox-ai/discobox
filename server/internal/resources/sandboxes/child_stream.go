package sandboxes

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/server/internal/auth"
)

// childTransport registers every child request before forwarding. It rechecks
// after registration, then watches committed primary-database notifications.
// A lost notification channel fails closed, including across server instances.
type childTransport struct {
	base      http.RoundTripper
	service   *Service
	principal auth.Principal
	target    string
	write     bool
}

func (t *childTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(r.Context())
	version, err := t.service.store.ChildAuthorityVersion(ctx, t.principal.ProjectID, t.principal.SandboxID, t.target, t.write)
	if err != nil {
		cancel()
		return nil, err
	}
	if _, err := (auth.SandboxRoleAuthorizer{Store: t.service.store}).AuthorizeChild(ctx, t.principal, t.target, t.write, r.Method+" "+r.URL.Path); err != nil {
		cancel()
		return nil, err
	}
	life := &childLifetime{cancel: cancel, request: r.Body}
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		// Recheck at once after registration; no mutation can fall in the gap
		// between the initial check and subscription.
		for {
			current, e := t.service.store.ChildAuthorityVersion(ctx, t.principal.ProjectID, t.principal.SandboxID, t.target, t.write)
			if e != nil || current != version {
				life.close()
				return
			}
			select {
			case <-ctx.Done():
				life.close()
				return
			case <-ticker.C:
			}
		}
	}()
	resp, err := t.base.RoundTrip(r.WithContext(ctx))
	if err != nil {
		life.close()
		return nil, err
	}
	life.mu.Lock()
	life.response = resp.Body
	closed := life.closed
	life.mu.Unlock()
	if closed {
		resp.Body.Close()
	}
	if rw, ok := resp.Body.(io.ReadWriteCloser); ok && resp.StatusCode == http.StatusSwitchingProtocols {
		resp.Body = &childDuplex{rw, life}
	} else {
		resp.Body = &childBody{resp.Body, life}
	}
	return resp, nil
}

type childLifetime struct {
	mu                sync.Mutex
	closed            bool
	cancel            context.CancelFunc
	request, response io.Closer
}

func (l *childLifetime) close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	request, response := l.request, l.response
	l.mu.Unlock()
	l.cancel()
	if request != nil {
		request.Close()
	}
	if response != nil {
		response.Close()
	}
}

type childBody struct {
	io.ReadCloser
	life *childLifetime
}

func (b *childBody) Close() error { b.life.close(); return nil }

type childDuplex struct {
	io.ReadWriteCloser
	life *childLifetime
}

func (b *childDuplex) Close() error { b.life.close(); return nil }
