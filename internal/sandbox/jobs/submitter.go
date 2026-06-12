package jobs

import (
	"context"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/store"
	"github.com/obot-platform/discobox/orchestration"
)

type SandboxID struct {
	ProjectID string
	SandboxID string
}

type SandboxSubmitter struct {
	store     *store.Store
	submitter *orchestration.Submitter[*model.Sandbox, model.OperationSpec, SandboxID, *store.Store]
}

func NewSandboxSubmitter(appStore *store.Store, queueConfig orchestration.QueueConfig, notify func(context.Context)) *SandboxSubmitter {
	return &SandboxSubmitter{
		store: appStore,
		submitter: orchestration.NewSubmitter(orchestration.SubmitterConfig[*model.Sandbox, model.OperationSpec, SandboxID, *store.Store]{
			Transaction: func(ctx context.Context, fn func(context.Context, *store.Store) error) error {
				return appStore.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
					return fn(ctx, txStore)
				})
			},
			Resource: orchestration.ResourceStore[*model.Sandbox, SandboxID, *store.Store]{
				Get: func(ctx context.Context, txStore *store.Store, id SandboxID) (*model.Sandbox, error) {
					return txStore.GetSandbox(ctx, id.ProjectID, id.SandboxID)
				},
				Create: func(ctx context.Context, txStore *store.Store, sandbox *model.Sandbox) error {
					return txStore.CreateSandbox(ctx, sandbox)
				},
				Update: func(ctx context.Context, txStore *store.Store, sandbox *model.Sandbox) error {
					return txStore.UpdateSandbox(ctx, sandbox)
				},
			},
			Payload:     sandboxReconcilePayload,
			QueueConfig: queueConfig,
			Notify:      notify,
		}),
	}
}

func (s *SandboxSubmitter) Create(ctx context.Context, sandbox *model.Sandbox) (*model.Sandbox, error) {
	accepted, err := s.submitter.Create(ctx, sandbox, model.SandboxCreateOperation)
	if err != nil {
		return nil, err
	}
	return s.store.GetSandbox(ctx, sandbox.ProjectID, accepted.ID)
}

func (s *SandboxSubmitter) Submit(ctx context.Context, projectID, sandboxID string, spec model.OperationSpec, mutate ...func(*model.Sandbox)) (*model.Sandbox, error) {
	accepted, err := s.submitter.Submit(ctx, SandboxID{ProjectID: projectID, SandboxID: sandboxID}, spec, mutate...)
	if err != nil {
		return nil, err
	}
	return s.store.GetSandbox(ctx, projectID, accepted.ID)
}

func sandboxReconcilePayload(sandbox *model.Sandbox) orchestration.Payload {
	return SandboxReconcilePayload{
		ProjectID:  sandbox.ProjectID,
		SandboxID:  sandbox.ID,
		Generation: sandbox.Generation,
	}
}
