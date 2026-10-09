package discovm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// hostAgent hosts a pool as a pool agent process on this machine, under
// <root>/pools/<pool ID>.
type hostAgent struct {
	root string
}

func (h *hostAgent) poolDir(poolID string) string {
	return filepath.Join(h.root, "pools", poolID)
}

var errHostAgentNotStaged = errors.New("a local driver's host pool agent is not staged yet (#64)")

func (h *hostAgent) ensurePoolHost(context.Context, *model.Pool, func(context.Context) error) error {
	return errHostAgentNotStaged
}

func (h *hostAgent) repairPoolHost(context.Context, *model.Pool, func(context.Context) error) error {
	return errHostAgentNotStaged
}

func (h *hostAgent) removePoolHost(_ context.Context, pool *model.Pool) error {
	return os.RemoveAll(h.poolDir(pool.ID))
}

// openConsole is refused: the pool's host is the user's own machine, and a root
// shell on it is not the server's to hand out. The pool agent's log is what
// says why it will not come up.
func (h *hostAgent) openConsole(context.Context, *model.Pool, sandbox.ConsoleOptions) (sandbox.PTY, error) {
	return nil, fmt.Errorf("this pool's agent runs on the server's own machine, which is not a pool host to open a shell on; read its log instead: %w", sandbox.ErrPoolConsoleUnsupported)
}

// openLogs reads the log the supervised pool agent writes.
func (h *hostAgent) openLogs(ctx context.Context, pool *model.Pool, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	reader, err := dockerworker.TailFile(ctx, filepath.Join(h.poolDir(pool.ID), "pool-agent.log"), opts)
	if err != nil {
		return nil, err
	}
	return &sandbox.PoolLogStream{Source: "host pool agent log", ReadCloser: reader}, nil
}
