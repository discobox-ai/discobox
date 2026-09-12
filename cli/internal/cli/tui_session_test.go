package cli

import (
	"testing"

	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/discobox/internal/originkey"
)

// A window opened on a repository URL carries that URL as what it cuts from,
// ref and all: the ref says which commit to cut, and nothing but `-C` carries
// it — the create the window builds is the only thing that would.
//
// Where its discoboxes are filed is a separate answer and a ref-less one: every
// ref of one repository is one place (ADR 0111 §1). The directory the window
// happens to run in is neither, and only holds its prompt draft.
func TestSessionOnARepositoryURLKeepsTheRefAndFilesWithoutIt(t *testing.T) {
	const host = "host_0123456789abcdef"
	const url = "https://github.com/acme/foo"
	t.Setenv(hostid.EnvVar, host)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := newRunSourceTestRepo(t)
	t.Chdir(repo)

	ds := &apiDataSource{app: &App{source: url + "@v2"}, projectID: "project-1"}
	session, err := ds.Session(t.Context())
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	if session.Remote != url+"@v2" {
		t.Fatalf("remote = %q, want the URL as -C named it, ref and all", session.Remote)
	}
	if want := originkey.Of(host, url); session.OriginKey != want {
		t.Fatalf("origin key = %q, want the repository's own %q, which carries no ref", session.OriginKey, want)
	}
	if want := originkey.Host(host); session.HostKey != want {
		t.Fatalf("host key = %q, want this machine's own %q", session.HostKey, want)
	}
	// A URL has no directory on this machine, so the window's own is where it
	// is standing — its draft's home, not where anything is filed.
	if session.Directory != repo {
		t.Fatalf("directory = %q, want the working directory's repository root %q", session.Directory, repo)
	}
}
