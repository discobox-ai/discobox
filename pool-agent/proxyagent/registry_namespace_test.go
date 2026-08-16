package proxyagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestARegistryNamespaceIsMintedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), RegistryNamespaceFile)

	if err := ensureRegistryNamespace(path); err != nil {
		t.Fatalf("mint: %v", err)
	}
	first, err := ReadRegistryNamespace(path)
	if err != nil || first == "" {
		t.Fatalf("read = %q, %v", first, err)
	}

	// A second boot must find the same namespace: the images published under it
	// are found again by name.
	if err := ensureRegistryNamespace(path); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	again, err := ReadRegistryNamespace(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if again != first {
		t.Fatalf("namespace changed across boots: %q then %q", first, again)
	}
}

// The namespace is the capability, so two sandboxes must not share one.
func TestEachSandboxGetsItsOwnNamespace(t *testing.T) {
	seen := map[string]bool{}
	for range 16 {
		path := filepath.Join(t.TempDir(), RegistryNamespaceFile)
		if err := ensureRegistryNamespace(path); err != nil {
			t.Fatalf("mint: %v", err)
		}
		namespace, err := ReadRegistryNamespace(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if seen[namespace] {
			t.Fatalf("namespace %q was minted twice", namespace)
		}
		seen[namespace] = true
	}
}

// It has to be usable as a repository path component, or every push fails.
func TestTheNamespaceIsAUsableRepositoryComponent(t *testing.T) {
	path := filepath.Join(t.TempDir(), RegistryNamespaceFile)
	if err := ensureRegistryNamespace(path); err != nil {
		t.Fatalf("mint: %v", err)
	}
	namespace, err := ReadRegistryNamespace(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !registryNamespacePattern.MatchString(namespace) {
		t.Fatalf("namespace %q is not a repository path component", namespace)
	}
}

// A namespace that cannot be parsed is reported, not replaced: whatever was
// published under it would be stranded by a silent remint.
func TestAMalformedNamespaceIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), RegistryNamespaceFile)
	if err := os.WriteFile(path, []byte("Not A Path\n"), 0o644); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := ReadRegistryNamespace(path); err == nil {
		t.Fatal("a malformed namespace should be reported")
	}
	if err := ensureRegistryNamespace(path); err == nil {
		t.Fatal("ensure should refuse to remint over a malformed namespace")
	}
}
