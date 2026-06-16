package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/discobox/model"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
	workerclient "github.com/obot-platform/discobox/worker-agent/workeragent/client/gen"
	"github.com/obot-platform/discobox/worker-agent/workeragent/sandboxruntime"
)

type Identity struct {
	TenantID  string
	ProjectID string
	SandboxID string
	WorkerID  string
}

type sandboxService struct {
	identity   Identity
	runtime    sandboxruntime.Runtime
	authTokens []string
}

type WorkerSandboxCollectionInput struct {
	ProjectID string `path:"projectId" doc:"Project ID"`
	WorkerID  string `path:"workerId" doc:"Worker ID"`
}

type WorkerSandboxPathInput struct {
	ProjectID string `path:"projectId" doc:"Project ID"`
	WorkerID  string `path:"workerId" doc:"Worker ID"`
	SandboxID string `path:"sandboxId" doc:"Sandbox ID"`
}

type WorkerCreateSandboxInput struct {
	ProjectID string                     `path:"projectId" doc:"Project ID"`
	WorkerID  string                     `path:"workerId" doc:"Worker ID"`
	Body      workerSandboxCreateRequest `json:"body"`
}

type WorkerUpdateSandboxInput struct {
	ProjectID string                     `path:"projectId" doc:"Project ID"`
	WorkerID  string                     `path:"workerId" doc:"Worker ID"`
	SandboxID string                     `path:"sandboxId" doc:"Sandbox ID"`
	Body      workerSandboxUpdateRequest `json:"body"`
}

type WorkerSandboxOperationInput struct {
	ProjectID string                        `path:"projectId" doc:"Project ID"`
	WorkerID  string                        `path:"workerId" doc:"Worker ID"`
	SandboxID string                        `path:"sandboxId" doc:"Sandbox ID"`
	Body      workerSandboxOperationRequest `json:"body"`
}

type WorkerSandboxListOutput struct {
	Body workerSandboxListResponse
}

type WorkerSandboxOutput struct {
	Body sandbox.Sandbox
}

type WorkerDeleteSandboxOutput struct {
	Status int `status:"204"`
}

type workerSandboxCreateRequest struct {
	SandboxID                string                     `json:"sandboxId"`
	Image                    string                     `json:"image,omitempty"`
	Env                      map[string]string          `json:"env,omitempty"`
	Name                     string                     `json:"name,omitempty"`
	Description              *string                    `json:"description,omitempty"`
	ProviderInstanceID       string                     `json:"providerInstanceId,omitempty"`
	AgentConfigID            *string                    `json:"agentConfigId,omitempty"`
	AgentModel               *string                    `json:"agentModel,omitempty"`
	AgentModelServiceTier    *string                    `json:"agentModelServiceTier,omitempty"`
	AgentModelReasoningLevel *string                    `json:"agentModelReasoningLevel,omitempty"`
	Prompt                   *string                    `json:"prompt,omitempty"`
	SourceURL                string                     `json:"sourceUrl,omitempty"`
	SourceRef                string                     `json:"sourceRef,omitempty"`
	SourceRefType            string                     `json:"sourceRefType,omitempty"`
	SourceDirectory          string                     `json:"sourceDirectory,omitempty"`
	SourceCodeReferences     model.SourceCodeReferences `json:"sourceCodeReferences,omitempty"`
	UserUID                  *int                       `json:"userUid,omitempty"`
	UserGID                  *int                       `json:"userGid,omitempty"`
	WorkspacePath            string                     `json:"workspacePath,omitempty"`
	WorkspaceSource          string                     `json:"workspaceSource,omitempty"`
	WorkspaceRef             string                     `json:"workspaceRef,omitempty"`
	WorkingDirectory         string                     `json:"workingDirectory,omitempty"`
	Resources                sandbox.ResourceConfig     `json:"resources,omitempty"`
	CPUVCPUs                 float64                    `json:"cpuVcpus,omitempty"`
	MemoryBytes              int64                      `json:"memoryBytes,omitempty"`
	StorageBytes             int64                      `json:"storageBytes,omitempty"`
}

type workerSandboxUpdateRequest struct {
	Image            string                 `json:"image,omitempty"`
	Env              map[string]string      `json:"env,omitempty"`
	WorkingDirectory string                 `json:"workingDirectory,omitempty"`
	Resources        sandbox.ResourceConfig `json:"resources,omitempty"`
	CPUVCPUs         float64                `json:"cpuVcpus,omitempty"`
	MemoryBytes      int64                  `json:"memoryBytes,omitempty"`
	StorageBytes     int64                  `json:"storageBytes,omitempty"`
}

type workerSandboxOperationRequest struct {
	Force bool `json:"force,omitempty"`
}

type workerSandboxListResponse struct {
	Sandboxes []*sandbox.Sandbox `json:"sandboxes"`
}

// RegisterSandboxOperations registers worker-local sandbox Huma operations.
func RegisterSandboxOperations(api huma.API, identity Identity, runtime sandboxruntime.Runtime, authTokens ...string) {
	svc := sandboxService{identity: identity, runtime: runtime, authTokens: normalizeAuthTokens(authTokens...)}
	const routePrefix = "/api/project/{projectId}/worker/{workerId}"
	huma.Register(api, svc.operation(api, http.MethodGet, routePrefix+"/sandboxes", "worker-list-sandboxes", "List worker sandboxes", 0), svc.listSandboxes)
	huma.Register(api, svc.operation(api, http.MethodPost, routePrefix+"/sandboxes", "worker-create-sandbox", "Create worker sandbox", 0), svc.createSandbox)
	huma.Register(api, svc.operation(api, http.MethodGet, routePrefix+"/sandboxes/{sandboxId}", "worker-get-sandbox", "Get worker sandbox", 0), svc.getSandbox)
	huma.Register(api, svc.operation(api, http.MethodPatch, routePrefix+"/sandboxes/{sandboxId}", "worker-update-sandbox", "Update worker sandbox", 0), svc.updateSandbox)
	huma.Register(api, svc.operation(api, http.MethodDelete, routePrefix+"/sandboxes/{sandboxId}", "worker-delete-sandbox", "Delete worker sandbox", http.StatusNoContent), svc.deleteSandbox)
	huma.Register(api, svc.operation(api, http.MethodPost, routePrefix+"/sandboxes/{sandboxId}/start", "worker-start-sandbox", "Start worker sandbox", 0), svc.startSandbox)
	huma.Register(api, svc.operation(api, http.MethodPost, routePrefix+"/sandboxes/{sandboxId}/stop", "worker-stop-sandbox", "Stop worker sandbox", 0), svc.stopSandbox)
}

func (s sandboxService) operation(api huma.API, method, path, operationID, summary string, defaultStatus int) huma.Operation {
	return huma.Operation{
		OperationID:   operationID,
		Method:        method,
		Path:          path,
		DefaultStatus: defaultStatus,
		Summary:       summary,
		Tags:          []string{"Worker Sandboxes"},
		Security:      []map[string][]string{{"workerBearerAuth": []string{}}},
		Middlewares:   huma.Middlewares{s.authorizeMiddleware(api)},
	}
}

func (s sandboxService) authorizeMiddleware(api huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		if !authorizedWorkerRequest(ctx.Header("Authorization"), s.authTokens) {
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
			return
		}
		next(ctx)
	}
}

func (s sandboxService) listSandboxes(ctx context.Context, input *WorkerSandboxCollectionInput) (*WorkerSandboxListOutput, error) {
	if err := s.authorize(input.ProjectID, input.WorkerID); err != nil {
		return nil, err
	}
	sandboxes, err := s.runtime.ListSandboxes(ctx)
	if err != nil {
		return nil, mapRuntimeError(err)
	}
	for _, sb := range sandboxes {
		normalizeSandboxResponse(sb)
	}
	return &WorkerSandboxListOutput{Body: workerSandboxListResponse{Sandboxes: sandboxes}}, nil
}

func (s sandboxService) createSandbox(ctx context.Context, input *WorkerCreateSandboxInput) (*WorkerSandboxOutput, error) {
	if err := s.authorize(input.ProjectID, input.WorkerID); err != nil {
		return nil, err
	}
	sb, err := s.runtime.CreateSandbox(ctx, workerCreateRequest(input.Body))
	return sandboxOutput(sb, err)
}

func (s sandboxService) getSandbox(ctx context.Context, input *WorkerSandboxPathInput) (*WorkerSandboxOutput, error) {
	if err := s.authorize(input.ProjectID, input.WorkerID); err != nil {
		return nil, err
	}
	sb, err := s.runtime.GetSandbox(ctx, input.SandboxID)
	return sandboxOutput(sb, err)
}

func (s sandboxService) updateSandbox(ctx context.Context, input *WorkerUpdateSandboxInput) (*WorkerSandboxOutput, error) {
	if err := s.authorize(input.ProjectID, input.WorkerID); err != nil {
		return nil, err
	}
	sb, err := s.runtime.UpdateSandbox(ctx, input.SandboxID, workerUpdateRequest(input.Body))
	return sandboxOutput(sb, err)
}

func (s sandboxService) deleteSandbox(ctx context.Context, input *WorkerSandboxPathInput) (*WorkerDeleteSandboxOutput, error) {
	if err := s.authorize(input.ProjectID, input.WorkerID); err != nil {
		return nil, err
	}
	if err := s.runtime.DeleteSandbox(ctx, input.SandboxID); err != nil {
		return nil, mapRuntimeError(err)
	}
	return &WorkerDeleteSandboxOutput{Status: http.StatusNoContent}, nil
}

func (s sandboxService) startSandbox(ctx context.Context, input *WorkerSandboxOperationInput) (*WorkerSandboxOutput, error) {
	if err := s.authorize(input.ProjectID, input.WorkerID); err != nil {
		return nil, err
	}
	sb, err := s.runtime.StartSandbox(ctx, input.SandboxID, workerOperationRequest(input.Body))
	return sandboxOutput(sb, err)
}

func (s sandboxService) stopSandbox(ctx context.Context, input *WorkerSandboxOperationInput) (*WorkerSandboxOutput, error) {
	if err := s.authorize(input.ProjectID, input.WorkerID); err != nil {
		return nil, err
	}
	sb, err := s.runtime.StopSandbox(ctx, input.SandboxID, workerOperationRequest(input.Body))
	return sandboxOutput(sb, err)
}

func (s sandboxService) authorize(projectID, workerID string) error {
	if s.runtime == nil {
		return huma.Error503ServiceUnavailable("sandbox runtime is not configured")
	}
	if projectID != s.identity.ProjectID || workerID != s.identity.WorkerID {
		return huma.Error404NotFound("worker sandbox route not found")
	}
	return nil
}

func sandboxOutput(sb *sandbox.Sandbox, err error) (*WorkerSandboxOutput, error) {
	if err != nil {
		return nil, mapRuntimeError(err)
	}
	if sb == nil {
		return nil, huma.Error404NotFound(http.StatusText(http.StatusNotFound))
	}
	normalizeSandboxResponse(sb)
	return &WorkerSandboxOutput{Body: *sb}, nil
}

func workerCreateRequest(req workerSandboxCreateRequest) *workerclient.WorkerSandboxCreateRequest {
	out := &workerclient.WorkerSandboxCreateRequest{SandboxId: req.SandboxID}
	if req.Image != "" {
		out.Image = workerclient.NewOptString(req.Image)
	}
	if req.Env != nil {
		out.Env = workerclient.NewOptWorkerSandboxCreateRequestEnv(workerclient.WorkerSandboxCreateRequestEnv(req.Env))
	}
	if req.Name != "" {
		out.Name = workerclient.NewOptString(req.Name)
	}
	if req.Description != nil {
		out.Description = workerclient.NewOptString(*req.Description)
	}
	if req.ProviderInstanceID != "" {
		out.ProviderInstanceId = workerclient.NewOptString(req.ProviderInstanceID)
	}
	if req.AgentConfigID != nil {
		out.AgentConfigId = workerclient.NewOptString(*req.AgentConfigID)
	}
	if req.AgentModel != nil {
		out.AgentModel = workerclient.NewOptString(*req.AgentModel)
	}
	if req.AgentModelServiceTier != nil {
		out.AgentModelServiceTier = workerclient.NewOptString(*req.AgentModelServiceTier)
	}
	if req.AgentModelReasoningLevel != nil {
		out.AgentModelReasoningLevel = workerclient.NewOptString(*req.AgentModelReasoningLevel)
	}
	if req.Prompt != nil {
		out.Prompt = workerclient.NewOptString(*req.Prompt)
	}
	if req.SourceURL != "" {
		out.SourceUrl = workerclient.NewOptString(req.SourceURL)
	}
	if req.SourceRef != "" {
		out.SourceRef = workerclient.NewOptString(req.SourceRef)
	}
	if req.SourceRefType != "" {
		out.SourceRefType = workerclient.NewOptString(req.SourceRefType)
	}
	if req.SourceDirectory != "" {
		out.SourceDirectory = workerclient.NewOptString(req.SourceDirectory)
	}
	if req.SourceCodeReferences != nil {
		out.SourceCodeReferences = workerclient.NewOptWorkerSandboxCreateRequestSourceCodeReferences(workerSourceCodeReferences(req.SourceCodeReferences))
	}
	if req.UserUID != nil {
		out.UserUid = workerclient.NewOptInt64(int64(*req.UserUID))
	}
	if req.UserGID != nil {
		out.UserGid = workerclient.NewOptInt64(int64(*req.UserGID))
	}
	if req.WorkspacePath != "" {
		out.WorkspacePath = workerclient.NewOptString(req.WorkspacePath)
	}
	if req.WorkspaceSource != "" {
		out.WorkspaceSource = workerclient.NewOptString(req.WorkspaceSource)
	}
	if req.WorkspaceRef != "" {
		out.WorkspaceRef = workerclient.NewOptString(req.WorkspaceRef)
	}
	if req.WorkingDirectory != "" {
		out.WorkingDirectory = workerclient.NewOptString(req.WorkingDirectory)
	}
	if req.Resources != (sandbox.ResourceConfig{}) {
		out.Resources = workerclient.NewOptResourceConfig(workerResourceConfig(req.Resources))
	}
	if req.CPUVCPUs != 0 {
		out.CpuVcpus = workerclient.NewOptFloat64(req.CPUVCPUs)
	}
	if req.MemoryBytes != 0 {
		out.MemoryBytes = workerclient.NewOptInt64(req.MemoryBytes)
	}
	if req.StorageBytes != 0 {
		out.StorageBytes = workerclient.NewOptInt64(req.StorageBytes)
	}
	return out
}

func workerUpdateRequest(req workerSandboxUpdateRequest) *workerclient.WorkerSandboxUpdateRequest {
	out := &workerclient.WorkerSandboxUpdateRequest{}
	if req.Image != "" {
		out.Image = workerclient.NewOptString(req.Image)
	}
	if req.Env != nil {
		out.Env = workerclient.NewOptWorkerSandboxUpdateRequestEnv(workerclient.WorkerSandboxUpdateRequestEnv(req.Env))
	}
	if req.WorkingDirectory != "" {
		out.WorkingDirectory = workerclient.NewOptString(req.WorkingDirectory)
	}
	if req.Resources != (sandbox.ResourceConfig{}) {
		out.Resources = workerclient.NewOptResourceConfig(workerResourceConfig(req.Resources))
	}
	if req.CPUVCPUs != 0 {
		out.CpuVcpus = workerclient.NewOptFloat64(req.CPUVCPUs)
	}
	if req.MemoryBytes != 0 {
		out.MemoryBytes = workerclient.NewOptInt64(req.MemoryBytes)
	}
	if req.StorageBytes != 0 {
		out.StorageBytes = workerclient.NewOptInt64(req.StorageBytes)
	}
	return out
}

func workerOperationRequest(req workerSandboxOperationRequest) *workerclient.WorkerSandboxOperationRequest {
	out := &workerclient.WorkerSandboxOperationRequest{}
	if req.Force {
		out.Force = workerclient.NewOptBool(req.Force)
	}
	return out
}

func workerResourceConfig(cfg sandbox.ResourceConfig) workerclient.ResourceConfig {
	return workerclient.ResourceConfig{
		MemoryMB: int64(cfg.MemoryMB),
		CPUCores: cfg.CPUCores,
		DiskMB:   int64(cfg.DiskMB),
		Timeout:  int64(cfg.Timeout),
	}
}

func workerSourceCodeReferences(in model.SourceCodeReferences) workerclient.WorkerSandboxCreateRequestSourceCodeReferences {
	out := make(workerclient.WorkerSandboxCreateRequestSourceCodeReferences, len(in))
	for key, ref := range in {
		workerRef := workerclient.GitSourceReference{
			Directory: ref.Directory,
		}
		if ref.URL != "" {
			if parsed, err := url.Parse(ref.URL); err == nil {
				workerRef.URL = *parsed
			}
		}
		if ref.Ref != nil {
			workerRef.Ref = workerclient.NewOptString(*ref.Ref)
		}
		if ref.RefType != nil {
			workerRef.RefType = workerclient.NewOptString(*ref.RefType)
		}
		out[key] = workerRef
	}
	return out
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
		return huma.Error404NotFound(http.StatusText(http.StatusNotFound))
	}
	if errors.Is(err, sandbox.ErrAlreadyExists) {
		return huma.Error409Conflict(http.StatusText(http.StatusConflict))
	}
	return huma.Error500InternalServerError(err.Error())
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

func authorizedWorkerRequest(authorization string, tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
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
