package handlers

import (
	"context"
	"net/http"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
)

func (h *Handler) GetPoolJudgeRuntime(ctx context.Context, params serverapi.GetPoolJudgeRuntimeParams) (serverapi.GetPoolJudgeRuntimeRes, error) {
	out, err := h.services.Pools.GetPoolJudgeRuntime(ctx, params.PoolId)
	if err != nil {
		return apiError(err), nil
	}
	return out, nil
}

// JudgeSandbox refuses public control-plane dispatch. Jobs are served exclusively by the dedicated sandbox-agent. The
// public control-plane API must not turn this into a general prompting route.
func (h *Handler) JudgeSandbox(context.Context, *apimodel.JudgeJob, serverapi.JudgeSandboxParams) (serverapi.JudgeSandboxRes, error) {
	return nil, apperrors.NewStatusError(http.StatusForbidden, "judge is pool-private")
}
