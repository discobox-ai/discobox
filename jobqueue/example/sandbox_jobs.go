package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/obot-platform/disco2/jobqueue"
)

const (
	TypeSandboxProvision jobqueue.Type = "sandbox.provision"
	TypeSandboxDelete    jobqueue.Type = "sandbox.delete"
)

type SandboxProvisionPayload struct {
	ProjectID string `json:"projectId"`
	SandboxID string `json:"sandboxId"`
}

func (p SandboxProvisionPayload) JobType() jobqueue.Type {
	return TypeSandboxProvision
}

func (p SandboxProvisionPayload) Resource() jobqueue.Resource {
	return jobqueue.Resource{Type: "sandbox", ID: p.SandboxID}
}

func (p SandboxProvisionPayload) MaxAttempts() int {
	return 1
}

type SandboxDeletePayload struct {
	ProjectID string    `json:"projectId"`
	SandboxID string    `json:"sandboxId"`
	DeleteAt  time.Time `json:"deleteAt"`
}

func (p SandboxDeletePayload) JobType() jobqueue.Type {
	return TypeSandboxDelete
}

func (p SandboxDeletePayload) Resource() jobqueue.Resource {
	return jobqueue.Resource{Type: "sandbox", ID: p.SandboxID}
}

func (p SandboxDeletePayload) ScheduledAt() time.Time {
	return p.DeleteAt
}

func (p SandboxDeletePayload) Priority() int {
	return 20
}

type SandboxService interface {
	Provision(ctx context.Context, projectID, sandboxID string) error
	Delete(ctx context.Context, projectID, sandboxID string) error
}

type SandboxProvisionExecutor struct {
	sandboxes SandboxService
}

func NewSandboxProvisionExecutor(sandboxes SandboxService) *SandboxProvisionExecutor {
	return &SandboxProvisionExecutor{sandboxes: sandboxes}
}

func (e *SandboxProvisionExecutor) Type() jobqueue.Type {
	return TypeSandboxProvision
}

func (e *SandboxProvisionExecutor) Execute(ctx context.Context, job *jobqueue.Job) error {
	var payload SandboxProvisionPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("invalid sandbox provision payload: %w", err)
	}
	if payload.ProjectID == "" || payload.SandboxID == "" {
		return fmt.Errorf("projectId and sandboxId are required")
	}
	return e.sandboxes.Provision(ctx, payload.ProjectID, payload.SandboxID)
}

type SandboxDeleteExecutor struct {
	sandboxes SandboxService
}

func NewSandboxDeleteExecutor(sandboxes SandboxService) *SandboxDeleteExecutor {
	return &SandboxDeleteExecutor{sandboxes: sandboxes}
}

func (e *SandboxDeleteExecutor) Type() jobqueue.Type {
	return TypeSandboxDelete
}

func (e *SandboxDeleteExecutor) Execute(ctx context.Context, job *jobqueue.Job) error {
	var payload SandboxDeletePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("invalid sandbox delete payload: %w", err)
	}
	if payload.ProjectID == "" || payload.SandboxID == "" {
		return fmt.Errorf("projectId and sandboxId are required")
	}
	return e.sandboxes.Delete(ctx, payload.ProjectID, payload.SandboxID)
}
