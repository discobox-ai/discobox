package poolruntime

import (
	"context"
	"fmt"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// PoolMaker is what a runtime provider makes a pool's box with (ADR
// 26-10-24-953 §§2, 4), as a pool makes a sandbox's: the pool's bootstrap, what
// the box is placed with, and the enrollment of the key the box makes. The
// provider supplies the box and the channel into it; nothing here is minted,
// so a drift check that finds a healthy pool may hold one and spend nothing.
type PoolMaker interface {
	// Bootstrap is the pool's bootstrap: which pool it is and which control
	// plane it trusts. It holds no credential. The provider fills in what is
	// its own: the control plane URL as the box reaches it, the agent's
	// listener.
	Bootstrap(ctx context.Context) (poolagent.Bootstrap, error)
	// Box is the agent and the pool's bundles to place, the agent's layer for
	// the box's platform among them.
	Box(ctx context.Context, platform imagecache.Platform) (sandbox.PoolBox, error)
	// Enroll records publicKey, which the box made (`discobox-provision
	// identity`), as the pool's identity: what the box fetches its bundles
	// with, and its agent signs with, from then on.
	Enroll(ctx context.Context, publicKey string) error
}

// poolMaker is the PoolMaker of one pool, over the control plane.
type poolMaker struct {
	manager PoolManager
	project *model.Project
	pool    *model.Pool
}

func newPoolMaker(manager PoolManager, project *model.Project, pool *model.Pool) (*poolMaker, error) {
	switch {
	case manager == nil:
		return nil, fmt.Errorf("pool manager is required")
	case project == nil:
		return nil, fmt.Errorf("project is required")
	case pool == nil:
		return nil, fmt.Errorf("pool is required")
	}
	return &poolMaker{manager: manager, project: project, pool: pool}, nil
}

func (m *poolMaker) Bootstrap(ctx context.Context) (poolagent.Bootstrap, error) {
	controlPlanePublicKey, err := m.manager.EnsureAgentTrustKey(ctx)
	if err != nil {
		return poolagent.Bootstrap{}, err
	}
	return poolagent.Bootstrap{
		ProjectID:       m.project.ID,
		PoolID:          m.pool.ID,
		ControlPlaneKey: controlPlanePublicKey,
	}, nil
}

func (m *poolMaker) Box(ctx context.Context, platform imagecache.Platform) (sandbox.PoolBox, error) {
	return m.manager.PoolBox(ctx, platform)
}

func (m *poolMaker) Enroll(ctx context.Context, publicKey string) error {
	return m.manager.EnrollPool(ctx, m.pool.ID, publicKey)
}
