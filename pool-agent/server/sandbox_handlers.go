package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	workerapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	"github.com/ogen-go/ogen/ogenerrors"
)

type Identity struct {
	ProjectID string
	PoolID    string
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

func (s *sandboxService) HandlePoolBearerAuth(ctx context.Context, operation workerapi.OperationName, _ workerapi.PoolBearerAuth) (context.Context, error) {
	claims, ok := SignedTokenClaimsFromContext(ctx)
	if !ok {
		return ctx, newStatusError(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	}
	requiredScope := requiredPoolOperationScope(operation)
	if requiredScope != "" && !claims.HasScope(requiredScope) {
		return ctx, newStatusError(http.StatusForbidden, http.StatusText(http.StatusForbidden))
	}
	return ctx, nil
}

func (s *sandboxService) PoolListSandboxes(ctx context.Context, params workerapi.PoolListSandboxesParams) (*workerapimodel.PoolSandboxListResponse, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	sandboxes, err := s.runtime.ListSandboxes(ctx)
	if err != nil {
		return nil, mapRuntimeError(err)
	}
	out := make([]workerapimodel.PoolSandboxInstance, 0, len(sandboxes))
	for _, sb := range sandboxes {
		out = append(out, sandboxInstanceFromRuntime(sb, nil))
	}
	return &workerapimodel.PoolSandboxListResponse{Sandboxes: out}, nil
}

func (s *sandboxService) PoolCreateSandbox(ctx context.Context, req *workerapimodel.PoolSandboxCreateRequest, params workerapi.PoolCreateSandboxParams) (*workerapimodel.PoolSandboxInstance, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	converted, err := convert[workerapimodel.PoolSandboxCreateRequest](req)
	if err != nil {
		return nil, err
	}
	sb, err := s.runtime.CreateSandbox(ctx, &converted)
	return sandboxOutput(sb, err, &converted.Config)
}

func (s *sandboxService) PoolGetSandbox(ctx context.Context, params workerapi.PoolGetSandboxParams) (*workerapimodel.PoolSandboxInstance, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	sb, err := s.runtime.GetSandbox(ctx, params.SandboxId)
	return sandboxOutput(sb, err, nil)
}

func (s *sandboxService) PoolUpdateSandbox(ctx context.Context, req *workerapimodel.PoolSandboxUpdateRequest, params workerapi.PoolUpdateSandboxParams) (*workerapimodel.PoolSandboxInstance, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	converted, err := convert[workerapimodel.PoolSandboxUpdateRequest](req)
	if err != nil {
		return nil, err
	}
	sb, err := s.runtime.UpdateSandbox(ctx, params.SandboxId, &converted)
	return sandboxOutput(sb, err, nil)
}

// PoolArchiveSandbox and PoolDeleteSandbox both answer only when the work is
// done, unlike the power instructions below. Each is a destructive act on state
// only this agent can see, so acceptance would be a promise the control plane
// could not verify — and in the delete case the row it would verify against is
// the thing being removed (ADR 0022 §3).
func (s *sandboxService) PoolArchiveSandbox(ctx context.Context, params workerapi.PoolArchiveSandboxParams) error {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return err
	}
	if err := s.runtime.ArchiveSandbox(ctx, params.SandboxId); err != nil {
		return mapRuntimeError(err)
	}
	return nil
}

func (s *sandboxService) PoolDeleteSandbox(ctx context.Context, params workerapi.PoolDeleteSandboxParams) error {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return err
	}
	if err := s.runtime.DeleteSandbox(ctx, params.SandboxId); err != nil {
		return mapRuntimeError(err)
	}
	return nil
}

func (s *sandboxService) PoolSync(ctx context.Context, req *workerapimodel.PoolSyncRequest, params workerapi.PoolSyncParams) error {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return err
	}
	if err := s.runtime.SyncKnownPools(ctx, req.KnownPoolIds); err != nil {
		return mapRuntimeError(err)
	}
	return nil
}

// PoolStartSandbox, PoolStopSandbox, and PoolRestartSandbox accept an
// instruction and answer with acceptance alone. The resulting state — starting,
// then running or stopped — is published on the agent's own state-reporting
// channel, because a response cannot express a transition that has not finished
// (ADR 0017 §§9-10).
func (s *sandboxService) PoolStartSandbox(ctx context.Context, req *workerapimodel.PoolSandboxOperationRequest, params workerapi.PoolStartSandboxParams) (*workerapimodel.PoolSandboxOperationAccepted, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	converted, err := convert[workerapimodel.PoolSandboxOperationRequest](req)
	if err != nil {
		return nil, err
	}
	if err := s.runtime.StartSandbox(ctx, params.SandboxId, &converted); err != nil {
		return nil, err
	}
	return &workerapimodel.PoolSandboxOperationAccepted{SandboxId: params.SandboxId}, nil
}

func (s *sandboxService) PoolStopSandbox(ctx context.Context, req *workerapimodel.PoolSandboxOperationRequest, params workerapi.PoolStopSandboxParams) (*workerapimodel.PoolSandboxOperationAccepted, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	converted, err := convert[workerapimodel.PoolSandboxOperationRequest](req)
	if err != nil {
		return nil, err
	}
	if err := s.runtime.StopSandbox(ctx, params.SandboxId, &converted); err != nil {
		return nil, err
	}
	return &workerapimodel.PoolSandboxOperationAccepted{SandboxId: params.SandboxId}, nil
}

func (s *sandboxService) PoolRestartSandbox(ctx context.Context, req *workerapimodel.PoolSandboxOperationRequest, params workerapi.PoolRestartSandboxParams) (*workerapimodel.PoolSandboxOperationAccepted, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	converted, err := convert[workerapimodel.PoolSandboxOperationRequest](req)
	if err != nil {
		return nil, err
	}
	if err := s.runtime.RestartSandbox(ctx, params.SandboxId, &converted); err != nil {
		return nil, err
	}
	return &workerapimodel.PoolSandboxOperationAccepted{SandboxId: params.SandboxId}, nil
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
	response := workerapimodel.ErrorModel{
		Status: workerapi.NewOptInt64(int64(status)),
		Title:  workerapi.NewOptString(http.StatusText(status)),
		Detail: workerapi.NewOptString(err.Error()),
	}
	var typedErr interface{ ErrorType() string }
	if errors.As(err, &typedErr) {
		if errorType := typedErr.ErrorType(); errorType != "" {
			if parsed, parseErr := url.Parse(errorType); parseErr == nil {
				response.Type = workerapi.NewOptURI(*parsed)
			}
		}
	}
	return &workerapi.ErrorModelStatusCode{
		StatusCode: status,
		Response:   response,
	}
}

func (s *sandboxService) authorize(projectID, poolID string) error {
	if s.runtime == nil {
		return newStatusError(http.StatusServiceUnavailable, "sandbox runtime is not configured")
	}
	if projectID != s.identity.ProjectID || poolID != s.identity.PoolID {
		return newStatusError(http.StatusNotFound, "pool sandbox route not found")
	}
	return nil
}

func sandboxOutput(sb *sandboxruntime.Sandbox, err error, config *workerapimodel.SandboxConfig) (*workerapimodel.PoolSandboxInstance, error) {
	if err != nil {
		return nil, mapRuntimeError(err)
	}
	if sb == nil {
		return nil, newStatusError(http.StatusNotFound, http.StatusText(http.StatusNotFound))
	}
	out := sandboxInstanceFromRuntime(sb, config)
	return &out, nil
}

func sandboxInstanceFromRuntime(sb *sandboxruntime.Sandbox, config *workerapimodel.SandboxConfig) workerapimodel.PoolSandboxInstance {
	normalizeSandboxResponse(sb)
	if config == nil {
		config = &workerapimodel.SandboxConfig{
			Image: workerapi.NewOptString(sb.Image),
			Env:   workerapi.NewOptSandboxConfigEnv(workerapi.SandboxConfigEnv(sb.Env)),
		}
	}
	return workerapimodel.PoolSandboxInstance{
		SandboxId: sb.SandboxID,
		Config:    *config,
		Runtime: workerapimodel.PoolSandboxRuntime{
			InstanceId: sb.ID,
			Status:     string(sb.Status),
			Image:      sb.Image,
			CreatedAt:  sb.CreatedAt,
			StartedAt:  nilDateTime(sb.StartedAt),
			StoppedAt:  nilDateTime(sb.StoppedAt),
			Error:      sb.Error,
			Metadata:   workerapi.PoolSandboxRuntimeMetadata(sb.Metadata),
			Ports:      workerSandboxPorts(sb.Ports),
			Env:        workerapi.PoolSandboxRuntimeEnv(sb.Env),
		},
	}
}

func nilDateTime(value *time.Time) workerapi.NilDateTime {
	if value == nil {
		return workerapi.NilDateTime{Null: true}
	}
	return workerapi.NewNilDateTime(*value)
}

func workerSandboxPorts(in []sandboxruntime.AssignedPort) []workerapimodel.PoolSandboxPort {
	out := make([]workerapimodel.PoolSandboxPort, 0, len(in))
	for _, port := range in {
		out = append(out, workerapimodel.PoolSandboxPort{
			ContainerPort: int64(port.ContainerPort),
			HostIp:        port.HostIP,
			HostPort:      int64(port.HostPort),
			Protocol:      port.Protocol,
		})
	}
	return out
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
	// Archived carries its own message: unlike the two above, the caller's next
	// move (unarchive) is not obvious from the status alone. It also carries a
	// type, because the message is for a human and the control plane has to
	// tell this 409 from the already-exists one above to act on it at all.
	if errors.Is(err, sandboxruntime.ErrArchived) {
		return newTypedStatusError(http.StatusConflict, err.Error(), workerapimodel.ErrorTypeSandboxArchived)
	}
	return newStatusError(http.StatusInternalServerError, err.Error())
}

type statusError struct {
	status    int
	message   string
	errorType string
}

func (e statusError) Error() string {
	return e.message
}

func (e statusError) StatusCode() int {
	return e.status
}

// ErrorType is the RFC 7807 `type` for this error, empty when it has none.
// NewError copies it onto the response so a caller can classify the failure
// without parsing the message.
func (e statusError) ErrorType() string {
	return e.errorType
}

func newStatusError(status int, message string) error {
	return statusError{status: status, message: message}
}

func newTypedStatusError(status int, message, errorType string) error {
	return statusError{status: status, message: message, errorType: errorType}
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
