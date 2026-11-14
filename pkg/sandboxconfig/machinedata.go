package sandboxconfig

// MachineDataWiredFile is where a machine's sandbox agent records, on the
// machine's own data volume, that it keeps the sandbox's data there this
// start: its declared volumes and its sources, wired from the volume. It holds
// one line, the sandbox's ID and the token of this start (MachineWiredFile),
// and whether the working root is durable too (MachineDataWiredLine).
// The pool gives a machine a new root, which keeps the volume and nothing
// else, only when it read this naming the start the machine is on: whether
// the data is on the volume is the agent's doing, and the agent is the
// sandbox's pin, which any rebuild may move to one that predates the wiring.
// A Linux guest's path.
const MachineDataWiredFile = "/.discobox/data/.discobox-wired"

// MachineWorkingRootWired is the line's third field when the start also kept
// the sandbox's working root durable: on the volume, or under a source or a
// declared volume that is. Only a line with it says all of the sandbox's data
// is on the volume: an agent from before it left a working root with no
// source on the machine's root, which a new root replaces (ADR 26-11-14-302
// §4).
const MachineWorkingRootWired = "working-root"

// MachineDataWiredLine is MachineDataWiredFile's line for sandboxID and the
// token of a start, without its newline; workingRoot adds
// MachineWorkingRootWired. The pool keeps the machine's data on its volume
// only when the line it reads is exactly this with workingRoot set.
func MachineDataWiredLine(sandboxID, token string, workingRoot bool) string {
	line := sandboxID + " " + token
	if workingRoot {
		line += " " + MachineWorkingRootWired
	}
	return line
}

// MachineWiredFile is where the agent keeps the token of this start, a
// random one it makes once its machine wiring has run, mounted or not: from
// its data and cache roots, whatever backs them, its driver's volumes or
// directories on its own disk -- or, at an agent's start on roots on the
// disk, nothing (only what keeps it from running twice in one start). /run
// is a new tmpfs at every start, of a container's guest as of a VM's, so a
// start whose wiring has not run has none.
// Only MachineDataWiredFile, written where the data root is a data volume,
// says the data is kept on a volume; roots on the disk write no line there,
// and a line on the volume naming another token names a start that is over.
// A Linux guest's path.
const MachineWiredFile = "/run/discobox/machine-wired"
