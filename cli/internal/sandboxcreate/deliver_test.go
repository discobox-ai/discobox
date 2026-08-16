package sandboxcreate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func pushSourceFixture(t *testing.T, mutate func(*apimodel.GitSource)) apimodel.GitSource {
	t.Helper()
	source := apimodel.GitSource{
		Kind:     apiclientgen.GitSourceKindGit,
		Delivery: apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryPush),
		Slug:     apiclientgen.NewOptString("primary"),
		Checkout: apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
			Commit:  apiclientgen.NewOptString(testCommit),
			RefName: apiclientgen.NewOptString("main"),
			RefType: apiclientgen.NewOptString("branch"),
		}),
	}
	if mutate != nil {
		mutate(&source)
	}
	return source
}

func TestSourceNeedsPush(t *testing.T) {
	sandboxWith := func(source apimodel.GitSource) *apimodel.Sandbox {
		return &apimodel.Sandbox{Config: apimodel.SandboxConfig{Source: apiclientgen.NewOptGitSource(source)}}
	}

	if !SourceNeedsPush(sandboxWith(pushSourceFixture(t, nil))) {
		t.Fatal("a push-delivered source was not recognized")
	}
	clone := pushSourceFixture(t, func(s *apimodel.GitSource) {
		s.Delivery = apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryClone)
	})
	if SourceNeedsPush(sandboxWith(clone)) {
		t.Fatal("a clone-delivered source must not be pushed")
	}
	// Delivery is stated. An absent value means clone, never push.
	unset := pushSourceFixture(t, func(s *apimodel.GitSource) { s.Delivery = apiclientgen.OptGitSourceDelivery{} })
	if SourceNeedsPush(sandboxWith(unset)) {
		t.Fatal("a source without delivery must not be pushed")
	}
	if SourceNeedsPush(&apimodel.Sandbox{}) {
		t.Fatal("a sandbox with no source must not be pushed")
	}
	if SourceNeedsPush(nil) {
		t.Fatal("a nil sandbox must not be pushed")
	}
}

func TestPushRefs(t *testing.T) {
	t.Run("clean branch source pushes the commit to its branch", func(t *testing.T) {
		commit, branch, snapshot, err := pushRefs(pushSourceFixture(t, nil))
		if err != nil {
			t.Fatalf("pushRefs: %v", err)
		}
		if commit != testCommit || branch != "main" || snapshot != "" {
			t.Fatalf("commit=%q branch=%q snapshot=%q, want the source's commit on main with no snapshot", commit, branch, snapshot)
		}
	})

	t.Run("dirty source also pushes its snapshot ref", func(t *testing.T) {
		source := pushSourceFixture(t, func(s *apimodel.GitSource) {
			s.Workspace = apiclientgen.NewOptGitSourceWorkspace(apimodel.GitSourceWorkspace{
				Mode:        apiclientgen.NewOptGitSourceWorkspaceMode(apiclientgen.GitSourceWorkspaceModeDirty),
				SnapshotRef: apiclientgen.NewOptString("refs/discobox/run/snap_1"),
				BaseCommit:  apiclientgen.NewOptString(testCommit),
			})
		})
		_, _, snapshot, err := pushRefs(source)
		if err != nil {
			t.Fatalf("pushRefs: %v", err)
		}
		// Without this the sandbox comes up clean and the edits are lost.
		if snapshot != "refs/discobox/run/snap_1" {
			t.Fatalf("snapshot = %q, want the workspace snapshot ref", snapshot)
		}
	})

	t.Run("a bare commit has no branch to push to", func(t *testing.T) {
		source := pushSourceFixture(t, func(s *apimodel.GitSource) {
			s.Checkout = apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
				Commit:  apiclientgen.NewOptString(testCommit),
				RefType: apiclientgen.NewOptString("commit"),
			})
		})
		_, branch, _, err := pushRefs(source)
		if err != nil {
			t.Fatalf("pushRefs: %v", err)
		}
		if branch != "" {
			t.Fatalf("branch = %q, want none for a bare commit", branch)
		}
	})

	t.Run("a source with no commit cannot be pushed", func(t *testing.T) {
		source := pushSourceFixture(t, func(s *apimodel.GitSource) { s.Checkout = apiclientgen.OptGitSourceCheckout{} })
		if _, _, _, err := pushRefs(source); err == nil {
			t.Fatal("source with no commit: got nil error, want failure")
		}
	})

	t.Run("a dirty source with no snapshot ref cannot be pushed", func(t *testing.T) {
		source := pushSourceFixture(t, func(s *apimodel.GitSource) {
			s.Workspace = apiclientgen.NewOptGitSourceWorkspace(apimodel.GitSourceWorkspace{
				Mode: apiclientgen.NewOptGitSourceWorkspaceMode(apiclientgen.GitSourceWorkspaceModeDirty),
			})
		})
		if _, _, _, err := pushRefs(source); err == nil {
			t.Fatal("dirty source with no snapshot ref: got nil error, want failure")
		}
	})
}

func TestSandboxGitRepositoryURL(t *testing.T) {
	source := pushSourceFixture(t, nil)

	got, err := SandboxGitRepositoryURL("https://disco.example.com/", "proj_1", "sbx_1", source)
	if err != nil {
		t.Fatalf("SandboxGitRepositoryURL: %v", err)
	}
	want := "https://disco.example.com/projects/proj_1/sandboxes/sbx_1/git-repositories/primary.git"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}

	// A local server binds the source instead of asking for a push, so a socket
	// endpoint here means the client and server disagree about reachability.
	if _, err := SandboxGitRepositoryURL("unix:///run/discobox.sock", "proj_1", "sbx_1", source); err == nil {
		t.Fatal("pushing to a unix endpoint: got nil error, want failure")
	}

	noSlug := pushSourceFixture(t, func(s *apimodel.GitSource) { s.Slug = apiclientgen.OptString{} })
	if _, err := SandboxGitRepositoryURL("https://disco.example.com", "proj_1", "sbx_1", noSlug); err == nil {
		t.Fatal("source with no slug: got nil error, want failure")
	}
}

// The token must ride on the request header, not in the URL, which would land
// it in the repository's remote config and in process listings.
func TestGitPushAuthArgs(t *testing.T) {
	args := GitAuthArgs("secret-token", []string{"push", "https://disco.example.com/x.git"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c http.extraHeader=Authorization: Bearer secret-token") {
		t.Fatalf("args = %v, want the token as an extraHeader config override", args)
	}
	if strings.Contains(args[len(args)-1], "secret-token") {
		t.Fatalf("token leaked into the push URL: %v", args)
	}

	plain := GitAuthArgs("  ", []string{"push", "url"})
	if len(plain) != 2 {
		t.Fatalf("args = %v, want no auth override without a token", plain)
	}
}

// A throwaway repository has to be able to deliver, since it holds the only
// copy of what the sandbox was configured against: the push carries the base
// commit and the snapshot, and the sandbox can reconstruct the directory from
// the two.
func TestPushSourceDeliversADirectoryWithNoRepository(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	source, err := resolveRunSource(ctx, dir, runSourceOptions{IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatalf("resolveRunSource: %v", err)
	}
	local := source.localSource()
	defer local.Close()

	sandboxRepo := t.TempDir()
	runSourceTestGit(t, sandboxRepo)("init", "--bare")
	repoRoot, err := local.pushRoot()
	if err != nil {
		t.Fatalf("pushRoot: %v", err)
	}
	if err := pushSource(ctx, repoRoot, sandboxRepo, "", source.Checkout.Commit, source.Checkout.RefName, source.Workspace.SnapshotRef); err != nil {
		t.Fatalf("pushSource: %v", err)
	}

	git := runSourceTestGit(t, sandboxRepo)
	if head := strings.TrimSpace(git("rev-parse", "refs/heads/"+source.Checkout.RefName)); head != source.Checkout.Commit {
		t.Fatalf("branch %s = %s, want the base commit %s", source.Checkout.RefName, head, source.Checkout.Commit)
	}
	restored := strings.Fields(git("diff", "--name-only", source.Checkout.Commit, source.Workspace.SnapshotRef))
	if strings.Join(restored, ",") != "a.txt" {
		t.Fatalf("delivered snapshot = %v, want the directory's files", restored)
	}
}
