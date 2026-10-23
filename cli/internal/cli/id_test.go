package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

func TestResolveShortIDMatchesRandomPart(t *testing.T) {
	ids := []string{"sbx_dfzx0123456789ab", "sbx_qqqq0123456789cd"}

	got, err := resolveShortID("dfzx", "sandbox ID", ids)
	if err != nil {
		t.Fatalf("resolveShortID: %v", err)
	}
	if got != "sbx_dfzx0123456789ab" {
		t.Fatalf("resolveShortID = %q, want %q", got, "sbx_dfzx0123456789ab")
	}
}

func TestResolveShortIDErrors(t *testing.T) {
	ids := []string{"sbx_dfzx0123456789ab", "sbx_dfzx0123456789cd"}

	if _, err := resolveShortID("dfzx", "sandbox ID", ids); err == nil {
		t.Fatal("expected ambiguous short ID error")
	}
	if _, err := resolveShortID("zzzz", "sandbox ID", ids); err == nil {
		t.Fatal("expected no-match error")
	}
	if _, err := resolveShortID("  ", "sandbox ID", ids); err == nil {
		t.Fatal("expected required error for blank ID")
	}
}

// A provider instance answers to its name, as a pool does; one name two
// instances share is refused with the IDs to choose between.
func TestResolveProviderIDTakesAName(t *testing.T) {
	const do, other = "prov_dfzx0123456789ab", "prov_qqqq0123456789cd"
	listing := `{"providers":[` + providerJSON(do, "do") + `,` + providerJSON(other, "lab") + `,` + providerJSON("prov_wwww0123456789ef", "lab") + `]}`
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/projects/project-1/providers" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listing))
	}))
	t.Cleanup(server.Close)
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{}
	ctx := context.Background()

	if got, err := a.resolveProviderID(ctx, client, "project-1", "do"); err != nil || got != do {
		t.Fatalf("resolve by name = %q, %v, want %q", got, err, do)
	}
	if got, err := a.resolveProviderID(ctx, client, "project-1", "qqqq"); err != nil || got != other {
		t.Fatalf("resolve by short ID = %q, %v, want %q", got, err, other)
	}
	if _, err := a.resolveProviderID(ctx, client, "project-1", "lab"); err == nil || !strings.Contains(err.Error(), "more than one provider") {
		t.Fatalf("resolve shared name: err = %v, want ambiguous", err)
	}
}

func providerJSON(id, name string) string {
	return `{"id":"` + id + `","name":"` + name + `","projectId":"project-1","type":"docker","config":{},"disabled":false,` +
		`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
}
