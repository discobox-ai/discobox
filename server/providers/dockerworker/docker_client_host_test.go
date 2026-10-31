package dockerworker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

// A daemon reached at a plain tcp:// endpoint, as an exec provider names one,
// is spoken to over plain HTTP: the client once chose https and every call
// failed with "server gave HTTP response to HTTPS client".
func TestADockerClientForAPlainEndpointSpeaksHTTP(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("Api-Version", "1.45")
			_, _ = w.Write([]byte("OK"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(daemon.Close)

	cli, err := NewDockerClientForHost("tcp://" + strings.TrimPrefix(daemon.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	if _, err := cli.Ping(t.Context(), client.PingOptions{}); err != nil {
		t.Fatalf("ping a plain daemon: %v", err)
	}
}
