package sandboxruntime

import (
	"path/filepath"
	"strings"
)

// testRoot relocates the state tree this package reads and writes.
//
// In production it is empty and resolve is the identity: the agent runs inside
// the pool container, where the state trees are bound at exactly the paths the
// layout package names, so it addresses them directly. A test process has no
// such mount and cannot write to an absolute container path, so tests point the
// tree at a temporary directory instead.
//
// It is unexported and set only by tests, so no production caller can relocate
// real pool state and the runtime's public surface carries no path-mapping
// parameter.
var testRoot string

// resolve applies testRoot to a container path.
//
// It is idempotent: an already-relocated path is returned unchanged, so a path
// that round-trips through an accessor twice does not nest the root inside
// itself.
func resolve(path string) string {
	if testRoot == "" || path == "" {
		return path
	}
	if path == testRoot || strings.HasPrefix(path, testRoot+string(filepath.Separator)) {
		return path
	}
	return filepath.Join(testRoot, path)
}
