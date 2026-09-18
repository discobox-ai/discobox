package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/auditid"
	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// artifactPools answers OpenHTTPAuditArtifact and records what it was asked;
// every other PoolService method is the nil embedded interface.
type artifactPools struct {
	services.PoolService
	asked  *[]string
	closed *bool
}

func (p artifactPools) OpenHTTPAuditArtifact(_ context.Context, projectID, poolID, sandboxID string, id auditid.ExchangeID, artifact string) (*sandbox.HTTPAuditArtifact, error) {
	*p.asked = []string{projectID, poolID, sandboxID, artifact, id.String()}
	return &sandbox.HTTPAuditArtifact{
		Body:        closeRecorder{Reader: strings.NewReader("<html>recorded</html>"), closed: p.closed},
		Format:      "raw",
		ContentType: "text/html",
	}, nil
}

type closeRecorder struct {
	io.Reader
	closed *bool
}

func (c closeRecorder) Close() error {
	*c.closed = true
	return nil
}

func TestPoolHTTPAuditArtifactRouteStreamsTheRecordedBody(t *testing.T) {
	var asked []string
	var closed bool
	router := chi.NewRouter()
	registerPoolHTTPAuditRoutes(router, artifactPools{asked: &asked, closed: &closed})

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/project-1/pools/pool-a/audit/http/http_3/response-body?sandboxId=sbx_1", nil))
	if resp.Code != http.StatusOK || resp.Body.String() != "<html>recorded</html>" {
		t.Fatalf("status = %d, body = %q", resp.Code, resp.Body.String())
	}
	if got := strings.Join(asked, " "); got != "project-1 pool-a sbx_1 response-body http_3" {
		t.Fatalf("asked %q", got)
	}
	if resp.Header().Get(httpAuditFormatHeader) != "raw" || resp.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(resp.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("headers = %v, want the format and a body that is never rendered as this server's page", resp.Header())
	}
	if !closed {
		t.Fatal("the artifact body, which holds a pool-agent lease, was not closed")
	}

	for _, path := range []string{
		"/api/projects/project-1/pools/pool-a/audit/http/http_0/response-body",
		"/api/projects/project-1/pools/pool-a/audit/http/3/response-body",
		"/api/projects/project-1/pools/pool-a/audit/http/abc/response-body",
		"/api/projects/project-1/pools/pool-a/audit/http/http_3/headers",
	} {
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("GET %s = %d, want 400", path, resp.Code)
		}
	}
}
