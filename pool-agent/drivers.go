package poolagent

// The disco-vm drivers a machine pool's engine is made on, so machine.New
// finds them by the name the bootstrap gives: each driver reached at its URLs
// that the server links as a provider type (ADR 26-11-11-080 §1). Each
// registers itself with disco-vm; nothing here acts on which one is
// configured.
import (
	// archil and boxd are in the cloud: a pool on either runs its agent
	// in a machine the driver makes, which opens this engine.
	_ "github.com/discobox-ai/vm/pkg/machine/archil"
	_ "github.com/discobox-ai/vm/pkg/machine/boxd"
)
