package execs

import "testing"

func int64ptr(v int64) *int64 { return &v }

func TestResolveUser(t *testing.T) {
	// An explicit home wins over any lookup.
	if _, home, err := ResolveUser(&User{UID: int64ptr(0), HomeDirectory: "/explicit"}); err != nil || home != "/explicit" {
		t.Fatalf("explicit home = %q, %v; want /explicit", home, err)
	}

	// An empty user resolves to nothing (no error).
	if name, home, err := ResolveUser(nil); err != nil || name != "" || home != "" {
		t.Fatalf("empty user = %q/%q/%v; want empty", name, home, err)
	}

	// A bare UID resolves a home from the OS user database when present.
	_, rootHome, err := ResolveUser(&User{UID: int64ptr(0)})
	if err != nil {
		t.Fatalf("resolve uid 0: %v", err)
	}
	if rootHome == "" {
		t.Skip("uid 0 has no passwd entry in this environment")
	}
	if rootHome[0] != '/' {
		t.Fatalf("uid 0 home = %q, want an absolute path", rootHome)
	}
}
