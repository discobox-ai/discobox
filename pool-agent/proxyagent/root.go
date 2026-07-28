package proxyagent

import (
	"path/filepath"
	"strings"
)

// testRoot relocates every path this package touches under a temporary
// directory. It is set only by tests: the package addresses absolute container
// paths (see the layout package), which a test process cannot write to.
//
// It is deliberately unexported and unset in production, so the public API
// carries no path-mapping parameter and no caller can accidentally relocate
// real pool state.
var testRoot string

// resolve applies testRoot. In production it is the identity.
//
// It is idempotent: a path that has already been relocated is returned
// unchanged. Paths cross back and forth across the proxy package (a certificate
// directory goes out, certificate file paths come back), so without this a
// round trip would nest the root inside itself.
func resolve(path string) string {
	if testRoot == "" || path == "" {
		return path
	}
	if path == testRoot || strings.HasPrefix(path, testRoot+string(filepath.Separator)) {
		return path
	}
	return filepath.Join(testRoot, path)
}
