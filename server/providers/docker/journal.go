package docker

import (
	"context"
	"fmt"
	"os/exec"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// PoolLogs reads the Docker daemon's systemd journal on this machine.
//
// This driver boots no VM, so there is no console to read: every pool's
// containers run on a daemon that is somebody else's process, and the closest
// thing to a host log is what that daemon writes to the journal. Two things
// have to hold for this process to be able to read it — the daemon has to be
// on this machine, and this machine has to keep a journal — and both are
// checked before a stream is promised, because a driver that cannot answer
// should say why rather than hand back an empty log.
//
// The pool ID is unused: pools on this backend share one daemon, so they share
// its journal too.
func (d *LocalDriver) PoolLogs(ctx context.Context, _ string, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	host := d.DaemonHost()
	if !daemonIsLocal(host) {
		return nil, fmt.Errorf("%w: the Docker daemon at %s is not this machine's, so its journal is not here either; read it on the machine running the daemon",
			sandbox.ErrPoolLogsUnsupported, host)
	}
	journalctl, err := exec.LookPath("journalctl")
	if err != nil {
		return nil, fmt.Errorf("%w: this machine runs the Docker daemon but has no journalctl to read its log from",
			sandbox.ErrPoolLogsUnsupported)
	}
	args := dockerworker.JournalCommand(opts)
	cmd := exec.CommandContext(ctx, journalctl, args[1:]...)
	return dockerworker.StreamCommand(cmd, "docker daemon journal (systemd) on "+host)
}
