package poolruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/server/internal/model"
)

const defaultPoolBootstrapTTL = 30 * time.Minute

// mintPoolBootstrap returns the deferred minter handed to a runtime provider.
// The provider calls it only when it actually creates a runtime, so a drift
// check over a healthy pool persists no bootstrap token. The runtime provider
// fills runtime-specific fields such as the control plane URL and agent port.
func mintPoolBootstrap(manager PoolManager, project *model.Project, pool *model.Pool) poolagent.MintBootstrap {
	return func(ctx context.Context) (poolagent.Bootstrap, error) {
		token, err := createPoolBootstrap(ctx, manager, project, pool)
		if err != nil {
			return poolagent.Bootstrap{}, err
		}
		controlPlanePublicKey, err := manager.EnsureAgentTrustKey(ctx)
		if err != nil {
			return poolagent.Bootstrap{}, err
		}
		return poolagent.Bootstrap{
			ProjectID:       project.ID,
			PoolID:          pool.ID,
			Token:           token,
			ControlPlaneKey: controlPlanePublicKey,
		}, nil
	}
}

func createPoolBootstrap(ctx context.Context, manager PoolManager, project *model.Project, pool *model.Pool) (string, error) {
	if manager == nil {
		return "", fmt.Errorf("pool manager is required")
	}
	if project == nil {
		return "", fmt.Errorf("project is required")
	}
	if pool == nil {
		return "", fmt.Errorf("pool is required")
	}
	token, err := randomPoolToken()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(token))
	bootstrapToken := &model.PoolBootstrapToken{
		PoolID:    pool.ID,
		TokenHash: hash[:],
		ExpiresAt: time.Now().UTC().Add(defaultPoolBootstrapTTL),
	}
	if err := manager.CreatePoolBootstrapToken(ctx, bootstrapToken); err != nil {
		return "", err
	}
	return token, nil
}

func randomPoolToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
