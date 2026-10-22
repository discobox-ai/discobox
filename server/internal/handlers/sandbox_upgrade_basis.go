package handlers

import (
	"context"
	"log/slog"

	"github.com/discobox-ai/discobox/server/internal/services"
)

// upgradeBasis is what the sandbox mappers report an available upgrade
// against besides a sandbox's own harness config: the project's reserved
// `shell` config, which a legacy sandbox with no harness config of its own
// upgrades to (ADR 0032 §4), and the platform bundle the release names.
//
// A lookup failure is logged and treated as absent rather than failing the
// request: this decorates a sandbox with an available upgrade, and losing that
// decoration is a much smaller harm than refusing to return the sandbox.
func (h *Handler) upgradeBasis(ctx context.Context, projectID string) services.SandboxUpgradeBasis {
	basis, err := h.services.Sandboxes.UpgradeBasis(ctx, projectID)
	if err != nil {
		slog.WarnContext(ctx, "resolve sandbox upgrade basis", "projectId", projectID, "error", err)
	}
	return basis
}
