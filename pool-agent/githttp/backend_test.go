package githttp

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// TestBackendEnvForwardsContentEncoding pins the CGI variable git http-backend
// needs to inflate a compressed request. git gzips an upload-pack request once
// negotiation runs past a round or two, so without this a fetch works for a
// small negotiation and fails for a large one — the backend reads gzip bytes as
// pkt-line, answers nothing, and the client reports that the remote end hung up.
func TestBackendEnvForwardsContentEncoding(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/repo.git/git-upload-pack", strings.NewReader("body"))
	r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	r.Header.Set("Content-Encoding", "gzip")

	env := backendEnv(r, "/srv/repo", "/git-upload-pack")
	if !slices.Contains(env, "HTTP_CONTENT_ENCODING=gzip") {
		t.Fatalf("HTTP_CONTENT_ENCODING missing from CGI environment: %v", env)
	}
}

func TestBackendEnvOmitsContentEncodingWhenAbsent(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/repo.git/git-upload-pack", strings.NewReader("body"))
	env := backendEnv(r, "/srv/repo", "/git-upload-pack")
	for _, entry := range env {
		if strings.HasPrefix(entry, "HTTP_CONTENT_ENCODING=") {
			t.Fatalf("unencoded request must not claim an encoding: %q", entry)
		}
	}
}
