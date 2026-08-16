package proxyagent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
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

// ensureRegistryNamespace writes the sandbox's build-registry namespace, minting
// one the first time.
//
// It is ensured rather than generated because the namespace has to outlive a
// restart: the images a sandbox published under it are found again by name, and
// a fresh token every boot would orphan them and re-push everything. This is the
// same reason the client certificate is issued once and reused.
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
