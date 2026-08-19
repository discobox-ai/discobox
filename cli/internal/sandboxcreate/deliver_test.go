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

func TestPushDeliveredSources(t *testing.T) {
	sandboxWith := func(source apimodel.GitSource) *apimodel.Sandbox {
		return &apimodel.Sandbox{Config: apimodel.SandboxConfig{Source: apiclientgen.NewOptGitSource(source)}}
	}

	if len(pushDeliveredSources(sandboxWith(pushSourceFixture(t, nil)))) != 1 {
		t.Fatal("a push-delivered source was not recognized")
	}
	clone := pushSourceFixture(t, func(s *apimodel.GitSource) {
		s.Delivery = apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryClone)
	})
	if len(pushDeliveredSources(sandboxWith(clone))) != 0 {
		t.Fatal("a clone-delivered source must not be pushed")
	}
	// Delivery is stated. An absent value means clone, never push.
	unset := pushSourceFixture(t, func(s *apimodel.GitSource) { s.Delivery = apiclientgen.OptGitSourceDelivery{} })
	if len(pushDeliveredSources(sandboxWith(unset))) != 0 {
		t.Fatal("a source without delivery must not be pushed")
	}
	if len(pushDeliveredSources(&apimodel.Sandbox{})) != 0 {
		t.Fatal("a sandbox with no source must not be pushed")
	}
	if len(pushDeliveredSources(nil)) != 0 {
		t.Fatal("a nil sandbox must not be pushed")
	}
}

// Delivery is decided per source, so a reference the sandbox cannot reach is
// pushed even when the primary source is bound, and the primary source comes
// first either way.
func TestPushDeliveredSourcesCoversReferences(t *testing.T) {
	reference := pushSourceFixture(t, func(s *apimodel.GitSource) { s.Slug = apiclientgen.NewOptString("foo") })
	boundPrimary := pushSourceFixture(t, func(s *apimodel.GitSource) {
		s.Delivery = apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryClone)
	})
	sandbox := &apimodel.Sandbox{Config: apimodel.SandboxConfig{
		Source: apiclientgen.NewOptGitSource(boundPrimary),
		SourceCodeReferences: apiclientgen.NewOptSandboxConfigSourceCodeReferences(apiclientgen.SandboxConfigSourceCodeReferences{
			"/src/foo": reference,
		}),
	}}
	pending := pushDeliveredSources(sandbox)
	if len(pending) != 1 || pending[0].key != "/src/foo" {
		t.Fatalf("pending = %+v, want only the unreachable reference", pending)
	}

	sandbox.Config.Source = apiclientgen.NewOptGitSource(pushSourceFixture(t, nil))
	pending = pushDeliveredSources(sandbox)
	if len(pending) != 2 || pending[0].key != "" || pending[1].key != "/src/foo" {
		t.Fatalf("pending = %+v, want the primary source first and then the reference", pending)
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
	local := &LocalSources{}
	local.add("", source)
	defer local.Close()

	sandboxRepo := t.TempDir()
	runSourceTestGit(t, sandboxRepo)("init", "--bare")
	repoRoot, err := local.pushRoot("")
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
