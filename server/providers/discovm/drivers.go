package discovm

// The disco-vm drivers this build links, so that machine.New finds them by the
// name a provider is configured with. Each registers itself with disco-vm;
// nothing here acts on which one is configured.
//
// They are imported one by one rather than through disco-vm's
// pkg/machine/drivers, which also links its fake driver: a fake guest is the
// running binary serving exec on loopback, which is a test's to run, never a
// server's.
import (
	// boxd's hypervisor is in the cloud, so every build can drive it.
	_ "github.com/discobox-ai/vm/pkg/machine/boxd"
)
