package requestprogress

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProgressArrivesBeforeTheFinalJSON(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Report(r.Context(), "waiting for the pool to come online")
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"running":true}`)
	})))
	defer server.Close()
	seen := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(Observe(context.Background(), func(s string) { seen <- s }), "POST", server.URL, nil)
		res, err := server.Client().Do(req)
		if err == nil {
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != 200 || string(body) != `{"running":true}` {
				err = fmt.Errorf("final response: %d %s", res.StatusCode, body)
			}
		}
		done <- err
	}()
	select {
	case text := <-seen:
		if text != "waiting for the pool to come online" {
			t.Error(text)
		}
	case <-time.After(time.Second):
		t.Error("no live progress")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
