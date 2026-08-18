package proxyagent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/obot-platform/discobox/layout"
)

// registryNamespaceBytes is how much randomness the namespace carries. The pool
// registry has no authentication, so the path *is* the capability — the same
// property a build's synthesized reference relies on. 16 bytes is what a token
// nobody may guess costs.
const registryNamespaceBytes = 16

// registryNamespacePrefix marks the path as a sandbox's own, so a human reading
// the registry's catalog can tell these from `discobox-build`.
const registryNamespacePrefix = "sbx-"

// registryNamespacePattern is a single repository path component, as the
// distribution spec defines one.
var registryNamespacePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// RegistryNamespacePath is where a sandbox's build-registry namespace is kept:
// its durable tree, alongside the data the sandbox itself owns.
//
// Not the staged material directory, which is where the sandbox *reads* it from
// but which archiving deletes — the material is disposable precisely because
// creation stages it again. The namespace is not disposable: it names
// repositories in the pool registry that outlive any one container, so an
// unarchive that minted a fresh one would orphan everything published under the
// old one with nothing left able to name it. The durable tree has the lifetime
// wanted: it survives archive and is removed by purge, which is when those
// repositories are removed too.
func RegistryNamespacePath(projectID, poolID, sandboxID string) string {
	return filepath.Join(layout.Sandbox(projectID, poolID, sandboxID), RegistryNamespaceFile)
}

// ensureRegistryNamespace writes the sandbox's build-registry namespace, minting
// one the first time.
//
// It is ensured rather than generated because the namespace has to outlive a
// restart and an archive: the images a sandbox published under it are found
// again by name, and a fresh token would orphan them and re-push everything.
// This is the same reason the client certificate is issued once and reused.
func ensureRegistryNamespace(path string) error {
	if existing, err := ReadRegistryNamespace(path); err == nil && existing != "" {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	raw := make([]byte, registryNamespaceBytes)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("mint registry namespace: %w", err)
	}
	namespace := registryNamespacePrefix + hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(resolve(path)), 0o755); err != nil {
		return fmt.Errorf("create registry namespace directory: %w", err)
	}
	//nolint:gosec // Readable inside the sandbox by design; see RegistryNamespaceFile.
	if err := os.WriteFile(resolve(path), []byte(namespace+"\n"), 0o644); err != nil {
		return fmt.Errorf("write registry namespace: %w", err)
	}
	return nil
}

// ReadRegistryNamespace reads a staged namespace, rejecting one that is not a
// usable repository path component. A malformed file is not silently replaced:
// the images already published under whatever it says would be stranded, and a
// namespace this process cannot parse is a fault to report rather than to paper
// over.
func ReadRegistryNamespace(path string) (string, error) {
	data, err := os.ReadFile(resolve(path))
	if err != nil {
		return "", err
	}
	namespace := strings.TrimSpace(string(data))
	if namespace == "" {
		return "", nil
	}
	if !registryNamespacePattern.MatchString(namespace) {
		return "", fmt.Errorf("registry namespace %q in %s is not a repository path component", namespace, path)
	}
	return namespace, nil
}
