package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

// Whether to introduce Discobox, and whether to explain saving harness
// changes, are the project's answers, read before the window opens. A project
// that has shown either does not get it again.
func TestProjectIntroductionsReadTheProject(t *testing.T) {
	for _, welcomed := range []bool{true, false} {
		body := fmt.Sprintf(`{"id":"project-1","ownerUserId":"user-1","name":"Project",`+
			`"default":true,"welcomed":%t,"harnessChangesExplained":%t,"createdAt":"2026-01-01T00:00:00Z",`+
			`"updatedAt":"2026-01-01T00:00:00Z"}`, welcomed, !welcomed)
		server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/projects/project-1" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(server.Close)

		client, err := apiclientgen.NewClient(server.URL)
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		app := &App{}
		got, err := app.projectIntroductions(context.Background(), client, "project-1")
		if err != nil {
			t.Fatalf("read the project: %v", err)
		}
		if got.welcomed != welcomed || got.harnessChangesExplained != !welcomed {
			t.Fatalf("projectIntroductions() = %+v, want welcomed %v and explained %v", got, welcomed, !welcomed)
		}
	}
}

// A server that cannot describe the project is worth stopping on: everything
// the window does needs the project, so opening anyway only defers the failure.
func TestProjectIntroductionsFailWithTheServer(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	app := &App{}
	if _, err := app.projectIntroductions(context.Background(), client, "project-1"); err == nil {
		t.Fatal("a server that could not answer was taken as a welcome already shown")
	}
}

// Dismissing the welcome records it on the project, through the same update the
// project commands use.
func TestMarkWelcomedUpdatesTheProject(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut && r.Method != http.MethodPatch && r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
		}
		if r.URL.Path != "/projects/project-1" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"project-1","ownerUserId":"user-1","name":"Project","default":true,` +
			`"welcomed":true,"harnessChangesExplained":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(server.Close)

	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ds := &apiDataSource{app: &App{}, client: client, projectID: "project-1"}
	if err := ds.MarkWelcomed(context.Background()); err != nil {
		t.Fatalf("mark welcomed: %v", err)
	}
	if got["welcomed"] != true {
		t.Fatalf("body = %v, want welcomed set", got)
	}
	// Only that. An update carrying the project's name would rename it to
	// whatever the window happened to be holding.
	if len(got) != 1 {
		t.Fatalf("body = %v, want nothing but the welcome", got)
	}
}

// Showing the harness-changes explainer records it on the project the same
// way, and sends nothing else.
func TestMarkHarnessChangesExplainedUpdatesTheProject(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/project-1" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"project-1","ownerUserId":"user-1","name":"Project","default":true,` +
			`"welcomed":true,"harnessChangesExplained":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(server.Close)

	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ds := &apiDataSource{app: &App{}, client: client, projectID: "project-1"}
	if err := ds.MarkHarnessChangesExplained(context.Background()); err != nil {
		t.Fatalf("mark explained: %v", err)
	}
	if len(got) != 1 || got["harnessChangesExplained"] != true {
		t.Fatalf("body = %v, want harnessChangesExplained set and nothing else", got)
	}
}
