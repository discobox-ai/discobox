package id

import (
	"strings"
	"testing"
)

func TestNewReturnsPrefixedRandomID(t *testing.T) {
	id, err := New(PrefixSandbox)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(id, PrefixSandbox+"_") {
		t.Fatalf("expected %q prefix, got %q", PrefixSandbox+"_", id)
	}
	random := strings.TrimPrefix(id, PrefixSandbox+"_")
	if len(random) != RandomLength {
		t.Fatalf("expected %d random characters, got %q", RandomLength, id)
	}
	for i := 0; i < len(random); i++ {
		if strings.IndexByte(alphabet, random[i]) < 0 {
			t.Fatalf("unexpected character %q in %q", random[i], id)
		}
	}
	if other := NewString(PrefixSandbox); other == id {
		t.Fatalf("expected unique IDs, got %q twice", id)
	}
}

func TestIsGenerated(t *testing.T) {
	if !IsGenerated(NewString(PrefixSecret)) {
		t.Fatal("expected generated ID to be recognized")
	}
	if !IsGenerated(NewString(PrefixExec)) {
		t.Fatal("expected generated exec ID to be recognized")
	}
	if IsGenerated("user_default") {
		t.Fatal("well-known ID is not generated")
	}
	if IsGenerated("sbx_abc") {
		t.Fatal("partial ID is not generated")
	}
	if IsGenerated("_0123456789abcdef") {
		t.Fatal("empty prefix is not generated")
	}
	if IsGenerated("sbx_ilou456789abcdef") {
		t.Fatal("excluded alphabet characters are not generated")
	}
	if IsGenerated("") {
		t.Fatal("blank value is not generated")
	}
}

func TestRandomPart(t *testing.T) {
	if got, want := RandomPart("sbx_0123456789abcdef"), "0123456789abcdef"; got != want {
		t.Fatalf("RandomPart = %q, want %q", got, want)
	}
	if got, want := RandomPart("noprefix"), "noprefix"; got != want {
		t.Fatalf("RandomPart = %q, want %q", got, want)
	}
}
