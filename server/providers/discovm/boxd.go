package discovm

import (
	"strconv"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// boxd's hypervisor is in the cloud, so every build has it. Its machines are
// boxd's, not a shim's, and its pool agent runs in a Linux machine of its own
// (ADR 26-10-09-106 §1).
//
// It authenticates with disco-vm's BOXD_API_KEY in the server's environment.
// The ADR makes the key provider configuration, as the DigitalOcean token is;
// that needs disco-vm's driver to take a key rather than read one, and lands
// with the pool it is for (#127).
func init() {
	drivers["boxd"] = driverDefinition{
		machine: func() (machine.Driver, error) { return boxd.New(), nil },
		pools: func(e *engine.Engine) driver {
			return &poolMachine{
				engine:    e,
				shell:     []string{"/bin/bash", "-l"},
				logs:      journalCommand,
				logSource: "pool machine journal (boxd)",
				// The boxd pool and sandbox images are disco-vm build specs
				// that #123 adds; until then there is nothing to build.
			}
		},
	}
}

// journalCommand reads a Linux pool machine's journal for this boot: the pool
// agent's unit and everything under it, which is what an operator needs when
// the agent will not register.
func journalCommand(opts sandbox.PoolLogOptions) []string {
	args := []string{"journalctl", "--no-pager", "--boot"}
	if opts.Tail > 0 {
		args = append(args, "--lines", strconv.Itoa(opts.Tail))
	} else {
		args = append(args, "--no-tail")
	}
	if opts.Follow {
		args = append(args, "--follow")
	}
	return args
}
