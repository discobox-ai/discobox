package rules

import (
	"context"
	"net/http"
	"testing"
)

func TestRewriterDeterministicSpecificity(t *testing.T) {
	rewriter := NewRewriter([]HeaderRule{
		{ID: "wild-all", Pattern: "*", Set: map[string]string{"X-Test": "all"}},
		{ID: "suffix", Pattern: "api.*", Set: map[string]string{"X-Test": "suffix"}},
		{ID: "exact", Pattern: "api.example.com", Set: map[string]string{"X-Test": "exact"}},
		{ID: "wild-prefix", Pattern: "*.example.com", Set: map[string]string{"X-Test": "prefix"}},
	})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.example.com/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "api.example.com"
	match := rewriter.Apply(req, "")
	if match.RuleID != "exact" {
		t.Fatalf("RuleID = %q, want exact", match.RuleID)
	}
	if got := req.Header.Get("X-Test"); got != "exact" {
		t.Fatalf("X-Test = %q", got)
	}
}

func TestRewriterConditions(t *testing.T) {
	rewriter := NewRewriter([]HeaderRule{{
		ID:      "conditional",
		Pattern: "*",
		Conditions: []HeaderCondition{{
			Header: "X-Environment",
			Equals: "prod",
		}},
		Set: map[string]string{"Authorization": "Bearer secret"},
	}})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	if match := rewriter.Apply(req, ""); match.Matched {
		t.Fatal("unexpected match without condition")
	}
	req.Header.Set("X-Environment", "prod")
	if match := rewriter.Apply(req, ""); !match.Matched {
		t.Fatal("expected match with condition")
	}
}

func TestRewriterMethodPathAndClientConstraints(t *testing.T) {
	rewriter := NewRewriter([]HeaderRule{{
		ID:          "scoped-secret",
		Pattern:     "api.example.com",
		Methods:     []string{http.MethodPost},
		PathRegexes: []string{`^/v1/secrets/[^/]+$`},
		ClientIDs:   []string{"sandbox-1"},
		Set:         map[string]string{"Authorization": "Bearer secret"},
	}})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.example.com/v1/secrets/item", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "api.example.com"
	if match := rewriter.Apply(req, "sandbox-1"); match.Matched {
		t.Fatal("unexpected match with wrong method")
	}

	req.Method = http.MethodPost
	req.URL.Path = "/v1/other/item"
	if match := rewriter.Apply(req, "sandbox-1"); match.Matched {
		t.Fatal("unexpected match with wrong path")
	}

	req.URL.Path = "/v1/secrets/item"
	if match := rewriter.Apply(req, "sandbox-2"); match.Matched {
		t.Fatal("unexpected match with wrong client")
	}

	match := rewriter.Apply(req, "sandbox-1")
	if !match.Matched {
		t.Fatal("expected scoped match")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization = %q", got)
	}
}
