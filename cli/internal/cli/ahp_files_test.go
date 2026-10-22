package cli

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/ahphost"
)

// The AHP host's files go through the files API, and each refusal is the fs
// error the host turns into the protocol's — a 409 only when the path is
// there to be in the way, since a 409 is also a refusal on the way.
func TestAHPFilesRefuseAsTheProtocolSays(t *testing.T) {
	var (
		status   int
		body     string
		statSays = http.StatusNotFound
	)
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/files/stat") {
			w.WriteHeader(statSays)
			if statSays == http.StatusOK {
				_, _ = io.WriteString(w, `{"path":"/w/f","name":"f","type":"file","size":1,"mode":420,"modifiedAt":"2026-10-22T00:00:00Z","version":"v1"}`)
				return
			}
			_, _ = io.WriteString(w, `{"error":"not found"}`)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	client, err := app.apiClient()
	if err != nil {
		t.Fatal(err)
	}
	b := ahpBoxes{ds: &apiDataSource{app: app, client: client, projectID: "project-1"}}
	ctx := t.Context()

	for _, tc := range []struct {
		name     string
		status   int
		statSays int
		do       func() error
		want     error
	}{
		{"a missing file", http.StatusNotFound, http.StatusNotFound, func() error { _, err := b.ReadFile(ctx, "sbx_1", "/w/f"); return err }, fs.ErrNotExist},
		{"a file the user may not read", http.StatusForbidden, http.StatusOK, func() error { _, err := b.ReadFile(ctx, "sbx_1", "/w/f"); return err }, fs.ErrPermission},
		{"create-only over a file", http.StatusPreconditionFailed, http.StatusOK, func() error {
			return b.WriteFile(ctx, "sbx_1", "/w/f", ahphost.FileWrite{Data: []byte("x"), CreateOnly: true})
		}, fs.ErrExist},
		{"a stale version", http.StatusPreconditionFailed, http.StatusOK, func() error {
			return b.WriteFile(ctx, "sbx_1", "/w/f", ahphost.FileWrite{Data: []byte("x"), IfVersion: "v0"})
		}, ahphost.ErrChanged},
		{"a copy onto a file there", http.StatusConflict, http.StatusOK, func() error { return b.CopyFile(ctx, "sbx_1", "/w/a", "/w/f", false) }, fs.ErrExist},
		{"a move into itself", http.StatusBadRequest, http.StatusOK, func() error { return b.MoveFile(ctx, "sbx_1", "/w", "/w/f", true) }, fs.ErrInvalid},
	} {
		status, statSays, body = tc.status, tc.statSays, `{"error":"refused"}`
		if err := tc.do(); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}

	// A 409 for a destination that is not there is a refusal on the way, and
	// says so rather than claiming a file in the way.
	status, statSays, body = http.StatusConflict, http.StatusNotFound, `{"error":"sandbox pool is not reachable"}`
	err = b.CopyFile(ctx, "sbx_1", "/w/a", "/w/b", false)
	if err == nil || errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("a 409 on the way = %v, want the refusal as it is", err)
	}
}
