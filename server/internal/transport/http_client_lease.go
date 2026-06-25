package transport

import (
	"context"
	"net/http"
	"sync"
)

// HTTPClientLease holds a transport client until Release is called. Client may
// use any RoundTripper, including transports that rewrite a logical endpoint to
// a Unix socket, VS Code socket, tunnel, or provider proxy. BaseURL is optional;
// when empty, callers use their own logical URL.
type HTTPClientLease struct {
	Client                   *http.Client
	BaseURL                  string
	AuthToken                string
	AuthTokenProvider        func(context.Context) (string, error)
	ForwardAuthTokenProvider func(context.Context) (string, error)
	release                  func()
	once                     sync.Once
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

// NewHTTPClientLeaseWithBaseURLAndAuthProvider creates a lease with a base URL
// and a bearer token provider. The provider is invoked by callers immediately
// before sending a request so short-lived credentials are not cached on leases.
func NewHTTPClientLeaseWithBaseURLAndAuthProvider(client *http.Client, baseURL string, authTokenProvider func(context.Context) (string, error), release func()) *HTTPClientLease {
	return &HTTPClientLease{Client: client, BaseURL: baseURL, AuthTokenProvider: authTokenProvider, release: release}
}

// NewHTTPClientLeaseWithAuth creates an authenticated lease for a client that
// handles the logical URL itself, for example by dialing a socket or tunnel from
// a custom RoundTripper.
func NewHTTPClientLeaseWithAuth(client *http.Client, authToken string, release func()) *HTTPClientLease {
	return &HTTPClientLease{Client: client, AuthToken: authToken, release: release}
}

// NewHTTPClientLeaseWithAuthProvider creates an authenticated lease for a client
// whose transport handles the logical URL itself.
func NewHTTPClientLeaseWithAuthProvider(client *http.Client, authTokenProvider func(context.Context) (string, error), release func()) *HTTPClientLease {
	return &HTTPClientLease{Client: client, AuthTokenProvider: authTokenProvider, release: release}
}

func (l *HTTPClientLease) AuthorizationToken(ctx context.Context) (string, error) {
	if l == nil {
		return "", nil
	}
	if l.AuthTokenProvider != nil {
		return l.AuthTokenProvider(ctx)
	}
	return l.AuthToken, nil
}

func (l *HTTPClientLease) ForwardAuthorizationToken(ctx context.Context) (string, error) {
	if l == nil || l.ForwardAuthTokenProvider == nil {
		return "", nil
	}
	return l.ForwardAuthTokenProvider(ctx)
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
