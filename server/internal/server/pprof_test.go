package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The API router, which every API transport serves, has no profiling routes.
func TestRouterDoesNotServePprof(t *testing.T) {
	router := newStubRouterForTest()
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/profile?seconds=1", "/debug/pprof/heap"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("API router answered GET %s with 200; profiling must stay off the API", path)
		}
	}
}
