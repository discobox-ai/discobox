package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/judge"
)

func TestJudgeSocketExposesOnlyJudging(t *testing.T) {
	calls := 0
	handler, err := NewJudgeHandler(func(context.Context, judge.Job) (judge.Verdict, error) {
		calls++
		return judge.Verdict{Allow: true, Reason: "associated", Role: judge.Role, Prompt: "evidence", PromptVersion: judge.PromptVersion}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/judge", strings.NewReader(`{"kind":"command","purpose":"open PR","host":"github.com","credential":"github","command":["gh","pr","create"]}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("judge: %d %s", response.Code, response.Body.String())
	}
	for _, path := range []string{"/health", "/sandboxes", "/sandboxes/sb/execs"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(`{}`)))
		if response.Code != http.StatusNotFound {
			t.Fatalf("private socket exposed %s: %d", path, response.Code)
		}
	}
}
