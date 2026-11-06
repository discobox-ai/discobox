package relay

import (
	"errors"
	"testing"
)

// The relay must either be a real Linux binary or fail loudly. A silently
// truncated or placeholder artifact would be installed into a guest and fail
// there, far from the cause.
func TestBinaryProducesTheEmbeddedProgramOrFailsClearly(t *testing.T) {
	binary, err := Binary()
	if !Available() || errors.Is(err, ErrNotBuilt) {
		if err == nil {
			t.Fatal("Binary succeeded even though no relay is embedded")
		}
		t.Skipf("relay artifact not built into this binary (run `task build:cp-relay`): %v", err)
	}
	if err != nil {
		t.Fatalf("Binary: %v", err)
	}

	if len(binary) < minimumSize {
		t.Fatalf("relay is %d bytes, want a real binary", len(binary))
	}

	// An ELF magic number confirms this is the cross-compiled Linux binary and
	// not, say, a host executable or a text placeholder.
	if magic := string(binary[:4]); magic != "\x7fELF" {
		t.Fatalf("relay magic = %q, want an ELF binary", magic)
	}
}

// Every pool start decompresses the relay again, so two calls have to agree:
// the host compares the SHA-256 the guest reports for what it received against
// the digest of these bytes.
func TestBinaryIsStable(t *testing.T) {
	if !Available() {
		t.Skip("relay artifact not built into this binary")
	}
	first, err := Binary()
	if err != nil {
		t.Skipf("relay not built: %v", err)
	}
	second, err := Binary()
	if err != nil {
		t.Fatalf("second Binary: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("relay length changed between calls: %d then %d", len(first), len(second))
	}
	if string(first) != string(second) {
		t.Fatal("relay contents changed between calls")
	}
}

func TestDigestIsStable(t *testing.T) {
	first, second := Digest(), Digest()
	if first == "" {
		t.Fatal("Digest returned an empty identifier")
	}
	if first != second {
		t.Fatalf("Digest is not stable: %q then %q", first, second)
	}
}
