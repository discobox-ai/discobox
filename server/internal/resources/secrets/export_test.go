package secrets

import (
	"net/http"
	"testing"
)

// SetTokenHTTPClient makes renewals through client's transport
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
