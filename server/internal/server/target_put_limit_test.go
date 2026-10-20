package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTargetPutsAreBounded(t *testing.T) {
	read := 0
	body := readerFunc(func(p []byte) (int, error) {
		for i := range p {
			p[i] = ' '
		}
		read += len(p)
		return len(p), nil
	})
	reached := false
	handler := boundTargetPuts(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		if _, err := io.ReadAll(r.Body); err == nil {
			t.Error("an endless body was read whole")
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/projects/default/targets/github.com", body))
	if !reached || read > maxTargetPutBodyBytes+(1<<20) {
		t.Fatalf("reached = %v, read %d bytes; want the body cut off near %d", reached, read, maxTargetPutBodyBytes)
	}

	other := boundTargetPuts(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if data, err := io.ReadAll(r.Body); err != nil || len(data) != 5 {
			t.Errorf("another route's body: %q, %v", data, err)
		}
	}))
	other.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/projects/default/target-lookup", strings.NewReader("hello")))
}
