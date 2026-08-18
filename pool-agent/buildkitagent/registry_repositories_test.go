package buildkitagent

import (
	"os"
	"path/filepath"
	"testing"
)

// stageRepository puts a repository where registry:2's filesystem driver keeps
// one, and returns the directory its namespace lives in.
func stageRepository(t *testing.T, projectID, poolID, name string) string {
	t.Helper()
	dir := filepath.Join(resolve(RegistryRoot(projectID, poolID)), registryRepositoriesDir, name)
	if err := os.MkdirAll(filepath.Join(dir, "_manifests"), 0o755); err != nil {
		t.Fatalf("stage repository %s: %v", name, err)
	}
	return dir
}

// A purge takes the sandbox's published images with it. Nothing else can name
// them: the namespace is unguessable and its only record went with the sandbox.
func TestRemoveRegistryNamespaceTakesEveryRepositoryUnderIt(t *testing.T) {
	SetTestRoot(t.TempDir())
	mine := stageRepository(t, "project-1", "pool-1", "sbx-abc123/discobox-sandbox-agent")
	alsoMine := stageRepository(t, "project-1", "pool-1", "sbx-abc123/some-base")
	neighbor := stageRepository(t, "project-1", "pool-1", "sbx-def456/discobox-sandbox-agent")
	shared := stageRepository(t, "project-1", "pool-1", "discobox-build/abc")

	if err := RemoveRegistryNamespace("project-1", "pool-1", "sbx-abc123"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	for _, gone := range []string{mine, alsoMine} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s survived the purge", gone)
		}
	}
	for _, kept := range []string{neighbor, shared} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s should not have been touched: %v", kept, err)
		}
	}
}

// A sandbox that never built has nothing to remove, and a purge must not fail
// on it.
func TestRemoveRegistryNamespaceIsIdempotent(t *testing.T) {
	SetTestRoot(t.TempDir())
	if err := RemoveRegistryNamespace("project-1", "pool-1", "sbx-neverbuilt"); err != nil {
		t.Fatalf("removing a namespace that was never used: %v", err)
	}
	if err := RemoveRegistryNamespace("project-1", "pool-1", ""); err != nil {
		t.Fatalf("removing an empty namespace: %v", err)
	}
}

// The namespace chooses a directory to delete recursively, so anything that is
// not a single path component is refused rather than resolved.
func TestRemoveRegistryNamespaceRefusesAPathThatIsNotOne(t *testing.T) {
	SetTestRoot(t.TempDir())
	for _, namespace := range []string{"../../etc", "sbx-abc/..", "sbx abc", "sbx/abc", "/abs"} {
		if err := RemoveRegistryNamespace("project-1", "pool-1", namespace); err == nil {
			t.Errorf("namespace %q should be refused", namespace)
		}
	}
}
