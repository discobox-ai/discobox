package sandboxruntime

import (
	"os"
	"testing"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pool-agent/vmtest"
)

// withTestRoot is a pool container's filesystem relocated under a temporary
// directory, for one test.
func withTestRoot(t *testing.T) layout.Root {
	t.Helper()
	return layout.ContainerAt(t.TempDir())
}

// TestMain lets this binary be the processes disco-vm's fake driver starts
// for a machine: its guest's agent and its shim.
func TestMain(m *testing.M) {
	vmtest.RunFakeGuestIfInvoked()
	os.Exit(m.Run())
}
