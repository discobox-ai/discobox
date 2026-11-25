package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/pkg/sourcecache"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// PullSandboxSource has the pool pull a remote source over HTTPS: anonymously,
// or with a credential lent only for this synchronous request, which never
// passes through intent or a resource that might persist it.
//
// For a source still owed its delivery the pull is that delivery (ADR
// 26-10-23-948), and the answer is the sandbox. For a delivered source it is
// a refresh (ADR 26-11-24-142 §4): the pool brings the source's namespace's
// record of its remote up to the remote, and the answer is what that did.
// Exactly one of the two is returned.
func (s *Service) PullSandboxSource(ctx context.Context, projectID, sandboxID, slug string, input services.PullSandboxSourceBody) (*model.Sandbox, *sourcecache.Refresh, error) {
	var credential *sourcecache.Credential
	if password, ok := input.Password.Get(); ok {
		credential = &sourcecache.Credential{Username: input.Username.Or(""), Password: password}
	} else if input.Username.IsSet() {
		return nil, nil, apperrors.NewStatusError(http.StatusBadRequest, "a Git credential needs its password")
	}
	// A sandbox may have its pool pull anonymously, as it may have it pull any
	// public HTTPS remote into that remote's cache; it never lends.
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox && credential != nil {
		return nil, nil, apperrors.NewStatusError(http.StatusForbidden, "a sandbox cannot lend Git credentials")
	}
	if err := authorizeRequestedScopes(ctx, []string{poolagentauth.ScopeSandboxWrite}); err != nil {
		return nil, nil, err
	}
	sb, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, nil, apperrors.NotFound(err, "sandbox not found")
	}
	var source *model.GitSource
	for _, entry := range clientDeliveredSources(sb) {
		if entry.source.Slug != nil && *entry.source.Slug == slug {
			source = entry.source
			break
		}
	}
	if source == nil || source.CacheKey == nil || !strings.HasPrefix(*source.CacheKey, "remote/") {
		return nil, nil, apperrors.NewStatusError(http.StatusNotFound, "source has no remote to pull")
	}
	if sb.SourceDeliveredAt != nil || sb.SourceDeliveries[slug].Status == sourcecache.Delivered {
		refresh, err := s.refreshSandboxSource(ctx, sb, source, slug, input.Branch.Or(""), credential)
		return nil, refresh, err
	}
	sb, err = s.pullOwedSource(ctx, sb, source, slug, input, credential)
	return sb, nil, err
}

// pullOwedSource delivers a source the sandbox is still owed by a pool pull
// of the commit it was created from.
func (s *Service) pullOwedSource(ctx context.Context, sb *model.Sandbox, source *model.GitSource, slug string, input services.PullSandboxSourceBody, credential *sourcecache.Credential) (*model.Sandbox, error) {
	if input.Branch.IsSet() {
		return nil, apperrors.NewStatusError(http.StatusConflict, "the source has not been delivered yet, and is pulled at the commit the discobox was created from; a branch applies only to a refresh")
	}
	if source.Checkout == nil || source.Checkout.Commit == nil {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "source has no remote to pull")
	}
	if sb.SourceDeliveries[slug].Status != sourcecache.NeedsCredential {
		return nil, apperrors.NewStatusError(http.StatusConflict, "source is not awaiting a Git credential")
	}
	branch := "main"
	if source.Checkout.RefType != nil && *source.Checkout.RefType == "branch" && source.Checkout.RefName != nil && *source.Checkout.RefName != "" {
		branch = *source.Checkout.RefName
	}
	request := sourcecache.PullRequest{Key: *source.CacheKey, Remote: sourceRemote(source), SandboxID: sb.ID, Slug: slug, Commit: *source.Checkout.Commit, Branch: branch, Credential: credential}
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
		return nil, apperrors.NewStatusError(http.StatusUnauthorized, "Git credential refused")
	}
	if result.Status != sourcecache.Delivered {
		return nil, apperrors.NewStatusError(http.StatusConflict, "pool could not pull the checkout commit; deliver it by push")
	}
	return s.CompleteSandboxSourcePush(ctx, sb.ProjectID, sb.ID, services.CompleteSandboxSourcePushBody{Sources: map[string]string{slug: *source.Checkout.Commit}})
}

// refreshSandboxSource has the pool refresh a delivered remote source's
// namespace from its remote. A refusal for want of a credential is an answer
// (needs_credential), not an error: it is what an anonymous refresh of a
// private remote expects to hear, and what tells the client to apply its
// credential policy.
func (s *Service) refreshSandboxSource(ctx context.Context, sb *model.Sandbox, source *model.GitSource, slug, branch string, credential *sourcecache.Credential) (*sourcecache.Refresh, error) {
	switch {
	case source.LocalDirectory != nil && strings.TrimSpace(*source.LocalDirectory) != "":
		return nil, apperrors.NewStatusError(http.StatusConflict, "the source came from a directory on a client's machine, so it is not refreshed from its remote; push its commits instead")
	case source.Delivery != model.GitSourceDeliveryCache:
		return nil, apperrors.NewStatusError(http.StatusConflict, "the source was delivered before pools cached Git, so the discobox fetches its remote itself")
	case sb.DesiredState == model.DesiredStateArchived:
		return nil, apperrors.NewStatusError(http.StatusConflict, "sandbox is archived; unarchive it first")
	}
	provider, err := s.sandboxProviders.ResolveForSandbox(ctx, sb)
	if err != nil {
		return nil, err
	}
	result, err := provider.RefreshSource(ctx, sandboxRefFromSandbox(sb), sourcecache.RefreshRequest{Key: *source.CacheKey, Remote: sourceRemote(source), SandboxID: sb.ID, Slug: slug, Branch: branch, Credential: credential})
	if errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
		// Neither way of refreshing reaches such a pool: it has no refresh
		// route, and takes no refresh push either.
		return nil, apperrors.NewStatusError(http.StatusConflict, "the discobox's pool is too old to refresh a source; upgrade the pool")
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}
