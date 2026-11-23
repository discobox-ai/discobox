package services

import (
	"context"
	"time"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// PoolToAPI presents effective health without changing the persisted agent
// observations. Every pool response uses this same freshness gate as placement.
//
// A discobox calling the API (a sandbox principal on ctx) is sent only
// sandboxPoolView of it, wherever the pool appears: read itself, or embedded
// in a sandbox (ADR 26-11-23-743 §2).
func PoolToAPI(ctx context.Context, pool *model.Pool) (apimodel.Pool, error) {
	health := pool.Health(time.Now())
	ready := health == model.PoolHealthReady
	view, err := Convert[apimodel.Pool](struct {
		*model.Pool
		Health      string `json:"health"`
		Ready       bool   `json:"ready"`
		Schedulable bool   `json:"schedulable"`
	}{pool, health, ready, ready && pool.Schedulable})
	if err != nil {
		return view, err
	}
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
		return sandboxPoolView(view), nil
	}
	return view, nil
}

// sandboxPoolView is what a discobox is sent of a pool: what placing a create
// on it needs — which pool it is, the platform it hosts, whether it takes
// sandboxes and its room, and where its lifecycle stands (ADR 26-11-23-743
// §2). It copies those fields and no others, so a field added to Pool reaches
// a discobox only once it is added here. Left out: the provider instance and
// its configuration, the agent's key, the host credential, proxy, outbound and
// egress settings, conditions, resources, provisioning progress and errors.
func sandboxPoolView(pool apimodel.Pool) apimodel.Pool {
	return apimodel.Pool{
		ID:                    pool.ID,
		ProjectId:             pool.ProjectId,
		Name:                  pool.Name,
		ProviderInstanceId:    pool.ProviderInstanceId,
		Platform:              pool.Platform,
		SandboxOs:             pool.SandboxOs,
		ImageKind:             pool.ImageKind,
		CpuVcpus:              pool.CpuVcpus,
		MemoryBytes:           pool.MemoryBytes,
		StorageBytes:          pool.StorageBytes,
		Health:                pool.Health,
		Ready:                 pool.Ready,
		Schedulable:           pool.Schedulable,
		Degraded:              pool.Degraded,
		AvailableCpuVcpus:     pool.AvailableCpuVcpus,
		AvailableMemoryBytes:  pool.AvailableMemoryBytes,
		AvailableStorageBytes: pool.AvailableStorageBytes,
		DesiredState:          pool.DesiredState,
		State:                 pool.State,
		StateChangedAt:        pool.StateChangedAt,
		Generation:            pool.Generation,
		ObservedGeneration:    pool.ObservedGeneration,
		CreatedAt:             pool.CreatedAt,
		UpdatedAt:             pool.UpdatedAt,
	}
}
