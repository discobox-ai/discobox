package poolruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/harness"
	poolclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	poolapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

const defaultPoolBaseURL = "https://pool"

// poolAgentClient adapts one worker's pool-agent API to the sandbox
// operations the pool needs. It is created per operation around a single
// pool-agent HTTP client lease; each call consumes and releases the lease.
type poolAgentClient struct {
	poolID      string
	tokenIssuer poolAgentTokenIssuer
	lease       *transport.HTTPClientLease
}

type poolAgentTokenIssuer interface {
	CreateAgentToken(ctx context.Context, claims poolagentauth.TokenClaims) (string, error)
	CreateSandboxAgentToken(ctx context.Context, claims poolagentauth.TokenClaims) (string, error)
}

func (p *poolAgentClient) Create(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	client, release, err := p.poolClient(ref, poolagentauth.ScopeSandboxWrite)
	if err != nil {
		return nil, state, err
	}
	defer release()
	poolSandbox, err := client.PoolCreateSandbox(ctx, poolCreateRequestFromOptions(ref.SandboxID, opts), poolclient.PoolCreateSandboxParams{ProjectId: ref.ProjectID, PoolId: p.poolID})
	if err != nil {
		mapped := mapPoolClientError(err)
		if errors.Is(mapped, sandbox.ErrAlreadyExists) {
			poolSandbox, err = client.PoolGetSandbox(ctx, poolclient.PoolGetSandboxParams{ProjectId: ref.ProjectID, PoolId: p.poolID, SandboxId: ref.SandboxID})
			if err != nil {
				return nil, state, mapPoolClientError(err)
			}
		} else {
			return nil, state, mapped
		}
	}
	runtimeSandbox := sandboxFromPool(poolSandbox, p.poolID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

// SyncKnownPools tells the pool-agent the authoritative set of pools this host
// should have, so it can reap the agent-created footprint of any other pool it
// observes on a shared daemon. It is a no-op on backends where each pool has its
// own isolated daemon (the agent simply sees no other pools).
func (p *poolAgentClient) SyncKnownPools(ctx context.Context, projectID string, knownPoolIDs []string) error {
	client, release, err := p.poolClient(sandbox.SandboxRef{ProjectID: projectID}, poolagentauth.ScopePoolSync)
	if err != nil {
		return err
	}
	defer release()
	if err := client.PoolSync(ctx, &poolapimodel.PoolSyncRequest{KnownPoolIds: knownPoolIDs}, poolclient.PoolSyncParams{ProjectId: projectID, PoolId: p.poolID}); err != nil {
		return mapPoolClientError(err)
	}
	return nil
}

func (p *poolAgentClient) Update(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.UpdateOptions) (*sandbox.Sandbox, []byte, error) {
	client, release, err := p.poolClient(ref, poolagentauth.ScopeSandboxWrite)
	if err != nil {
		return nil, state, err
	}
	defer release()
	req := &poolapimodel.PoolSandboxUpdateRequest{
		Sentinels: poolclient.NewOptNilStringArray(opts.Sentinels),
	}
	if len(opts.SecretEnv) > 0 {
		req.SecretEnv = poolclient.NewOptNilPoolSandboxUpdateRequestSecretEnv(poolclient.PoolSandboxUpdateRequestSecretEnv(opts.SecretEnv))
	}
	poolSandbox, err := client.PoolUpdateSandbox(ctx, req, poolclient.PoolUpdateSandboxParams{ProjectId: ref.ProjectID, PoolId: p.poolID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, state, mapPoolClientError(err)
	}
	runtimeSandbox := sandboxFromPool(poolSandbox, p.poolID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

// Start, Stop, and Restart deliver an instruction and return only the sealed
// state, because the pool agent answers them with acceptance rather than a
// sandbox: what became of the instruction arrives on its state-reporting
// channel (ADR 0017 §§9–10).
func (p *poolAgentClient) Start(ctx context.Context, ref sandbox.SandboxRef, state []byte) ([]byte, error) {
	return p.instruct(ctx, ref, state, func(ctx context.Context, client *poolclient.Client) error {
		_, err := client.PoolStartSandbox(ctx, &poolapimodel.PoolSandboxOperationRequest{}, poolclient.PoolStartSandboxParams{ProjectId: ref.ProjectID, PoolId: p.poolID, SandboxId: ref.SandboxID})
		return err
	})
}

func (p *poolAgentClient) Stop(ctx context.Context, ref sandbox.SandboxRef, state []byte, _ time.Duration) ([]byte, error) {
	return p.instruct(ctx, ref, state, func(ctx context.Context, client *poolclient.Client) error {
		_, err := client.PoolStopSandbox(ctx, &poolapimodel.PoolSandboxOperationRequest{}, poolclient.PoolStopSandboxParams{ProjectId: ref.ProjectID, PoolId: p.poolID, SandboxId: ref.SandboxID})
		return err
	})
}

func (p *poolAgentClient) Restart(ctx context.Context, ref sandbox.SandboxRef, state []byte, _ time.Duration) ([]byte, error) {
	return p.instruct(ctx, ref, state, func(ctx context.Context, client *poolclient.Client) error {
		_, err := client.PoolRestartSandbox(ctx, &poolapimodel.PoolSandboxOperationRequest{}, poolclient.PoolRestartSandboxParams{ProjectId: ref.ProjectID, PoolId: p.poolID, SandboxId: ref.SandboxID})
		return err
	})
}

func (p *poolAgentClient) instruct(ctx context.Context, ref sandbox.SandboxRef, state []byte, call func(context.Context, *poolclient.Client) error) ([]byte, error) {
	client, release, err := p.poolClient(ref, poolagentauth.ScopeSandboxWrite)
	if err != nil {
		return state, err
	}
	defer release()
	if err := call(ctx, client); err != nil {
		return state, mapPoolClientError(err)
	}
	return state, nil
}

// Archive keeps the runtime state: the sandbox still belongs to this pool, and
// unarchiving must reach the same agent to find its data.
func (p *poolAgentClient) Archive(ctx context.Context, ref sandbox.SandboxRef, state []byte) ([]byte, error) {
	client, release, err := p.poolClient(ref, poolagentauth.ScopeSandboxWrite)
	if err != nil {
		return state, err
	}
	defer release()
	if err := client.PoolArchiveSandbox(ctx, poolclient.PoolArchiveSandboxParams{ProjectId: ref.ProjectID, PoolId: p.poolID, SandboxId: ref.SandboxID}); err != nil {
		return state, mapPoolClientError(err)
	}
	return state, nil
}

// Remove returns nil state: the sandbox no longer exists anywhere, so there is
// no pool binding left to remember. The agent answers only once the container
// and the durable tree are both gone (ADR 0022 §3), so a nil error here is the
// confirmation the reconciler deletes the row on.
func (p *poolAgentClient) Remove(ctx context.Context, ref sandbox.SandboxRef, state []byte) ([]byte, error) {
	client, release, err := p.poolClient(ref, poolagentauth.ScopeSandboxWrite)
	if err != nil {
		return state, err
	}
	defer release()
	if err := client.PoolDeleteSandbox(ctx, poolclient.PoolDeleteSandboxParams{ProjectId: ref.ProjectID, PoolId: p.poolID, SandboxId: ref.SandboxID}); err != nil {
		return state, mapPoolClientError(err)
	}
	return nil, nil
}

func (p *poolAgentClient) Get(ctx context.Context, ref sandbox.SandboxRef, _ []byte) (*sandbox.Sandbox, error) {
	client, release, err := p.poolClient(ref, poolagentauth.ScopeSandboxRead)
	if err != nil {
		return nil, err
	}
	defer release()
	poolSandbox, err := client.PoolGetSandbox(ctx, poolclient.PoolGetSandboxParams{ProjectId: ref.ProjectID, PoolId: p.poolID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, mapPoolClientError(err)
	}
	return sandboxFromPool(poolSandbox, p.poolID), nil
}

// AcquireHTTPClient hands the lease to the caller with per-request token
// providers attached; the caller owns releasing it.
func (p *poolAgentClient) AcquireHTTPClient(_ context.Context, ref sandbox.SandboxRef, _ []byte, scopes []string) (*transport.HTTPClientLease, error) {
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

func (p *poolAgentClient) poolClient(ref sandbox.SandboxRef, scopes ...string) (*poolclient.Client, func(), error) {
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

func (p *poolAgentClient) authTokenProvider(ref sandbox.SandboxRef, scopes ...string) func(context.Context) (string, error) {
	tokenScopes := append([]string(nil), scopes...)
	return func(ctx context.Context) (string, error) {
		if p.tokenIssuer == nil {
			return "", fmt.Errorf("pool-agent token issuer is required")
		}
		return p.tokenIssuer.CreateAgentToken(ctx, poolagentauth.TokenClaims{
			ProjectID: ref.ProjectID,
			PoolID:    p.poolID,
			SandboxID: ref.SandboxID,
			Scopes:    tokenScopes,
		})
	}
}

func (p *poolAgentClient) sandboxAgentAuthTokenProvider(ref sandbox.SandboxRef, scopes ...string) func(context.Context) (string, error) {
	tokenScopes := append([]string(nil), scopes...)
	return func(ctx context.Context) (string, error) {
		if p.tokenIssuer == nil {
			return "", fmt.Errorf("pool-agent token issuer is required")
		}
		return p.tokenIssuer.CreateSandboxAgentToken(ctx, poolagentauth.TokenClaims{
			ProjectID: ref.ProjectID,
			PoolID:    p.poolID,
			SandboxID: ref.SandboxID,
			Scopes:    tokenScopes,
		})
	}
}

func requiresSandboxAgentToken(scopes []string) bool {
	for _, scope := range scopes {
		switch scope {
		case poolagentauth.ScopeTerminalRead, poolagentauth.ScopeTerminalWrite, poolagentauth.ScopeExecRead, poolagentauth.ScopeExecWrite, poolagentauth.ScopeTCPConnect, "terminal:*", "exec:*", "tcp:*", "*":
			return true
		}
	}
	return false
}

func newWorkerAgentClient(lease *transport.HTTPClientLease) (*poolclient.Client, error) {
	httpClient := http.DefaultClient
	baseURL := defaultPoolBaseURL
	if lease != nil {
		if lease.Client != nil {
			httpClient = lease.Client
		}
		if strings.TrimSpace(lease.BaseURL) != "" {
			baseURL = lease.BaseURL
		}
	}
	return poolclient.NewClient(strings.TrimRight(baseURL, "/"), workerSecuritySource{lease: lease}, poolclient.WithClient(httpClient))
}

// poolHarnessConfigSecrets forwards the harness's declared credentials. Only
// the declaration crosses here -- never a value: what the sandbox needs is
// Delivery, which says whether the sentinel is exported as an environment
// variable or withheld because the harness reads it from a file.
func poolHarnessConfigSecrets(secrets []model.HarnessConfigSecret) poolclient.OptNilHarnessSecretArray {
	if len(secrets) == 0 {
		return poolclient.OptNilHarnessSecretArray{}
	}
	out := make([]poolapimodel.HarnessSecret, 0, len(secrets))
	for _, secret := range secrets {
		entry := poolapimodel.HarnessSecret{
			Name:     secret.Name,
			Required: poolclient.NewOptBool(secret.Required),
		}
		if secret.OneOfGroup != "" {
			entry.OneOfGroup = poolclient.NewOptString(secret.OneOfGroup)
		}
		if secret.Delivery != "" {
			entry.Delivery = poolclient.NewOptString(secret.Delivery)
		}
		out = append(out, entry)
	}
	return poolclient.NewOptNilHarnessSecretArray(out)
}

func poolHarnessConfigFiles(files []model.HarnessConfigFile) poolclient.OptNilHarnessConfigFileArray {
	if len(files) == 0 {
		return poolclient.OptNilHarnessConfigFileArray{}
	}
	out := make([]poolapimodel.HarnessConfigFile, 0, len(files))
	for _, file := range files {
		out = append(out, poolapimodel.HarnessConfigFile{
			Path:       file.Path,
			Content:    file.Content,
			CreateOnly: poolclient.NewOptBool(file.CreateOnly),
			Template:   poolclient.NewOptBool(file.Template),
		})
	}
	return poolclient.NewOptNilHarnessConfigFileArray(out)
}

func poolHarnessVolumes(volumes []harness.Volume) []poolapimodel.HarnessVolume {
	if len(volumes) == 0 {
		return nil
	}
	out := make([]poolapimodel.HarnessVolume, 0, len(volumes))
	for _, v := range volumes {
		volume := poolapimodel.HarnessVolume{
			Path:   v.Path,
			Volume: poolclient.HarnessVolumeVolume(v.Volume),
		}
		if uid := string(v.UID); uid != "" {
			volume.UID = poolclient.NewOptString(uid)
		}
		if gid := string(v.GID); gid != "" {
			volume.Gid = poolclient.NewOptString(gid)
		}
		if v.Mode != "" {
			volume.Mode = poolclient.NewOptString(v.Mode)
		}
		out = append(out, volume)
	}
	return out
}

func poolCreateRequestFromOptions(sandboxID string, opts sandbox.CreateOptions) *poolapimodel.PoolSandboxCreateRequest {
	out := &poolapimodel.PoolSandboxCreateRequest{SandboxId: sandboxID}
	config := &out.Config
	if opts.Image.Name != "" {
		config.Image = poolclient.NewOptString(opts.Image.Name)
	}
	if opts.Image.Digest != "" {
		config.ImageDigest = poolclient.NewOptString(opts.Image.Digest)
	}
	if opts.SpecFingerprint != "" {
		config.SpecFingerprint = poolclient.NewOptString(opts.SpecFingerprint)
	}
	config.Start = poolclient.NewOptBool(opts.Start)
	if opts.Env != nil {
		config.Env = poolclient.NewOptSandboxConfigEnv(poolclient.SandboxConfigEnv(opts.Env))
	}
	if len(opts.Sentinels) > 0 {
		out.Sentinels = poolclient.NewOptNilStringArray(opts.Sentinels)
	}
	if len(opts.SecretEnv) > 0 {
		out.SecretEnv = poolclient.NewOptNilPoolSandboxCreateRequestSecretEnv(poolclient.PoolSandboxCreateRequestSecretEnv(opts.SecretEnv))
	}
	if opts.Name != "" {
		config.Name = poolclient.NewOptString(opts.Name)
	}
	if opts.Description != nil {
		config.Description = poolclient.NewOptString(*opts.Description)
	}
	if opts.HarnessConfigID != nil {
		config.HarnessConfigId = poolclient.NewOptString(*opts.HarnessConfigID)
	}
	if opts.HarnessMode != "" {
		config.HarnessMode = poolclient.NewOptSandboxConfigHarnessMode(poolclient.SandboxConfigHarnessMode(opts.HarnessMode))
	}
	if opts.ResolvedHarnessConfig != nil {
		rhc := opts.ResolvedHarnessConfig
		resolved := poolapimodel.ResolvedHarnessConfig{
			ID: rhc.ID, Name: rhc.Name,
			Files:           poolHarnessConfigFiles(rhc.Files),
			ConfiguredFiles: poolHarnessConfigFiles(rhc.ConfiguredFiles),
			Secrets:         poolHarnessConfigSecrets(rhc.Secrets),
		}
		if rhc.Description != "" {
			resolved.Description = poolclient.NewOptString(rhc.Description)
		}
		if len(rhc.RunCommand) > 0 {
			resolved.RunCommand = poolclient.NewOptNilStringArray(rhc.RunCommand)
		}
		if len(rhc.RelaunchCommand) > 0 {
			resolved.RelaunchCommand = poolclient.NewOptNilStringArray(rhc.RelaunchCommand)
		}
		if len(rhc.ConfigCommand) > 0 {
			resolved.ConfigCommand = poolclient.NewOptNilStringArray(rhc.ConfigCommand)
		}
		if len(rhc.Env) > 0 {
			resolved.Env = poolclient.NewOptNilResolvedHarnessConfigEnv(poolclient.ResolvedHarnessConfigEnv(rhc.Env))
		}
		if volumes := poolHarnessVolumes(rhc.Volumes); len(volumes) > 0 {
			resolved.Volumes = poolclient.NewOptNilHarnessVolumeArray(volumes)
		}
		if len(rhc.AdditionalGroups) > 0 {
			resolved.AdditionalGroups = poolclient.NewOptNilStringArray(rhc.AdditionalGroups)
		}
		out.ResolvedHarnessConfig = poolclient.NewOptResolvedHarnessConfig(resolved)
	}
	if opts.Model != nil {
		config.Model = poolclient.NewOptString(*opts.Model)
	}
	if opts.ModelServiceTier != nil {
		config.ModelServiceTier = poolclient.NewOptString(*opts.ModelServiceTier)
	}
	if opts.ModelReasoningLevel != nil {
		config.ModelReasoningLevel = poolclient.NewOptString(*opts.ModelReasoningLevel)
	}
	if len(opts.Prompt) > 0 {
		config.Prompt = opts.Prompt
	}
	if opts.Source != nil {
		workerSource, err := poolGitSource(*opts.Source, opts.SourceDataKey)
		if err == nil {
			config.Source = poolclient.NewOptGitSource(workerSource)
		}
	}
	if opts.SourceCodeReferences != nil {
		config.SourceCodeReferences = poolclient.NewOptSandboxConfigSourceCodeReferences(poolSourceCodeReferences(opts.SourceCodeReferences, opts.SourceCodeReferenceDataKeys))
	}
	user := poolapimodel.SandboxUser{}
	user.SetName(poolOptStringPtr(opts.UserName))
	if opts.UserUID != nil {
		user.SetUID(poolclient.NewOptInt64(int64(*opts.UserUID)))
	}
	if opts.UserGID != nil {
		user.SetGid(poolclient.NewOptInt64(int64(*opts.UserGID)))
	}
	user.SetGroupName(poolOptStringPtr(opts.UserGroupName))
	if len(opts.UserAdditionalGroups) > 0 {
		user.SetAdditionalGroups(append([]string(nil), opts.UserAdditionalGroups...))
	}
	user.SetHomeDirectory(poolOptStringPtr(opts.HomeDirectory))
	if user.Name.Set || user.UID.Set || user.Gid.Set || user.GroupName.Set || user.HomeDirectory.Set || len(user.AdditionalGroups) > 0 {
		config.User = poolclient.NewOptSandboxUser(user)
	}
	git := poolapimodel.SandboxGitIdentity{}
	git.SetUserName(poolOptStringPtr(opts.GitUserName))
	git.SetUserEmail(poolOptStringPtr(opts.GitUserEmail))
	if git.UserName.Set || git.UserEmail.Set {
		config.Git = poolclient.NewOptSandboxGitIdentity(git)
	}
	return out
}

func poolOptStringPtr(value *string) poolclient.OptString {
	if value == nil {
		return poolclient.OptString{}
	}
	return poolclient.NewOptString(*value)
}

func poolSourceCodeReferences(in model.SourceCodeReferences, dataKeys map[string]string) poolclient.SandboxConfigSourceCodeReferences {
	out := make(poolclient.SandboxConfigSourceCodeReferences, len(in))
	for key, ref := range in {
		workerRef, err := poolGitSource(ref, dataKeys[key])
		if err != nil {
			continue
		}
		out[key] = workerRef
	}
	return out
}

func poolGitSource(in model.GitSource, dataKey string) (poolapimodel.GitSource, error) {
	out := poolapimodel.GitSource{Kind: poolclient.GitSourceKind(in.Kind)}
	if dataKey != "" {
		out.DataKey = poolclient.NewOptString(dataKey)
	}
	if out.Kind == "" {
		out.Kind = poolclient.GitSourceKindGit
	}
	// A push-delivered source names a repository this worker cannot reach. Its
	// URL and directory are deliberately not forwarded, so the worker cannot
	// try; the client pushes the commits in instead.
	push := in.Delivery == model.GitSourceDeliveryPush
	if push {
		out.Delivery = poolclient.NewOptGitSourceDelivery(poolclient.GitSourceDeliveryPush)
	} else {
		if in.URL != nil && *in.URL != "" {
			parsed, err := url.Parse(*in.URL)
			if err != nil {
				return out, err
			}
			out.URL = poolclient.NewOptURI(*parsed)
		}
		if in.LocalDirectory != nil {
			out.LocalDirectory = poolclient.NewOptString(*in.LocalDirectory)
		}
	}
	if in.Checkout != nil {
		checkout := poolapimodel.GitSourceCheckout{}
		if in.Checkout.Commit != nil {
			checkout.Commit = poolclient.NewOptString(*in.Checkout.Commit)
		}
		if in.Checkout.RefName != nil {
			checkout.RefName = poolclient.NewOptString(*in.Checkout.RefName)
		}
		if in.Checkout.RefType != nil {
			checkout.RefType = poolclient.NewOptString(*in.Checkout.RefType)
		}
		out.Checkout = poolclient.NewOptGitSourceCheckout(checkout)
	}
	// A dirty workspace still has to be restored on the push path: its semantics
	// are the base commit checked out with uncommitted changes on top, which the
	// snapshot ref describes. Only the fetch differs — the client pushes the
	// snapshot ref in, so the worker already has the objects.
	if in.Workspace != nil {
		workspace := poolapimodel.GitSourceWorkspace{}
		if in.Workspace.Mode != "" {
			workspace.Mode = poolclient.NewOptGitSourceWorkspaceMode(poolclient.GitSourceWorkspaceMode(in.Workspace.Mode))
		}
		if in.Workspace.SnapshotRef != nil {
			workspace.SnapshotRef = poolclient.NewOptString(*in.Workspace.SnapshotRef)
		}
		if in.Workspace.BaseCommit != nil {
			workspace.BaseCommit = poolclient.NewOptString(*in.Workspace.BaseCommit)
		}
		out.Workspace = poolclient.NewOptGitSourceWorkspace(workspace)
	}
	if in.Destination != nil {
		destination := poolapimodel.GitSourceDestination{}
		if in.Destination.Directory != nil {
			destination.Directory = poolclient.NewOptString(*in.Destination.Directory)
		}
		if in.Destination.WorkingDirectory != nil {
			destination.WorkingDirectory = poolclient.NewOptString(*in.Destination.WorkingDirectory)
		}
		out.Destination = poolclient.NewOptGitSourceDestination(destination)
	}
	return out, nil
}

func sandboxFromPool(in *poolapimodel.PoolSandboxInstance, poolID string) *sandbox.Sandbox {
	if in == nil {
		return nil
	}
	runtime := in.Runtime
	metadata := map[string]string(runtime.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata["pool_id"] = poolID
	return &sandbox.Sandbox{
		ID:        runtime.InstanceId,
		SandboxID: in.SandboxId,
		Status:    sandbox.Status(runtime.Status),
		Image:     runtime.Image,
		CreatedAt: runtime.CreatedAt,
		StartedAt: timePtrFromPool(runtime.StartedAt),
		StoppedAt: timePtrFromPool(runtime.StoppedAt),
		Error:     runtime.Error,
		Metadata:  metadata,
		Ports:     portsFromPool(runtime.Ports),
		Env:       map[string]string(runtime.Env),
	}
}

func timePtrFromPool(in poolclient.NilDateTime) *time.Time {
	if in.Null {
		return nil
	}
	return &in.Value
}

func portsFromPool(in []poolapimodel.PoolSandboxPort) []sandbox.AssignedPort {
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

func mapPoolClientError(err error) error {
	if err == nil {
		return nil
	}
	var statusErr *poolclient.ErrorModelStatusCode
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		return sandbox.ErrNotFound
	}
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusConflict {
		return sandbox.ErrAlreadyExists
	}
	if errors.As(err, &statusErr) {
		return fmt.Errorf("pool-agent request failed: %s", poolClientErrorMessage(statusErr))
	}
	return err
}

func poolClientErrorMessage(statusErr *poolclient.ErrorModelStatusCode) string {
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

func (s workerSecuritySource) PoolBearerAuth(ctx context.Context, _ poolclient.OperationName) (poolclient.PoolBearerAuth, error) {
	token, err := s.lease.AuthorizationToken(ctx)
	if err != nil {
		return poolclient.PoolBearerAuth{}, err
	}
	return poolclient.PoolBearerAuth{Token: strings.TrimSpace(token)}, nil
}
