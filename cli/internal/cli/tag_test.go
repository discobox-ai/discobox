package cli

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/internal/hostid"
)

// tagTestServer serves rmTestServer's listing and the meta change, applying
// each change to the tags the docs discobox holds — wip, to begin with — the
// way the sandbox does, and answering with the sandbox. It records "list" for
// the listing and "meta sandboxID change" for each change.
func tagTestServer(t *testing.T, calls *[]string) *httptest.Server {
	t.Helper()
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	t.Chdir(t.TempDir())
	listing := namedSandboxJSON(rmDocsID, "docs", "Fixing the flaky test", "present") + `,` +
		namedSandboxJSON(rmAPIID, "api", "api", "present")
	tags := map[string]string{"wip": ""}
	description := "Fixing the flaky test"
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		const collection = "/projects/project-1/sandboxes"
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == collection:
			*calls = append(*calls, "list")
			_, _ = w.Write([]byte(`{"sandboxes":[` + listing + `]}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, collection+"/") && strings.HasSuffix(r.URL.Path, "/meta"):
			sandboxID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, collection+"/"), "/meta")
			var change struct {
				Description *string           `json:"description,omitempty"`
				SetTags     map[string]string `json:"setTags,omitempty"`
				RemoveTags  []string          `json:"removeTags,omitempty"`
			}
			if err := json.NewDecoder(r.Body).Decode(&change); err != nil {
				t.Errorf("decode meta change: %v", err)
			}
			// Re-encoded, so the record spells setTags in key order whatever
			// order the client wrote the map in.
			recorded, _ := json.Marshal(change)
			*calls = append(*calls, "meta "+sandboxID+" "+string(recorded))
			for _, key := range change.RemoveTags {
				delete(tags, key)
			}
			maps.Copy(tags, change.SetTags)
			if change.Description != nil {
				description = *change.Description
			}
			meta, _ := json.Marshal(map[string]any{"description": description, "tags": tags})
			sandbox := strings.TrimSuffix(namedSandboxJSON(sandboxID, "docs", "docs", "present"), "}") + `,"meta":` + string(meta) + `}`
			_, _ = w.Write([]byte(sandbox))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// Setting tags sends them as setTags, a bare KEY as a plain label, and prints
// every tag the discobox then holds, the ones it already had included.
func TestTagSetsTags(t *testing.T) {
	var calls []string
	server := tagTestServer(t, &calls)

	out, errOut, err := runRM(t, server.URL, "tag", "Fixing the flaky test", "to-delete", "ticket=ENG-12")
	if err != nil {
		t.Fatalf("execute tag: %v\n%s", err, errOut)
	}
	if got, want := strings.Join(calls, ","), `list,meta `+rmDocsID+` {"setTags":{"ticket":"ENG-12","to-delete":""}}`; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	if got, want := out, "ticket=ENG-12\nto-delete\nwip\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

// --rm sends the keys as removeTags, and the tags left are printed. A full ID
// names the discobox outright, so no listing is fetched: inside a discobox the
// one call is what its use describes.
func TestTagRemovesTagsByFullIDWithoutListing(t *testing.T) {
	var calls []string
	server := tagTestServer(t, &calls)

	out, errOut, err := runRM(t, server.URL, "tag", rmDocsID, "--rm", "wip", "--rm", "absent")
	if err != nil {
		t.Fatalf("execute tag: %v\n%s", err, errOut)
	}
	if got, want := strings.Join(calls, ","), `meta `+rmDocsID+` {"removeTags":["wip","absent"]}`; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want no tags left", out)
	}
}

// --description replaces the description, and "" is a change that clears it
// rather than no change; -o json prints the discobox's ID, description, and
// tags.
func TestTagSetsTheDescriptionAndPrintsJSON(t *testing.T) {
	var calls []string
	server := tagTestServer(t, &calls)

	out, errOut, err := runRM(t, server.URL, "-o", "json", "tag", rmDocsID, "--description", "")
	if err != nil {
		t.Fatalf("execute tag: %v\n%s", err, errOut)
	}
	if got, want := strings.Join(calls, ","), `meta `+rmDocsID+` {"description":""}`; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	var printed struct {
		ID          string            `json:"id"`
		Description *string           `json:"description"`
		Tags        map[string]string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(out), &printed); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if printed.ID != rmDocsID || printed.Description == nil || *printed.Description != "" || !maps.Equal(printed.Tags, map[string]string{"wip": ""}) {
		t.Fatalf("stdout = %s", out)
	}
}

// What sandboxmeta refuses is refused before any call, naming the argument: an
// invalid key or value, a key both set and removed, a key set twice, and no
// change at all.
func TestTagRefusesAnInvalidChangeBeforeAnyCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"invalid key", []string{"bad key"}, `tag "bad key": tag key "bad key" cannot contain whitespace`},
		{"key with a comma", []string{"a,b"}, `tag "a,b": tag key "a,b" cannot contain ','`},
		{"value with a comma", []string{"ticket=a,b"}, `tag "ticket=a,b": the value of tag "ticket" cannot contain ','`},
		{"invalid removed key", []string{"--rm", "a=b"}, `--rm "a=b": tag key "a=b" cannot contain '='`},
		{"set and removed", []string{"wip", "--rm", "wip"}, `tag "wip" is both set and removed`},
		{"set twice", []string{"ticket=ENG-1", "ticket=ENG-2"}, `tag "ticket" is set more than once`},
		{"nothing", nil, "nothing to change"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			server := tagTestServer(t, &calls)

			_, _, err := runRM(t, server.URL, append([]string{"tag", rmDocsID}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if len(calls) != 0 {
				t.Fatalf("calls = %q, want none", calls)
			}
		})
	}
}

// A name the listing does not show is refused in the words `start` uses, and
// nothing is changed.
func TestTagRefusesAnUnknownDiscobox(t *testing.T) {
	var calls []string
	server := tagTestServer(t, &calls)

	_, _, err := runRM(t, server.URL, "tag", "no-such-box", "to-delete")
	if err == nil || !strings.Contains(err.Error(), `no discobox named "no-such-box"`) {
		t.Fatalf("error = %v, want the name refused", err)
	}
	if got, want := strings.Join(calls, ","), "list"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}
