package service

import (
	"context"

	"github.com/obot-platform/disco2/internal/jobs"
	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/orchestration"
	"github.com/obot-platform/disco2/internal/store"
	"github.com/obot-platform/disco2/jobqueue"
)

type SandboxOrchestrator struct {
	store        *store.Store
	orchestrator *orchestration.Orchestrator
}

func NewSandboxOrchestrator(store *store.Store, orchestrator *orchestration.Orchestrator) *SandboxOrchestrator {
	return &SandboxOrchestrator{
		store:        store,
		orchestrator: orchestrator,
	}
}

func (o *SandboxOrchestrator) Create(ctx context.Context, sandbox *model.Sandbox) (*model.Sandbox, error) {
	orchestrated, err := orchestration.Begin(ctx,
		o.orchestrator,
		model.SandboxCreateOperation,
		func(context.Context, *store.Store) (*model.Sandbox, error) {
			return sandbox, nil
		},
		func(ctx context.Context, txStore *store.Store, resource *model.Sandbox) error {
			return txStore.CreateSandbox(ctx, resource)
		},
		sandboxReconcilePayload,
	)
	if err != nil {
		return nil, err
	}
	return o.store.GetSandbox(ctx, sandbox.ProjectID, orchestrated.ID)
}

func (o *SandboxOrchestrator) Begin(ctx context.Context, projectID, sandboxID string, spec model.OperationSpec, mutate ...func(*model.Sandbox)) (*model.Sandbox, error) {
	orchestrated, err := orchestration.Begin(ctx,
		o.orchestrator,
		spec,
		func(ctx context.Context, txStore *store.Store) (*model.Sandbox, error) {
			return txStore.GetSandbox(ctx, projectID, sandboxID)
		},
		func(ctx context.Context, txStore *store.Store, resource *model.Sandbox) error {
			return txStore.UpdateSandbox(ctx, resource)
		},
		sandboxReconcilePayload,
		mutate...,
	)
	if err != nil {
		return nil, err
	}
	return o.store.GetSandbox(ctx, projectID, orchestrated.ID)
}

func sandboxReconcilePayload(sandbox *model.Sandbox) jobqueue.Payload {
	return jobs.SandboxReconcilePayload{
		ProjectID:  sandbox.ProjectID,
		SandboxID:  sandbox.ID,
		Generation: sandbox.Generation,
	}
}
