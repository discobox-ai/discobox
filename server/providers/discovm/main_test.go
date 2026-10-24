package discovm

import (
	"fmt"
	"os"
	"testing"

	"github.com/discobox-ai/vm/pkg/engine"
	_ "github.com/discobox-ai/vm/pkg/machine/fake" // linked by tests alone: its guest agent is the running binary

	"github.com/discobox-ai/discobox/pool-agent/vmtest"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// fakePoolMachine hosts a pool in a machine of disco-vm's fake driver, booted
// from one of its bases, the way a remote driver's pool is hosted, so the
// engine, its shim, and the guest operations run here as they do against a
// real machine. The fake driver is local, so newPoolHost would give it a host
// agent; tests that drive a pool machine build this one. Its guests are host
// processes with no journal, so its log is a command that honors the tail it
// is asked for.
func fakePoolMachine(e *engine.Engine, base string) *poolMachine {
	return &poolMachine{
		engine: e,
		base:   base,
		shell:  []string{"/bin/sh"},
		logs: func(opts sandbox.PoolLogOptions) []string {
			return []string{"sh", "-c", fmt.Sprintf("seq 1 5 | tail -n %d", opts.Tail)}
		},
		logSource: "fake machine log",
	}
}

// TestMain lets this binary be the two processes the engine starts: a shim, as
// the server is, and a fake guest's agent, as `disco-vm guest` is.
func TestMain(m *testing.M) {
	vmtest.RunFakeGuestIfInvoked()
	RunShimIfInvoked()
	os.Exit(m.Run())
}
