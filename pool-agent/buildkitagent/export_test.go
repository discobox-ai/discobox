package buildkitagent

// SetTestRoot relocates every path this package writes under dir, so tests can
// exercise the real rendering without touching the container's absolute paths.
func SetTestRoot(dir string) { testRoot = dir }
