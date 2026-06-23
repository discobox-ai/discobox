package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	workerapi "github.com/obot-platform/discobox/worker-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/worker-agent/api/model"
	"github.com/obot-platform/discobox/worker-agent/sandboxruntime"
	"github.com/ogen-go/ogen/ogenerrors"
)

type Identity struct {
	ProjectID string
	SandboxID string
	WorkerID  string
}

type sandboxService struct {
	identity Identity
	runtime  sandboxruntime.Runtime
}

var _ workerapi.Handler = (*sandboxService)(nil)
var _ workerapi.SecurityHandler = (*sandboxService)(nil)

func newSandboxService(identity Identity, runtime sandboxruntime.Runtime) *sandboxService {
	return &sandboxService{identity: identity, runtime: runtime}
}

func (s *sandboxService) HandleWorkerBearerAuth(ctx context.Context, operation workerapi.OperationName, _ workerapi.WorkerBearerAuth) (context.Context, error) {
	claims, ok := SignedTokenClaimsFromContext(ctx)
	if !ok {
		return ctx, newStatusError(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	}
	requiredScope := requiredWorkerOperationScope(operation)
	if requiredScope != "" && !claims.HasScope(requiredScope) {
		return ctx, newStatusError(http.StatusForbidden, http.StatusText(http.StatusForbidden))
	}
	return ctx, nil
}

func (s *sandboxService) WorkerListSandboxes(ctx context.Context, params workerapi.WorkerListSandboxesParams) (*workerapimodel.WorkerSandboxListResponse, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	sandboxes, err := s.runtime.ListSandboxes(ctx)
	if err != nil {
		return nil, mapRuntimeError(err)
	}
	out := make([]workerapimodel.Sandbox, 0, len(sandboxes))
	for _, sb := range sandboxes {
		normalizeSandboxResponse(sb)
		converted, err := convert[workerapimodel.Sandbox](sb)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return &workerapimodel.WorkerSandboxListResponse{Sandboxes: out}, nil
}

func (s *sandboxService) WorkerCreateSandbox(ctx context.Context, req *workerapimodel.WorkerSandboxCreateRequest, params workerapi.WorkerCreateSandboxParams) (*workerapimodel.Sandbox, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	converted, err := convert[workerapimodel.WorkerSandboxCreateRequest](req)
	if err != nil {
		return nil, err
	}
	sb, err := s.runtime.CreateSandbox(ctx, &converted)
	return sandboxOutput(sb, err)
}

func (s *sandboxService) WorkerGetSandbox(ctx context.Context, params workerapi.WorkerGetSandboxParams) (*workerapimodel.Sandbox, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	sb, err := s.runtime.GetSandbox(ctx, params.SandboxId)
	return sandboxOutput(sb, err)
}

func (s *sandboxService) WorkerUpdateSandbox(ctx context.Context, req *workerapimodel.WorkerSandboxUpdateRequest, params workerapi.WorkerUpdateSandboxParams) (*workerapimodel.Sandbox, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	converted, err := convert[workerapimodel.WorkerSandboxUpdateRequest](req)
	if err != nil {
		return nil, err
	}
	sb, err := s.runtime.UpdateSandbox(ctx, params.SandboxId, &converted)
	return sandboxOutput(sb, err)
}

func (s *sandboxService) WorkerDeleteSandbox(ctx context.Context, params workerapi.WorkerDeleteSandboxParams) error {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return err
	}
	if err := s.runtime.DeleteSandbox(ctx, params.SandboxId); err != nil {
		return mapRuntimeError(err)
	}
	return nil
}

func (s *sandboxService) WorkerStartSandbox(ctx context.Context, req *workerapimodel.WorkerSandboxOperationRequest, params workerapi.WorkerStartSandboxParams) (*workerapimodel.Sandbox, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	converted, err := convert[workerapimodel.WorkerSandboxOperationRequest](req)
	if err != nil {
		return nil, err
	}
	sb, err := s.runtime.StartSandbox(ctx, params.SandboxId, &converted)
	return sandboxOutput(sb, err)
}

func (s *sandboxService) WorkerStopSandbox(ctx context.Context, req *workerapimodel.WorkerSandboxOperationRequest, params workerapi.WorkerStopSandboxParams) (*workerapimodel.Sandbox, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	converted, err := convert[workerapimodel.WorkerSandboxOperationRequest](req)
	if err != nil {
		return nil, err
	}
	sb, err := s.runtime.StopSandbox(ctx, params.SandboxId, &converted)
	return sandboxOutput(sb, err)
}

func (s *sandboxService) NewError(_ context.Context, err error) *workerapi.ErrorModelStatusCode {
	status := http.StatusInternalServerError
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		status = statusErr.StatusCode()
	} else if errors.Is(err, ogenerrors.ErrSecurityRequirementIsNotSatisfied) {
		status = http.StatusUnauthorized
	} else {
		var securityErr *ogenerrors.SecurityError
		if errors.As(err, &securityErr) {
			status = http.StatusUnauthorized
		}
	}
	return &workerapi.ErrorModelStatusCode{
		StatusCode: status,
		Response: workerapimodel.ErrorModel{
			Status: workerapi.NewOptInt64(int64(status)),
			Title:  workerapi.NewOptString(http.StatusText(status)),
			Detail: workerapi.NewOptString(err.Error()),
		},
	}
}

func (s *sandboxService) authorize(projectID, workerID string) error {
	if s.runtime == nil {
		return newStatusError(http.StatusServiceUnavailable, "sandbox runtime is not configured")
	}
	if projectID != s.identity.ProjectID || workerID != s.identity.WorkerID {
		return newStatusError(http.StatusNotFound, "worker sandbox route not found")
	}
	return nil
}

func sandboxOutput(sb *sandboxruntime.Sandbox, err error) (*workerapimodel.Sandbox, error) {
	if err != nil {
		return nil, mapRuntimeError(err)
	}
	if sb == nil {
		return nil, newStatusError(http.StatusNotFound, http.StatusText(http.StatusNotFound))
	}
	normalizeSandboxResponse(sb)
	out, err := convert[workerapimodel.Sandbox](sb)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func normalizeSandboxResponse(sb *sandboxruntime.Sandbox) {
	if sb == nil {
		return
	}
	if sb.Metadata == nil {
		sb.Metadata = map[string]string{}
	}
	if sb.Env == nil {
		sb.Env = map[string]string{}
	}
	if sb.Ports == nil {
		sb.Ports = []sandboxruntime.AssignedPort{}
	}
}

func mapRuntimeError(err error) error {
	if errors.Is(err, sandboxruntime.ErrNotFound) {
		return newStatusError(http.StatusNotFound, http.StatusText(http.StatusNotFound))
	}
	if errors.Is(err, sandboxruntime.ErrAlreadyExists) {
		return newStatusError(http.StatusConflict, http.StatusText(http.StatusConflict))
	}
	return newStatusError(http.StatusInternalServerError, err.Error())
}

type statusError struct {
	status  int
	message string
}

func (e statusError) Error() string {
	return e.message
}

func (e statusError) StatusCode() int {
	return e.status
}

func newStatusError(status int, message string) error {
	return statusError{status: status, message: message}
}

func convert[To any](from any) (To, error) {
	var to To
	data, err := json.Marshal(from)
	if err != nil {
		return to, err
	}
	if err := json.Unmarshal(data, &to); err != nil {
		return to, err
	}
	return to, nil
}
