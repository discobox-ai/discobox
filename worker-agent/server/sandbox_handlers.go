package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/apperrors"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
	workerclient "github.com/obot-platform/discobox/worker-agent/api/clientgen"
	workerapi "github.com/obot-platform/discobox/worker-agent/api/servergen"
	"github.com/obot-platform/discobox/worker-agent/sandboxruntime"
	"github.com/ogen-go/ogen/ogenerrors"
)

type Identity struct {
	ProjectID string
	SandboxID string
	WorkerID  string
}

type sandboxService struct {
	identity   Identity
	runtime    sandboxruntime.Runtime
	authTokens []string
}

var _ workerapi.Handler = (*sandboxService)(nil)
var _ workerapi.SecurityHandler = (*sandboxService)(nil)

func newSandboxService(identity Identity, runtime sandboxruntime.Runtime, authTokens ...string) *sandboxService {
	return &sandboxService{identity: identity, runtime: runtime, authTokens: normalizeAuthTokens(authTokens...)}
}

func (s *sandboxService) HandleWorkerBearerAuth(ctx context.Context, _ workerapi.OperationName, token workerapi.WorkerBearerAuth) (context.Context, error) {
	if !authorizedWorkerToken(token.Token, s.authTokens) {
		return ctx, apperrors.NewStatusError(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	}
	return ctx, nil
}

func (s *sandboxService) WorkerListSandboxes(ctx context.Context, params workerapi.WorkerListSandboxesParams) (*workerapi.WorkerSandboxListResponse, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	sandboxes, err := s.runtime.ListSandboxes(ctx)
	if err != nil {
		return nil, mapRuntimeError(err)
	}
	out := make([]workerapi.Sandbox, 0, len(sandboxes))
	for _, sb := range sandboxes {
		normalizeSandboxResponse(sb)
		converted, err := convert[workerapi.Sandbox](sb)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return &workerapi.WorkerSandboxListResponse{Sandboxes: out}, nil
}

func (s *sandboxService) WorkerCreateSandbox(ctx context.Context, req *workerapi.WorkerSandboxCreateRequest, params workerapi.WorkerCreateSandboxParams) (*workerapi.Sandbox, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	converted, err := convert[workerclient.WorkerSandboxCreateRequest](req)
	if err != nil {
		return nil, err
	}
	sb, err := s.runtime.CreateSandbox(ctx, &converted)
	return sandboxOutput(sb, err)
}

func (s *sandboxService) WorkerGetSandbox(ctx context.Context, params workerapi.WorkerGetSandboxParams) (*workerapi.Sandbox, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	sb, err := s.runtime.GetSandbox(ctx, params.SandboxId)
	return sandboxOutput(sb, err)
}

func (s *sandboxService) WorkerUpdateSandbox(ctx context.Context, req *workerapi.WorkerSandboxUpdateRequest, params workerapi.WorkerUpdateSandboxParams) (*workerapi.Sandbox, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	converted, err := convert[workerclient.WorkerSandboxUpdateRequest](req)
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

func (s *sandboxService) WorkerStartSandbox(ctx context.Context, req *workerapi.WorkerSandboxOperationRequest, params workerapi.WorkerStartSandboxParams) (*workerapi.Sandbox, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	converted, err := convert[workerclient.WorkerSandboxOperationRequest](req)
	if err != nil {
		return nil, err
	}
	sb, err := s.runtime.StartSandbox(ctx, params.SandboxId, &converted)
	return sandboxOutput(sb, err)
}

func (s *sandboxService) WorkerStopSandbox(ctx context.Context, req *workerapi.WorkerSandboxOperationRequest, params workerapi.WorkerStopSandboxParams) (*workerapi.Sandbox, error) {
	if err := s.authorize(params.ProjectId, params.WorkerId); err != nil {
		return nil, err
	}
	converted, err := convert[workerclient.WorkerSandboxOperationRequest](req)
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
		Response: workerapi.ErrorModel{
			Status: workerapi.NewOptInt64(int64(status)),
			Title:  workerapi.NewOptString(http.StatusText(status)),
			Detail: workerapi.NewOptString(err.Error()),
		},
	}
}

func (s *sandboxService) authorize(projectID, workerID string) error {
	if s.runtime == nil {
		return apperrors.NewStatusError(http.StatusServiceUnavailable, "sandbox runtime is not configured")
	}
	if projectID != s.identity.ProjectID || workerID != s.identity.WorkerID {
		return apperrors.NewStatusError(http.StatusNotFound, "worker sandbox route not found")
	}
	return nil
}

func sandboxOutput(sb *sandbox.Sandbox, err error) (*workerapi.Sandbox, error) {
	if err != nil {
		return nil, mapRuntimeError(err)
	}
	if sb == nil {
		return nil, apperrors.NewStatusError(http.StatusNotFound, http.StatusText(http.StatusNotFound))
	}
	normalizeSandboxResponse(sb)
	out, err := convert[workerapi.Sandbox](sb)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func normalizeSandboxResponse(sb *sandbox.Sandbox) {
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
		sb.Ports = []sandbox.AssignedPort{}
	}
}

func mapRuntimeError(err error) error {
	if errors.Is(err, sandbox.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, http.StatusText(http.StatusNotFound))
	}
	if errors.Is(err, sandbox.ErrAlreadyExists) {
		return apperrors.NewStatusError(http.StatusConflict, http.StatusText(http.StatusConflict))
	}
	return apperrors.NewStatusError(http.StatusInternalServerError, err.Error())
}

func normalizeAuthTokens(tokens ...string) []string {
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token = strings.TrimSpace(token); token != "" {
			out = append(out, token)
		}
	}
	return out
}

func authorizedWorkerToken(got string, tokens []string) bool {
	if got == "" {
		return false
	}
	for _, want := range tokens {
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return true
		}
	}
	return false
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
