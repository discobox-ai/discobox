package transport

import (
	"net/http"
	"sync"
)

// HTTPClientLease holds a transport client until Release is called. Client may
// use any RoundTripper, including transports that rewrite a logical endpoint to
// a Unix socket, VS Code socket, tunnel, or provider proxy. BaseURL is optional;
// when empty, callers use their own logical URL.
type HTTPClientLease struct {
	Client    *http.Client
	BaseURL   string
	AuthToken string
	release   func()
	once      sync.Once
}

// NewHTTPClientLease creates a lease around a client and release callback.
func NewHTTPClientLease(client *http.Client, release func()) *HTTPClientLease {
	return &HTTPClientLease{Client: client, release: release}
}

// NewHTTPClientLeaseWithBaseURL creates a lease with a preferred logical base URL.
func NewHTTPClientLeaseWithBaseURL(client *http.Client, baseURL string, release func()) *HTTPClientLease {
	return &HTTPClientLease{Client: client, BaseURL: baseURL, release: release}
}

// NewHTTPClientLeaseWithBaseURLAndAuth creates a lease with a base URL and bearer token.
func NewHTTPClientLeaseWithBaseURLAndAuth(client *http.Client, baseURL, authToken string, release func()) *HTTPClientLease {
	return &HTTPClientLease{Client: client, BaseURL: baseURL, AuthToken: authToken, release: release}
}

// NewHTTPClientLeaseWithAuth creates an authenticated lease for a client that
// handles the logical URL itself, for example by dialing a socket or tunnel from
// a custom RoundTripper.
func NewHTTPClientLeaseWithAuth(client *http.Client, authToken string, release func()) *HTTPClientLease {
	return &HTTPClientLease{Client: client, AuthToken: authToken, release: release}
}

// Release returns the leased client.
func (l *HTTPClientLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.release != nil {
			l.release()
		}
	})
}
