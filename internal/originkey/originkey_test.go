package originkey

import "testing"

func TestOfIsStableAndDistinguishesInputs(t *testing.T) {
	base := Of("host_aaaaaaaaaaaaaaaa", "/src/alpha")
	if base == "" {
		t.Fatal("key is empty for a complete origin")
	}
	if again := Of("host_aaaaaaaaaaaaaaaa", "/src/alpha"); again != base {
		t.Fatalf("key is not stable: %q then %q", base, again)
	}
	// The same directory on another machine is a different origin.
	if other := Of("host_bbbbbbbbbbbbbbbb", "/src/alpha"); other == base {
		t.Fatal("different hosts produced the same key")
	}
	// The same machine in another directory is a different origin.
	if other := Of("host_aaaaaaaaaaaaaaaa", "/src/beta"); other == base {
		t.Fatal("different project paths produced the same key")
	}
}

// Without a separator, ("ab", "c") and ("a", "bc") would concatenate to the
// same bytes and silently merge two clients' listings.
func TestOfDoesNotCollideAcrossFieldBoundary(t *testing.T) {
	if Of("host_ab", "c") == Of("host_a", "bc") {
		t.Fatal("keys collided across the host/path boundary")
	}
}

// An incomplete origin cannot identify a project directory, and an empty key
// means "no filter" to the store — it must never look like a real one.
func TestOfRequiresBothFields(t *testing.T) {
	for _, tc := range []struct{ hostID, projectPath string }{
		{"", "/src/alpha"},
		{"host_aaaaaaaaaaaaaaaa", ""},
		{"", ""},
		{"   ", "/src/alpha"},
	} {
		if got := Of(tc.hostID, tc.projectPath); got != "" {
			t.Fatalf("Of(%q, %q) = %q, want empty", tc.hostID, tc.projectPath, got)
		}
	}
}
