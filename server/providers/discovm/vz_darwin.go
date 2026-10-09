package discovm

import (
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/vz"
)

// vz is Virtualization.framework, so only a macOS build has it. Its pool agent
// is a native process on this Mac, beside the server (ADR 26-10-09-106 §1, ADR
// 0144 §1), and only its sandboxes are machines.
func init() {
	drivers["vz"] = driverDefinition{
		machine: func() (machine.Driver, error) { return &vz.Driver{}, nil },
		pools:   func(e *engine.Engine) driver { return &hostAgent{root: e.Root} },
	}
}
