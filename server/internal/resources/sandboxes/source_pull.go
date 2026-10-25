package sandboxes

import (
	"context"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/pkg/sourcecache"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// PullSandboxSource lends a credential only for this synchronous request. It
// never passes through intent or a resource that might persist the credential.
func (s *Service) PullSandboxSource(ctx context.Context, projectID, sandboxID, slug string, input services.PullSandboxSourceBody) (*model.Sandbox, error) {
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "a sandbox cannot lend Git credentials")
	}
	if err := authorizeRequestedScopes(ctx, []string{poolagentauth.ScopeSandboxWrite}); err != nil {
		return nil, err
	}
	sb, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	var source *model.GitSource
	for _, entry := range clientDeliveredSources(sb) {
		if entry.source.Slug != nil && *entry.source.Slug == slug {
			source = entry.source
			break
		}
	}
	if source == nil || source.CacheKey == nil || !strings.HasPrefix(*source.CacheKey, "remote/") || source.Checkout == nil || source.Checkout.Commit == nil {
		return nil, apperrors.NewStatusError(404, "source has no remote to pull")
	}
	if sb.SourceDeliveredAt != nil || sb.SourceDeliveries[slug].Status == sourcecache.Delivered {
		return sb, nil
	}
	if sb.SourceDeliveries[slug].Status != sourcecache.NeedsCredential {
		return nil, apperrors.NewStatusError(409, "source is not awaiting a Git credential")
	}
	branch := "main"
	if source.Checkout.RefType != nil && *source.Checkout.RefType == "branch" && source.Checkout.RefName != nil && *source.Checkout.RefName != "" {
		branch = *source.Checkout.RefName
	}
	request := sourcecache.PullRequest{Key: *source.CacheKey, Remote: sourceRemote(source), SandboxID: sandboxID, Slug: slug, Commit: *source.Checkout.Commit, Branch: branch, Credential: &sourcecache.Credential{Username: input.Username, Password: input.Password}}
	if source.Workspace != nil && source.Workspace.SnapshotRef != nil {
		request.Snapshot = *source.Workspace.SnapshotRef != ""
	}
	if source.Destination != nil && source.Destination.Directory != nil && source.Destination.WorkingDirectory != nil {
		if rest, ok := strings.CutPrefix(*source.Destination.WorkingDirectory, *source.Destination.Directory); ok {
			request.WorkingPath = strings.TrimLeft(strings.ReplaceAll(rest, `\`, "/"), "/")
		}
	}
	provider, err := s.sandboxProviders.ResolveForSandbox(ctx, sb)
	if err != nil {
		return nil, err
	}
	result, err := provider.PullSource(ctx, sandboxRefFromSandbox(sb), request)
	if err != nil {
		return nil, err
	}
	if result.Status == sourcecache.NeedsCredential {
		return nil, apperrors.NewStatusError(401, "Git credential refused")
	}
	if result.Status != sourcecache.Delivered {
		return nil, apperrors.NewStatusError(409, "pool could not pull the checkout commit; deliver it by push")
	}
	return s.CompleteSandboxSourcePush(ctx, projectID, sandboxID, services.CompleteSandboxSourcePushBody{Sources: map[string]string{slug: *source.Checkout.Commit}})
}
