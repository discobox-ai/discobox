package buildkitagent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The pool registry keeps a repository per name under a fixed tree, which is
// how a sandbox's published images are removed when the sandbox is purged.
//
// There is no repository-delete in the distribution API: it deletes manifests
// by digest, one at a time, and leaves the repository behind. The pool owns
// this registry's storage outright — same host, same process tree, a path this
// package renders — so the names go by removing the directory that holds them.
// The blobs they referenced are unlinked by that and reclaimed by the
// registry's own garbage collection, which is a separate pass over shared
// content and not something one sandbox's purge should be running.

// registryRepositoriesDir is where registry:2's filesystem driver keeps one
// directory per repository name.
const registryRepositoriesDir = "docker/registry/v2/repositories"

// repositoryNamespacePattern is a single repository path component, matching
// what proxyagent mints. It is re-checked here because this value chooses a
// directory to delete recursively: a namespace carrying a separator or a `..`
// would name a tree that is not the sandbox's.
var repositoryNamespacePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// RemoveRegistryNamespace deletes every repository a sandbox published under
// its namespace. It is idempotent: a sandbox that never built has no directory,
// and that is not an error.
func RemoveRegistryNamespace(projectID, poolID, namespace string) error {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil
	}
	if !repositoryNamespacePattern.MatchString(namespace) {
		return fmt.Errorf("registry namespace %q is not a repository path component", namespace)
	}
	dir := filepath.Join(RegistryRoot(projectID, poolID), registryRepositoriesDir, namespace)
	if err := os.RemoveAll(resolve(dir)); err != nil {
		return fmt.Errorf("remove registry namespace %s: %w", namespace, err)
	}
	return nil
}
