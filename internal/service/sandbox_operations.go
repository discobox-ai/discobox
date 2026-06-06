package service

import (
	"context"
	"time"

	"github.com/obot-platform/disco2/internal/model"
)

// SandboxOperations contains the provider-facing mechanics for sandboxes.
//
// The current implementation is intentionally stubbed. Real provider logic
// belongs here rather than in API service methods or reconciliation bookkeeping.
type SandboxOperations struct{}

func NewSandboxOperations() *SandboxOperations {
	return &SandboxOperations{}
}

func (o *SandboxOperations) Start(ctx context.Context, sandbox *model.Sandbox) error {
	now := time.Now().UTC()
	sandbox.LastActiveAt = &now
	return nil
}

func (o *SandboxOperations) Stop(ctx context.Context, sandbox *model.Sandbox) error {
	return nil
}

func (o *SandboxOperations) Delete(ctx context.Context, sandbox *model.Sandbox) error {
	return nil
}
