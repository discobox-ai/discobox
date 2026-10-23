package cli

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

// The inline source question offers what the create card offers: what the
// project's discoboxes were cut from, newest first, each once, spelled the way
// SOURCE takes it.
func TestProjectSourcesAreWhatTheProjectWasCutFrom(t *testing.T) {
	box := func(id, created, source string) string {
		return `{"id":"` + id + `","projectId":"project-1","createdByUserId":"user-1","displayName":"` + id + `",` +
			`"config":{"name":"` + id + `","image":""` + source + `},` +
			`"runtime":{"state":"ready","desiredState":"present","displayState":"running","generation":1,"observedGeneration":1},` +
			`"createdAt":"` + created + `","updatedAt":"` + created + `"}`
	}
	local := func(dir string) string {
		return `,"source":{"kind":"git","slug":"primary","delivery":"push","localDirectory":"` + dir + `"}`
	}
	listing := box("sbx_old", "2026-01-01T00:00:00Z", local("/src/obot")) + `,` +
		box("sbx_new", "2026-01-03T00:00:00Z", `,"source":{"kind":"git","slug":"primary","delivery":"clone","url":"https://github.com/acme/foo"}`) + `,` +
		box("sbx_again", "2026-01-02T00:00:00Z", local("/src/obot")) + `,` +
		box("sbx_none", "2026-01-04T00:00:00Z", "")
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/project-1/sandboxes" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"sandboxes":[` + listing + `]}`))
	}))
	t.Cleanup(server.Close)
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	sources, err := projectSources(t.Context(), client, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []runSource{{value: "https://github.com/acme/foo", remote: true}, {value: "/src/obot"}}
	if !slices.Equal(sources, want) {
		t.Fatalf("sources = %+v, want %+v", sources, want)
	}
}

// A source is titled by what tells it apart at a glance, its SOURCE value riding
// in the column beside it.
func TestSourceTitles(t *testing.T) {
	for _, tc := range []struct {
		source runSource
		want   string
	}{
		{runSource{value: "/src/obot"}, "obot"},
		{runSource{value: "/src/obot/"}, "obot"},
		{runSource{value: "https://github.com/acme/foo.git", remote: true}, "acme/foo"},
		{runSource{value: "https://github.com/acme/foo", remote: true}, "acme/foo"},
	} {
		if got := sourceTitle(tc.source); got != tc.want {
			t.Errorf("sourceTitle(%q) = %q, want %q", tc.source.value, got, tc.want)
		}
	}
}
