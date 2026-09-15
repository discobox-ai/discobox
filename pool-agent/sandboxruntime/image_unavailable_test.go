package sandboxruntime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

// fakeDaemon answers the two calls ensureImageAvailable makes: an inspect that
// finds nothing, and a pull the daemon refuses with pullStatus.
func fakeDaemon(t *testing.T, pullStatus int, pullMessage string) *DockerSandboxRuntime {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/images/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such image"}`))
		case strings.HasSuffix(r.URL.Path, "/images/create"):
			w.WriteHeader(pullStatus)
			_, _ = w.Write([]byte(`{"message":"` + pullMessage + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &DockerSandboxRuntime{client: cli}
}

// A reference no registry will hand the pool is an answer about the image: the
// sandbox cannot run on it however many times it is retried, and the control
// plane has to be told that rather than handed a pull that merely failed.
func TestPullOfAnImageNoRegistryHasIsUnavailable(t *testing.T) {
	for name, tc := range map[string]struct {
		status  int
		message string
	}{
		"no such repository": {http.StatusNotFound, "pull access denied for discobox/harness, repository does not exist or may require 'docker login'"},
		"no such tag":        {http.StatusNotFound, "docker.io/library/alpine:gone: not found"},
	} {
		t.Run(name, func(t *testing.T) {
			r := fakeDaemon(t, tc.status, tc.message)
			err := r.ensureImageAvailable(context.Background(), "sbx_1", "discobox/harness:local")
			if !errors.Is(err, ErrImageUnavailable) {
				t.Fatalf("ensureImageAvailable = %v, want ErrImageUnavailable", err)
			}
		})
	}
}

// A pull refused for credentials, or failing for the daemon's own reasons, has
// said nothing about whether the image exists. It stays an ordinary failure:
// fixing credentials or retrying is what helps, and an upgrade offered for it
// would sit behind the same refusal.
func TestPullThatFailsForOtherReasonsIsNotUnavailable(t *testing.T) {
	for name, tc := range map[string]struct {
		status  int
		message string
	}{
		"not authorized": {http.StatusUnauthorized, "unauthorized: authentication required"},
		"forbidden":      {http.StatusForbidden, "denied: requested access to the resource is denied"},
		"daemon error":   {http.StatusInternalServerError, "context deadline exceeded"},
	} {
		t.Run(name, func(t *testing.T) {
			r := fakeDaemon(t, tc.status, tc.message)
			err := r.ensureImageAvailable(context.Background(), "sbx_1", "discobox/harness:v1")
			if err == nil || errors.Is(err, ErrImageUnavailable) {
				t.Fatalf("ensureImageAvailable = %v, want a plain pull failure", err)
			}
		})
	}
}
