package sandboxruntime

import (
	"testing"

	"github.com/discobox-ai/discobox/layout"
)

// withTestRoot is a pool container's filesystem relocated under a temporary
// directory, for one test.
func withTestRoot(t *testing.T) layout.Root {
	t.Helper()
	return layout.ContainerAt(t.TempDir())
}
