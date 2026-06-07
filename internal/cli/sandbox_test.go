package cli

import (
	"testing"
)

func TestRuntimeStateReadsInlineJSON(t *testing.T) {
	raw, err := runtimeState(`{"ready":true}`)
	if err != nil {
		t.Fatalf("runtimeState: %v", err)
	}
	if string(raw) != `{"ready":true}` {
		t.Fatalf("raw = %q", string(raw))
	}
}

func TestRuntimeStateRejectsInvalidJSON(t *testing.T) {
	if _, err := runtimeState(`{bad`); err == nil {
		t.Fatal("runtimeState error = nil, want error")
	}
}
