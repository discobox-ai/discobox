package discovm

// The disco-vm drivers this build links, each a provider type of its own name
// (ADR 26-11-11-080 §1). Each registers itself with disco-vm; nothing here acts
// on which one a provider is.
//
// They are imported one by one rather than through disco-vm's
// pkg/machine/drivers, which also links its fake driver (a test's to run,
// never a server's) and drivers whose names are built-in provider types
// already: docker, libkrun and vz are not linked, and a test fails a build
// that links one (ADR 26-11-11-080 §1, Deferred).
import (
	// archil's and boxd's hypervisors are in the cloud, so every build
	// can drive them.
	_ "github.com/discobox-ai/vm/pkg/machine/archil"
	_ "github.com/discobox-ai/vm/pkg/machine/boxd"
)
