package pools

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func (s *Service) GetPoolJudgeRuntime(ctx context.Context, poolID string) (*apimodel.PoolJudgeRuntimeResponse, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool || principal.PoolID != poolID {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "pool identity required")
	}
	pool, err := s.store.GetPoolByID(ctx, poolID)
	if err != nil {
		return nil, err
	}
	project, err := s.store.GetProject(ctx, pool.ProjectID)
	if err != nil {
		return nil, err
	}
	selected := project.JudgeHarnessConfigID
	if selected == "" {
		selected = project.DefaultHarnessConfigID
	}
	if selected == "" {
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "select a default or judge harness")
	}
	hc, err := s.store.GetHarnessConfig(ctx, project.ID, selected)
	if err != nil {
		return nil, err
	}
	if !hc.Configured || hc.Slug == "shell" {
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "judge requires a configured prompting harness")
	}
	bindings, err := s.store.ListHarnessConfigSecretBindings(ctx, project.ID, selected)
	if err != nil {
		return nil, err
	}
	revision, err := model.JudgeRevision(hc, bindings)
	if err != nil {
		return nil, err
	}
	identity := sha256.Sum256([]byte(poolID + ":" + revision))
	row := model.PoolJudge{PoolID: poolID, ProjectID: project.ID, SandboxID: fmt.Sprintf("judge_%x", identity[:12]), HarnessConfigID: selected, Revision: revision}
	env, err := s.store.EnsurePoolJudge(ctx, row, bindings)
	if err != nil {
		return nil, err
	}
	token, err := s.pools.CreateSandboxAgentToken(ctx, poolagentauth.TokenClaims{ProjectID: project.ID, PoolID: poolID, SandboxID: row.SandboxID, Scopes: []string{judge.Scope}})
	if err != nil {
		return nil, err
	}
	converted, err := services.Convert[apimodel.HarnessConfig](hc)
	if err != nil {
		return nil, err
	}
	return &apimodel.PoolJudgeRuntimeResponse{SandboxId: row.SandboxID, Revision: revision, Harness: converted, SecretEnv: env, Token: token}, nil
}
