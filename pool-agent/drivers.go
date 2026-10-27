package poolagent

// The disco-vm drivers a machine pool's engine is made on, so machine.New
// finds them by the name the bootstrap gives. Each registers itself with
// disco-vm; nothing here acts on which one is configured.
import (
	// boxd is the remote driver a machine pool runs on (ADR 26-10-23-551). A
	// driver whose machines are this host's (vz, hcs) keeps its pool agent
	// beside the server instead, and disco-vm's docker driver is deferred.
	_ "github.com/discobox-ai/vm/pkg/machine/boxd"
)
