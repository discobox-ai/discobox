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

func TestShortReturnsRightmostCharacters(t *testing.T) {
	if got, want := Short("0123456789abcdef"), "89abcdef"; got != want {
		t.Fatalf("Short = %q, want %q", got, want)
	}
	if got := Short("usr_default"); got != "usr_default" {
		t.Fatalf("Short preserved friendly value = %q", got)
	}
	if got := Short("short"); got != "short" {
		t.Fatalf("Short preserved short value = %q", got)
	}
	if got, want := ShortN("0123456789abcdef", 12), "456789abcdef"; got != want {
		t.Fatalf("ShortN = %q, want %q", got, want)
	}
}

func TestIsShort(t *testing.T) {
	if !IsShort("12345678") {
		t.Fatal("expected 8-character value to be short")
	}
	if !IsShort("usr_default") {
		t.Fatal("expected friendly ID to be short")
	}
	if IsShort("0123456789abcdefghijklmnop") {
		t.Fatal("expected generated-length value not to be short")
	}
	if IsShort(" ") {
		t.Fatal("blank value is not a short id")
	}
}

func TestIsFriendly(t *testing.T) {
	if !IsFriendly("usr_default") {
		t.Fatal("expected typed default ID to be friendly")
	}
	if IsFriendly("12345678") {
		t.Fatal("plain short ID is not friendly")
	}
	if IsFriendly("0123456789abcdefghijklmnop") {
		t.Fatal("generated-length value is not friendly")
	}
}
