package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Judging is its own authority (ADR 0148 §2): a token for a discobox's own
// work cannot ask the judge anything, whatever else it may do.
func TestJudgeRequiresItsOwnScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	job := `{"kind":"request","purpose":"open a PR","host":"api.github.com","round":1,` +
		`"request":{"method":"POST","url":"https://api.github.com/graphql"}}`

	for _, tc := range []struct {
		name, scope string
		refused     bool
	}{
		{"a token for the sandbox's own work", ScopeExecWrite, true},
		{"a token that may read what it does", ScopeExecRead, true},
		{"a token for terminals", ScopeTerminalWrite, true},
		{"a token that may ask the judge", ScopeJudgeRun, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				"/api/projects/project-1/sandboxes/sandbox-1/judge", strings.NewReader(job))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", tc.scope))
			router.ServeHTTP(resp, req)

			switch {
			case tc.refused && resp.Code != http.StatusForbidden:
				t.Fatalf("status = %d, want 403: this token may not ask the judge", resp.Code)
			case !tc.refused && resp.Code == http.StatusForbidden:
				t.Fatalf("status = %d, want the scope accepted", resp.Code)
			}
		})
	}
}

// A discobox that is not a judge answers no judging: this router is an
// ordinary sandbox's, and the ask reaches the service and is refused there
// rather than being answered by something that is not a judge.
func TestJudgeIsRefusedByADiscoboxThatIsNotOne(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	job := `{"kind":"command","purpose":"open a PR","host":"api.github.com","round":1,"command":["gh","pr","create"]}`

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/projects/project-1/sandboxes/sandbox-1/judge", strings.NewReader(job))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeJudgeRun))
	router.ServeHTTP(resp, req)

	if resp.Code == http.StatusOK {
		t.Fatalf("status = %d, body = %s; want no verdict from a discobox that is not a judge", resp.Code, resp.Body.String())
	}
}
