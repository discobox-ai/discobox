package sourcecache

import "testing"

// A path names a repository only on the machine it is a path on, so it is not
// a remote: two clients' /srv/git/app must not share a cache.
func TestAPathIsNotARemote(t *testing.T) {
	for _, remote := range []string{
		"",
		"/srv/git/app.git",
		"../app",
		"./app:odd",
		"file:///srv/git/app.git",
		`C:\src\app`,
		"C:/src/app",
		"https://github.com",
		"https://github.com/",
		"git@github.com:",
		"has space@host:path",
	} {
		if got := RemoteKey(remote); got != "" {
			t.Errorf("RemoteKey(%q) = %q, want none", remote, got)
		}
	}
}

func TestOnlyASandboxKeyIsForOneSandbox(t *testing.T) {
	if !ForOneSandbox(SandboxKey("sbx-1", "primary")) {
		t.Fatal("a sandbox key was not recognized")
	}
	for _, key := range []string{RemoteKey("https://github.com/sandbox/x"), DirectoryKey("abc"), ""} {
		if ForOneSandbox(key) {
			t.Errorf("%q was taken for one sandbox's cache", key)
		}
	}
	if DirectoryKey("") != "" {
		t.Fatal("no data key made a directory key")
	}
}

// A sandbox's route to its own pool is not a remote: a discobox created from
// inside one must not be keyed by, or told to fetch from, an address that
// resolves only there.
func TestAPoolsOriginsHostIsNotARemote(t *testing.T) {
	plumbing := "https://" + PoolOriginsHost + "/api/project/p/pool/q/sandboxes/sbx-1/git-origins/primary.git"
	if !OnPoolOriginsHost(plumbing) {
		t.Fatal("a pool origin route was not recognized")
	}
	if got := RemoteKey(plumbing); got != "" {
		t.Fatalf("RemoteKey of a pool origin route = %q, want none", got)
	}
	if OnPoolOriginsHost("https://github.com/acme/widget") || OnPoolOriginsHost("/srv/git/x") {
		t.Fatal("an ordinary remote was taken for a pool origin route")
	}
}
