package id

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestNewReturnsLowercaseULID(t *testing.T) {
	id, err := New()
	if err != nil {
		t.Fatal(err)
	}

	if len(id) != 26 {
		t.Fatalf("expected 26-character ULID, got %q", id)
	}
	if id != strings.ToLower(id) {
		t.Fatalf("expected lowercase ULID, got %q", id)
	}
	if _, err := ulid.ParseStrict(strings.ToUpper(id)); err != nil {
		t.Fatalf("expected valid ULID %q: %v", id, err)
	}
}
