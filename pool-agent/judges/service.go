// Package judges owns the dedicated pool harness and its request lifecycle.
package judges

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-faster/jx"

	api "github.com/discobox-ai/discobox/api/gen"
	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/judge"
	poolapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	"github.com/discobox-ai/discobox/pool-agent/internalhttp"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

type Service struct {
	runtime   sandboxruntime.Runtime
	fetch     func(context.Context) (*api.PoolJudgeRuntimeResponse, error)
	projectID string
	gate      chan struct{}
	slots     chan struct{}
}

func New(projectID string, runtime sandboxruntime.Runtime, fetch func(context.Context) (*api.PoolJudgeRuntimeResponse, error)) *Service {
	return &Service{runtime: runtime, fetch: fetch, projectID: projectID, slots: make(chan struct{}, 1), gate: make(chan struct{}, 1)}
}

func (s *Service) Run(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		refreshCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		if _, err := s.ensure(refreshCtx); err != nil && ctx.Err() == nil {
			logger.Debug("pool judge unavailable", "error", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) ensure(ctx context.Context) (*api.PoolJudgeRuntimeResponse, error) {
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	spec, err := s.fetch(ctx)
	if err != nil {
		return nil, err
	}
	// Retire all former judge revisions, including survivors of a pool-agent
	// restart. Work sandboxes use a different ID namespace.
	sandboxes, err := s.runtime.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	for _, sb := range sandboxes {
		if strings.HasPrefix(sb.SandboxID, "judge_") && sb.SandboxID != spec.SandboxId {
			if err := s.runtime.DeleteSandbox(ctx, sb.SandboxID); err != nil {
				return nil, err
			}
		}
	}
	req, err := createRequest(spec)
	if err != nil {
		return nil, err
	}
	if _, err = s.runtime.CreateSandbox(ctx, req); err != nil {
		return nil, err
	}
	if err = s.runtime.StartSandbox(ctx, spec.SandboxId, nil); err != nil {
		return nil, err
	}
	return spec, nil
}

func createRequest(spec *api.PoolJudgeRuntimeResponse) (*poolapi.PoolSandboxCreateRequest, error) {
	// These are the image-owned fields consumed by the runtime, with configured
	// files applied by sandboxconfig's existing merge. Never copy project sources.
	var encoder jx.Encoder
	spec.Harness.Encode(&encoder)
	data := encoder.Bytes()
	var err error
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	selected := map[string]json.RawMessage{}
	for _, name := range []string{"id", "name", "description", "runCommand", "relaunchCommand", "configCommand", "env", "files", "configuredFiles", "secrets", "volumes", "additionalGroups"} {
		if v, ok := fields[name]; ok {
			selected[name] = v
		}
	}
	for _, name := range []string{"files", "configuredFiles", "secrets"} {
		if _, ok := selected[name]; !ok {
			selected[name] = json.RawMessage(`[]`)
		}
	}
	data, err = json.Marshal(selected)
	if err != nil {
		return nil, err
	}
	var harness poolapi.ResolvedHarnessConfig
	if err = json.Unmarshal(data, &harness); err != nil {
		return nil, err
	}
	req := &poolapi.PoolSandboxCreateRequest{SandboxId: spec.SandboxId, ResolvedHarnessConfig: poolapi.NewOptResolvedHarnessConfig(harness)}
	req.Config.Image = poolapi.NewOptString(spec.Harness.Image.Or(""))
	req.Config.ImageDigest = poolapi.NewOptString(spec.Harness.ImageDigest.Or(""))
	req.Config.HarnessConfigId = poolapi.NewOptString(spec.Harness.ID)
	req.Config.HarnessMode = poolapi.NewOptSandboxConfigHarnessMode(poolapi.SandboxConfigHarnessModeJudge)
	req.Config.SpecFingerprint = poolapi.NewOptString(spec.Revision)
	req.Config.Start = poolapi.NewOptBool(true)
	req.SecretEnv = poolapi.NewOptNilPoolSandboxCreateRequestSecretEnv(poolapi.PoolSandboxCreateRequestSecretEnv(spec.SecretEnv))
	sentinels := make([]string, 0, len(spec.SecretEnv))
	for _, v := range spec.SecretEnv {
		sentinels = append(sentinels, v)
	}
	req.Sentinels = poolapi.NewOptNilStringArray(sentinels)
	return req, nil
}

func (s *Service) Judge(ctx context.Context, job judge.Job) (verdict judge.Verdict, resultErr error) {
	start := time.Now()
	prompt, _ := judge.Prompt(job)
	var selected *api.PoolJudgeRuntimeResponse
	defer func() {
		if resultErr != nil {
			verdict = judge.Verdict{Allow: false, Reason: "pool judge unavailable: " + resultErr.Error()}
		}
		verdict.Role, verdict.Prompt, verdict.PromptVersion = judge.Role, prompt, judge.PromptVersion
		verdict.LatencyMS = time.Since(start).Milliseconds()
		if selected != nil {
			verdict.HarnessConfigID, verdict.Revision = selected.Harness.ID, selected.Revision
			verdict.Image = selected.Harness.ImageDigest.Or(selected.Harness.Image.Or(""))
		}
	}()
	if err := job.Validate(); err != nil {
		return judge.Verdict{}, err
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return judge.Verdict{}, fmt.Errorf("pool judge is busy")
	}
	ctx, cancel := context.WithTimeout(ctx, judge.Timeout)
	defer cancel()
	spec, err := s.ensure(ctx)
	if err != nil {
		return judge.Verdict{}, err
	}
	selected = spec
	base, err := s.runtime.HTTPBaseURL(ctx, spec.SandboxId, sandboxruntime.SandboxAgentPort)
	if err != nil {
		return judge.Verdict{}, err
	}
	data, err := json.Marshal(job)
	if err != nil {
		return judge.Verdict{}, err
	}
	var input sandboxapi.JudgeJob
	if err = json.Unmarshal(data, &input); err != nil {
		return judge.Verdict{}, err
	}
	// The generated sandbox client uses a transport for the pool's narrow
	// judge-only token; it is scoped to this dedicated runtime identity.
	tokenClient := *internalhttp.Client
	tokenClient.Transport = &bearerTransport{base: internalhttp.Client.Transport, token: spec.Token}
	client, err := sandboxapi.NewClient(base.String(), sandboxapi.WithClient(&tokenClient))
	if err != nil {
		return judge.Verdict{}, err
	}
	result, err := client.JudgeSandbox(ctx, &input, sandboxapi.JudgeSandboxParams{ProjectId: s.projectID, SandboxId: spec.SandboxId})
	if err != nil {
		return judge.Verdict{}, err
	}
	data, err = json.Marshal(result)
	if err != nil {
		return judge.Verdict{}, err
	}
	if err = json.Unmarshal(data, &verdict); err != nil {
		return judge.Verdict{}, err
	}
	latest, err := s.fetch(ctx)
	if err != nil {
		return judge.Verdict{}, err
	}
	if latest.Revision != spec.Revision {
		return judge.Verdict{}, fmt.Errorf("judge configuration changed during verdict")
	}
	verdict.HarnessConfigID = spec.Harness.ID
	verdict.Revision = spec.Revision
	verdict.Image = spec.Harness.ImageDigest.Or(spec.Harness.Image.Or(""))
	return verdict, nil
}
