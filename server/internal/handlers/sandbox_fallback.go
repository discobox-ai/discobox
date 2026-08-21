package handlers

import (
	"context"
	"log/slog"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// fallbackHarnessConfig is the project's reserved `shell` config, which a
// sandbox with no harness config of its own upgrades to (ADR 0025 §4). The
// sandbox mappers need it to report that upgrade.
//
// A lookup failure is logged and treated as absent rather than failing the
// request: this decorates a sandbox with an available upgrade, and losing that
// decoration is a much smaller harm than refusing to return the sandbox.
func (h *Handler) fallbackHarnessConfig(ctx context.Context, projectID string) *model.HarnessConfig {
	config, err := h.services.Sandboxes.FallbackHarnessConfig(ctx, projectID)
	if err != nil {
		slog.WarnContext(ctx, "resolve fallback harness config", "projectId", projectID, "error", err)
		return nil
	}
	return config
}
