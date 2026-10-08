package secrets

import (
	"net/http"
	"net/netip"
	"testing"
)

// SetTokenHTTPClient makes renewals and exchanges through client's transport
// for the length of a test, such as an httptest TLS server's, which trusts its
// certificate. Everything else stays the real client's, its refusal to follow
// a redirect above all.
func SetTokenHTTPClient(t testing.TB, client *http.Client) {
	t.Helper()
	previous := tokenHTTPClient
	withTransport := *previous
	withTransport.Transport = client.Transport
	tokenHTTPClient = &withTransport
	t.Cleanup(func() { tokenHTTPClient = previous })
}

// AllowInternalExchangeTargets lets exchanges reach internal addresses for the
// length of a test, whose token server listens on loopback.
func AllowInternalExchangeTargets(t testing.TB) {
	t.Helper()
	previous := internalAddress
	internalAddress = func(netip.Addr) bool { return false }
	t.Cleanup(func() { internalAddress = previous })
}
