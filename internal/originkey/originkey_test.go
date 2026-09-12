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

// An incomplete pair cannot identify a place on a host, and an empty key means
// "no filter" to the store — it must never look like a real one.
func TestOfRequiresBothFields(t *testing.T) {
	for _, tc := range []struct{ hostID, root string }{
		{"", "/src/alpha"},
		{"host_aaaaaaaaaaaaaaaa", ""},
		{"", ""},
		{"   ", "/src/alpha"},
	} {
		if got := Of(tc.hostID, tc.root); got != "" {
			t.Fatalf("Of(%q, %q) = %q, want empty", tc.hostID, tc.root, got)
		}
	}
}

// A sandbox with a source is filed under its place, and one without under its
// host alone — which is never the key of any place on that host.
func TestForFilesASourcelessSandboxUnderItsHost(t *testing.T) {
	const host = "host_aaaaaaaaaaaaaaaa"
	if got, want := For(host, "/src/alpha"), Of(host, "/src/alpha"); got != want {
		t.Fatalf("For with a source = %q, want the place's key %q", got, want)
	}
	if got, want := For(host, "  "), Host(host); got != want || got == "" {
		t.Fatalf("For with no source = %q, want the host's key %q", got, want)
	}
	for _, place := range []string{"/", "/src/alpha", "https://github.com/acme/api"} {
		if Host(host) == Of(host, place) {
			t.Fatalf("the host's key equals the key of its place %q", place)
		}
	}
	if Host(host) == Host("host_bbbbbbbbbbbbbbbb") {
		t.Fatal("two hosts share a key")
	}
}

// With no host there is no machine to file a sandbox under, and an empty key
// means "no filter" to the store.
func TestForRequiresAHost(t *testing.T) {
	for _, root := range []string{"", "/src/alpha"} {
		if got := For(" ", root); got != "" {
			t.Fatalf("For(no host, %q) = %q, want empty", root, got)
		}
	}
}
