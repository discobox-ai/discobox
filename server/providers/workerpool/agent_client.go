package workerpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	workerclient "github.com/obot-platform/discobox/worker-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/worker-agent/api/model"
)

const defaultWorkerBaseURL = "https://worker"

// workerAgentClient adapts one worker's worker-agent API to the sandbox
// operations the pool needs. It is created per operation around a single
// worker-agent HTTP client lease; each call consumes and releases the lease.
type workerAgentClient struct {
	workerID    string
	tokenIssuer workerAgentTokenIssuer
	lease       *transport.HTTPClientLease
}

type workerAgentTokenIssuer interface {
	CreateWorkerAgentToken(ctx context.Context, claims workeragentauth.TokenClaims) (string, error)
	CreateSandboxAgentToken(ctx context.Context, claims workeragentauth.TokenClaims) (string, error)
}

func (p *workerAgentClient) Create(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	client, release, err := p.workerClient(ref, workeragentauth.ScopeSandboxWrite)
	if err != nil {
		return nil, state, err
	}
	defer release()
	workerSandbox, err := client.WorkerCreateSandbox(ctx, workerCreateRequestFromOptions(ref.SandboxID, opts), workerclient.WorkerCreateSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID})
	if err != nil {
		mapped := mapWorkerClientError(err)
		if errors.Is(mapped, sandbox.ErrAlreadyExists) {
			workerSandbox, err = client.WorkerGetSandbox(ctx, workerclient.WorkerGetSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID, SandboxId: ref.SandboxID})
			if err != nil {
				return nil, state, mapWorkerClientError(err)
			}
		} else {
			return nil, state, mapped
		}
	}
	runtimeSandbox := sandboxFromWorker(workerSandbox, p.workerID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

func (p *workerAgentClient) Update(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.UpdateOptions) (*sandbox.Sandbox, []byte, error) {
	client, release, err := p.workerClient(ref, workeragentauth.ScopeSandboxWrite)
	if err != nil {
		return nil, state, err
	}
	defer release()
	req := &workerapimodel.WorkerSandboxUpdateRequest{
		Sentinels: workerclient.NewOptNilStringArray(opts.Sentinels),
	}
	workerSandbox, err := client.WorkerUpdateSandbox(ctx, req, workerclient.WorkerUpdateSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, state, mapWorkerClientError(err)
	}
	runtimeSandbox := sandboxFromWorker(workerSandbox, p.workerID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

func (p *workerAgentClient) Start(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, []byte, error) {
	client, release, err := p.workerClient(ref, workeragentauth.ScopeSandboxWrite)
	if err != nil {
		return nil, state, err
	}
	defer release()
	workerSandbox, err := client.WorkerStartSandbox(ctx, &workerapimodel.WorkerSandboxOperationRequest{}, workerclient.WorkerStartSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, state, mapWorkerClientError(err)
	}
	runtimeSandbox := sandboxFromWorker(workerSandbox, p.workerID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

func (p *workerAgentClient) Stop(ctx context.Context, ref sandbox.SandboxRef, state []byte, _ time.Duration) (*sandbox.Sandbox, []byte, error) {
	client, release, err := p.workerClient(ref, workeragentauth.ScopeSandboxWrite)
	if err != nil {
		return nil, state, err
	}
	defer release()
	workerSandbox, err := client.WorkerStopSandbox(ctx, &workerapimodel.WorkerSandboxOperationRequest{}, workerclient.WorkerStopSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, state, mapWorkerClientError(err)
	}
	runtimeSandbox := sandboxFromWorker(workerSandbox, p.workerID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

func (p *workerAgentClient) Remove(ctx context.Context, ref sandbox.SandboxRef, state []byte, _ ...sandbox.RemoveOption) ([]byte, error) {
	client, release, err := p.workerClient(ref, workeragentauth.ScopeSandboxWrite)
	if err != nil {
		return state, err
	}
	defer release()
	if err := client.WorkerDeleteSandbox(ctx, workerclient.WorkerDeleteSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID, SandboxId: ref.SandboxID}); err != nil {
		return state, mapWorkerClientError(err)
	}
	return nil, nil
}

func (p *workerAgentClient) Get(ctx context.Context, ref sandbox.SandboxRef, _ []byte) (*sandbox.Sandbox, error) {
	client, release, err := p.workerClient(ref, workeragentauth.ScopeSandboxRead)
	if err != nil {
		return nil, err
	}
	defer release()
	workerSandbox, err := client.WorkerGetSandbox(ctx, workerclient.WorkerGetSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, mapWorkerClientError(err)
	}
	return sandboxFromWorker(workerSandbox, p.workerID), nil
}

// AcquireHTTPClient hands the lease to the caller with per-request token
// providers attached; the caller owns releasing it.
func (p *workerAgentClient) AcquireHTTPClient(_ context.Context, ref sandbox.SandboxRef, _ []byte, scopes []string) (*transport.HTTPClientLease, error) {
	lease := p.lease
	p.lease = nil
	if lease != nil && lease.AuthTokenProvider == nil {
		lease.AuthTokenProvider = p.authTokenProvider(ref, scopes...)
	}
	if lease != nil && lease.ForwardAuthTokenProvider == nil && requiresSandboxAgentToken(scopes) {
		lease.ForwardAuthTokenProvider = p.sandboxAgentAuthTokenProvider(ref, scopes...)
	}
	return lease, nil
}

func (p *workerAgentClient) workerClient(ref sandbox.SandboxRef, scopes ...string) (*workerclient.Client, func(), error) {
	lease := p.lease
	p.lease = nil
	if lease != nil && lease.AuthTokenProvider == nil {
		lease.AuthTokenProvider = p.authTokenProvider(ref, scopes...)
	}
	client, err := newWorkerAgentClient(lease)
	if err != nil {
		if lease != nil {
			lease.Release()
		}
		return nil, nil, err
	}
	return client, func() {
		if lease != nil {
			lease.Release()
		}
	}, nil
}

func (p *workerAgentClient) authTokenProvider(ref sandbox.SandboxRef, scopes ...string) func(context.Context) (string, error) {
	tokenScopes := append([]string(nil), scopes...)
	return func(ctx context.Context) (string, error) {
		if p.tokenIssuer == nil {
			return "", fmt.Errorf("worker-agent token issuer is required")
		}
		return p.tokenIssuer.CreateWorkerAgentToken(ctx, workeragentauth.TokenClaims{
			ProjectID: ref.ProjectID,
			WorkerID:  p.workerID,
			SandboxID: ref.SandboxID,
			Scopes:    tokenScopes,
		})
	}
}

func (p *workerAgentClient) sandboxAgentAuthTokenProvider(ref sandbox.SandboxRef, scopes ...string) func(context.Context) (string, error) {
	tokenScopes := append([]string(nil), scopes...)
	return func(ctx context.Context) (string, error) {
		if p.tokenIssuer == nil {
			return "", fmt.Errorf("worker-agent token issuer is required")
		}
		return p.tokenIssuer.CreateSandboxAgentToken(ctx, workeragentauth.TokenClaims{
			ProjectID: ref.ProjectID,
			WorkerID:  p.workerID,
			SandboxID: ref.SandboxID,
			Scopes:    tokenScopes,
		})
	}
}

func requiresSandboxAgentToken(scopes []string) bool {
	for _, scope := range scopes {
		switch scope {
		case workeragentauth.ScopeTerminalRead, workeragentauth.ScopeTerminalWrite, workeragentauth.ScopeExecRead, workeragentauth.ScopeExecWrite, "terminal:*", "exec:*", "*":
			return true
		}
	}
	return false
}

func newWorkerAgentClient(lease *transport.HTTPClientLease) (*workerclient.Client, error) {
	httpClient := http.DefaultClient
	baseURL := defaultWorkerBaseURL
	if lease != nil {
		if lease.Client != nil {
			httpClient = lease.Client
		}
		if strings.TrimSpace(lease.BaseURL) != "" {
			baseURL = lease.BaseURL
		}
	}
	return workerclient.NewClient(strings.TrimRight(baseURL, "/"), workerSecuritySource{lease: lease}, workerclient.WithClient(httpClient))
}

func workerOptStringArray(values []string) workerclient.OptNilStringArray {
	if len(values) == 0 {
		return workerclient.OptNilStringArray{}
	}
	return workerclient.NewOptNilStringArray(values)
}

func workerHarnessConfigFiles(files []model.HarnessConfigFile) workerclient.OptNilHarnessConfigFileArray {
	if len(files) == 0 {
		return workerclient.OptNilHarnessConfigFileArray{}
	}
	out := make([]workerapimodel.HarnessConfigFile, 0, len(files))
	for _, file := range files {
		out = append(out, workerapimodel.HarnessConfigFile{
			Path:       file.Path,
			Content:    file.Content,
			CreateOnly: workerclient.NewOptBool(file.CreateOnly),
		})
	}
	return workerclient.NewOptNilHarnessConfigFileArray(out)
}

func workerCreateRequestFromOptions(sandboxID string, opts sandbox.CreateOptions) *workerapimodel.WorkerSandboxCreateRequest {
	out := &workerapimodel.WorkerSandboxCreateRequest{SandboxId: sandboxID}
	config := &out.Config
	if opts.Image.Name != "" {
		config.Image = workerclient.NewOptString(opts.Image.Name)
	}
	if opts.Env != nil {
		config.Env = workerclient.NewOptSandboxConfigEnv(workerclient.SandboxConfigEnv(opts.Env))
	}
	if len(opts.Sentinels) > 0 {
		out.Sentinels = workerclient.NewOptNilStringArray(opts.Sentinels)
	}
	if opts.Name != "" {
		config.Name = workerclient.NewOptString(opts.Name)
	}
	if opts.Description != nil {
		config.Description = workerclient.NewOptString(*opts.Description)
	}
	if opts.HarnessConfigID != nil {
		config.HarnessConfigId = workerclient.NewOptString(*opts.HarnessConfigID)
	}
	if opts.ResolvedHarnessConfig != nil {
		resolved := workerapimodel.ResolvedHarnessConfig{
			ID:              opts.ResolvedHarnessConfig.ID,
			Name:            opts.ResolvedHarnessConfig.Name,
			InstallCommand:  workerOptStringArray(opts.ResolvedHarnessConfig.InstallCommand),
			RunCommand:      opts.ResolvedHarnessConfig.RunCommand,
			RelaunchCommand: workerOptStringArray(opts.ResolvedHarnessConfig.RelaunchCommand),
			Files:           workerHarnessConfigFiles(opts.ResolvedHarnessConfig.Files),
		}
		out.ResolvedHarnessConfig = workerclient.NewOptResolvedHarnessConfig(resolved)
	}
	if len(opts.HarnessConfigs) > 0 {
		configs := make([]workerapimodel.SandboxHarnessConfig, 0, len(opts.HarnessConfigs))
		for _, config := range opts.HarnessConfigs {
			configs = append(configs, workerapimodel.SandboxHarnessConfig{
				ID:              config.ID,
				Name:            config.Name,
				InstallCommand:  workerOptStringArray(config.InstallCommand),
				RunCommand:      config.RunCommand,
				RelaunchCommand: workerOptStringArray(config.RelaunchCommand),
				IsDefault:       config.IsDefault,
				Files:           workerHarnessConfigFiles(config.Files),
			})
		}
		out.HarnessConfigs = workerclient.NewOptNilSandboxHarnessConfigArray(configs)
	}
	if opts.Model != nil {
		config.Model = workerclient.NewOptString(*opts.Model)
	}
	if opts.ModelServiceTier != nil {
		config.ModelServiceTier = workerclient.NewOptString(*opts.ModelServiceTier)
	}
	if opts.ModelReasoningLevel != nil {
		config.ModelReasoningLevel = workerclient.NewOptString(*opts.ModelReasoningLevel)
	}
	if len(opts.Prompt) > 0 {
		config.Prompt = opts.Prompt
	}
	if opts.Source != nil {
		workerSource, err := workerGitSource(*opts.Source)
		if err == nil {
			config.Source = workerclient.NewOptGitSource(workerSource)
		}
	}
	if opts.SourceCodeReferences != nil {
		config.SourceCodeReferences = workerclient.NewOptSandboxConfigSourceCodeReferences(workerSourceCodeReferences(opts.SourceCodeReferences))
	}
	user := workerapimodel.SandboxUser{}
	user.SetName(workerOptStringPtr(opts.UserName))
	if opts.UserUID != nil {
		user.SetUID(workerclient.NewOptInt64(int64(*opts.UserUID)))
	}
	if opts.UserGID != nil {
		user.SetGid(workerclient.NewOptInt64(int64(*opts.UserGID)))
	}
	user.SetHomeDirectory(workerOptStringPtr(opts.HomeDirectory))
	if user.Name.Set || user.UID.Set || user.Gid.Set || user.HomeDirectory.Set {
		config.User = workerclient.NewOptSandboxUser(user)
	}
	if opts.Resources != (sandbox.ResourceConfig{}) {
		out.Resources = workerclient.NewOptWorkerSandboxResources(workerResourceConfig(opts.Resources))
	}
	if opts.CPUVCPUs != 0 {
		config.CpuVcpus = workerclient.NewOptFloat64(opts.CPUVCPUs)
	}
	if opts.MemoryBytes != 0 {
		config.MemoryBytes = workerclient.NewOptInt64(opts.MemoryBytes)
	}
	if opts.StorageBytes != 0 {
		config.StorageBytes = workerclient.NewOptInt64(opts.StorageBytes)
	}
	return out
}

func workerOptStringPtr(value *string) workerclient.OptString {
	if value == nil {
		return workerclient.OptString{}
	}
	return workerclient.NewOptString(*value)
}

func workerResourceConfig(cfg sandbox.ResourceConfig) workerapimodel.WorkerSandboxResources {
	return workerapimodel.WorkerSandboxResources{
		MemoryMb:       int64(cfg.MemoryMB),
		CpuCores:       cfg.CPUCores,
		DiskMb:         int64(cfg.DiskMB),
		TimeoutSeconds: int64(cfg.Timeout),
	}
}

func workerSourceCodeReferences(in model.SourceCodeReferences) workerclient.SandboxConfigSourceCodeReferences {
	out := make(workerclient.SandboxConfigSourceCodeReferences, len(in))
	for key, ref := range in {
		workerRef, err := workerGitSource(ref)
		if err != nil {
			continue
		}
		out[key] = workerRef
	}
	return out
}

func workerGitSource(in model.GitSource) (workerapimodel.GitSource, error) {
	out := workerapimodel.GitSource{Kind: workerclient.GitSourceKind(in.Kind)}
	if out.Kind == "" {
		out.Kind = workerclient.GitSourceKindGit
	}
	if in.URL != nil && *in.URL != "" {
		parsed, err := url.Parse(*in.URL)
		if err != nil {
			return out, err
		}
		out.URL = workerclient.NewOptURI(*parsed)
	}
	if in.LocalDirectory != nil {
		out.LocalDirectory = workerclient.NewOptString(*in.LocalDirectory)
	}
	if in.Checkout != nil {
		checkout := workerapimodel.GitSourceCheckout{}
		if in.Checkout.Commit != nil {
			checkout.Commit = workerclient.NewOptString(*in.Checkout.Commit)
		}
		if in.Checkout.RefName != nil {
			checkout.RefName = workerclient.NewOptString(*in.Checkout.RefName)
		}
		if in.Checkout.RefType != nil {
			checkout.RefType = workerclient.NewOptString(*in.Checkout.RefType)
		}
		out.Checkout = workerclient.NewOptGitSourceCheckout(checkout)
	}
	if in.Workspace != nil {
		workspace := workerapimodel.GitSourceWorkspace{}
		if in.Workspace.Mode != "" {
			workspace.Mode = workerclient.NewOptGitSourceWorkspaceMode(workerclient.GitSourceWorkspaceMode(in.Workspace.Mode))
		}
		if in.Workspace.SnapshotRef != nil {
			workspace.SnapshotRef = workerclient.NewOptString(*in.Workspace.SnapshotRef)
		}
		if in.Workspace.BaseCommit != nil {
			workspace.BaseCommit = workerclient.NewOptString(*in.Workspace.BaseCommit)
		}
		out.Workspace = workerclient.NewOptGitSourceWorkspace(workspace)
	}
	if in.Destination != nil {
		destination := workerapimodel.GitSourceDestination{}
		if in.Destination.Directory != nil {
			destination.Directory = workerclient.NewOptString(*in.Destination.Directory)
		}
		if in.Destination.WorkingDirectory != nil {
			destination.WorkingDirectory = workerclient.NewOptString(*in.Destination.WorkingDirectory)
		}
		out.Destination = workerclient.NewOptGitSourceDestination(destination)
	}
	return out, nil
}

func sandboxFromWorker(in *workerapimodel.WorkerSandboxInstance, workerID string) *sandbox.Sandbox {
	if in == nil {
		return nil
	}
	runtime := in.Runtime
	metadata := map[string]string(runtime.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata["worker_id"] = workerID
	return &sandbox.Sandbox{
		ID:        runtime.InstanceId,
		SandboxID: in.SandboxId,
		Status:    sandbox.Status(runtime.Status),
		Image:     runtime.Image,
		CreatedAt: runtime.CreatedAt,
		StartedAt: timePtrFromWorker(runtime.StartedAt),
		StoppedAt: timePtrFromWorker(runtime.StoppedAt),
		Error:     runtime.Error,
		Metadata:  metadata,
		Ports:     portsFromWorker(runtime.Ports),
		Env:       map[string]string(runtime.Env),
	}
}

func timePtrFromWorker(in workerclient.NilDateTime) *time.Time {
	if in.Null {
		return nil
	}
	return &in.Value
}

func portsFromWorker(in []workerapimodel.WorkerSandboxPort) []sandbox.AssignedPort {
	if in == nil {
		return nil
	}
	out := make([]sandbox.AssignedPort, 0, len(in))
	for _, port := range in {
		out = append(out, sandbox.AssignedPort{
			ContainerPort: int(port.ContainerPort),
			HostPort:      int(port.HostPort),
			HostIP:        port.HostIp,
			Protocol:      port.Protocol,
		})
	}
	return out
}

func mapWorkerClientError(err error) error {
	if err == nil {
		return nil
	}
	var statusErr *workerclient.ErrorModelStatusCode
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		return sandbox.ErrNotFound
	}
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusConflict {
		return sandbox.ErrAlreadyExists
	}
	if errors.As(err, &statusErr) {
		return fmt.Errorf("worker-agent request failed: %s", workerClientErrorMessage(statusErr))
	}
	return err
}

func workerClientErrorMessage(statusErr *workerclient.ErrorModelStatusCode) string {
	if statusErr == nil {
		return ""
	}
	if detail, ok := statusErr.Response.Detail.Get(); ok && strings.TrimSpace(detail) != "" {
		return strings.TrimSpace(detail)
	}
	if title, ok := statusErr.Response.Title.Get(); ok && strings.TrimSpace(title) != "" {
		if statusErr.StatusCode != 0 {
			return fmt.Sprintf("status %d: %s", statusErr.StatusCode, strings.TrimSpace(title))
		}
		return strings.TrimSpace(title)
	}
	if statusErr.StatusCode != 0 {
		return fmt.Sprintf("status %d", statusErr.StatusCode)
	}
	return statusErr.Error()
}

type workerSecuritySource struct {
	lease *transport.HTTPClientLease
}

func (s workerSecuritySource) WorkerBearerAuth(ctx context.Context, _ workerclient.OperationName) (workerclient.WorkerBearerAuth, error) {
	token, err := s.lease.AuthorizationToken(ctx)
	if err != nil {
		return workerclient.WorkerBearerAuth{}, err
	}
	return workerclient.WorkerBearerAuth{Token: strings.TrimSpace(token)}, nil
}
