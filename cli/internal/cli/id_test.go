package cli

import "testing"

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
