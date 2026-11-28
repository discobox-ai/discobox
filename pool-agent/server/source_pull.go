package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pkg/gitprogress"
	"github.com/discobox-ai/discobox/pool-agent/gitcache"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"

	"github.com/discobox-ai/discobox/pkg/sourcecache"
	workerapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

func (s *sandboxService) pullSources(ctx context.Context, req *workerapimodel.PoolSandboxCreateRequest) (map[string]sourcecache.Delivery, error) {
	if !req.PullSources.Or(false) {
		return nil, nil
	}
	sources := []workerapimodel.GitSource{}
	if source, ok := req.Config.Source.Get(); ok {
		sources = append(sources, source)
	}
	if refs, ok := req.Config.SourceCodeReferences.Get(); ok {
		for _, source := range refs {
			sources = append(sources, source)
		}
	}
	results := map[string]sourcecache.Delivery{}
	var firstErr error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, source := range sources {
		key := source.CacheKey.Or("")
		if source.Delivery.Or("") != workerapi.GitSourceDeliveryCache || !strings.HasPrefix(key, "remote/") {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			remote := source.UpstreamUrl.Or("")
			if source.LocalDirectory.Or("") == "" {
				if u, ok := source.URL.Get(); ok {
					remote = u.String()
				}
			}
			checkout, _ := source.Checkout.Get()
			branch := "main"
			if checkout.RefType.Or("") == "branch" {
				branch = checkout.RefName.Or("main")
			}
			if branch == "" {
				branch = "main"
			}
			workspace, _ := source.Workspace.Get()
			result, err := s.runtime.GitCache().Pull(ctx, sourcecache.PullRequest{Key: key, Remote: remote, SandboxID: req.SandboxId, Slug: source.Slug.Or(""), Commit: checkout.Commit.Or(""), Branch: branch, Snapshot: workspace.SnapshotRef.Or("") != "", WorkingPath: pullWorkingPath(source)}, s.pullProgress(ctx, req.SandboxId, remote))
			mu.Lock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			results[source.Slug.Or("")] = result
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results, firstErr
}

func (s *sandboxService) PoolPullSandboxSource(ctx context.Context, body *workerapimodel.PoolSourcePullRequest, params workerapi.PoolPullSandboxSourceParams) (*workerapimodel.SourceDelivery, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	request := sourcecache.PullRequest{Key: body.Key, Remote: body.Remote, SandboxID: params.SandboxId, Slug: params.Slug, Commit: body.Commit, Branch: body.Branch, Snapshot: body.Snapshot.Or(false), WorkingPath: body.WorkingPath.Or("")}
	if c, ok := body.Credential.Get(); ok {
		request.Credential = &sourcecache.Credential{Username: c.Username, Password: c.Password}
	}
	result, err := s.runtime.GitCache().Pull(ctx, request, s.pullProgress(ctx, params.SandboxId, request.Remote))
	if err != nil {
		return nil, mapRuntimeError(err)
	}
	return &workerapimodel.SourceDelivery{Status: result.Status, Protocol: workerapi.NewOptString(result.Protocol), Host: workerapi.NewOptString(result.Host), Path: workerapi.NewOptString(result.Path)}, nil
}

// PoolRefreshSandboxSource refreshes a delivered source's namespace from its
// remote (ADR 26-11-24-142 §4). The control plane names the cache and the
// remote, from the sandbox it holds; only the credential is the client's.
func (s *sandboxService) PoolRefreshSandboxSource(ctx context.Context, body *workerapimodel.PoolSourceRefreshRequest, params workerapi.PoolRefreshSandboxSourceParams) (*workerapimodel.SourceRefresh, error) {
	if err := s.authorize(params.ProjectId, params.PoolId); err != nil {
		return nil, err
	}
	request := sourcecache.RefreshRequest{Key: body.Key, Remote: body.Remote, SandboxID: params.SandboxId, Slug: params.Slug, Branch: body.Branch.Or("")}
	if c, ok := body.Credential.Get(); ok {
		request.Credential = &sourcecache.Credential{Username: c.Username, Password: c.Password}
	}
	result, err := s.runtime.GitCache().Refresh(ctx, request, nil)
	switch {
	case errors.Is(err, gitcache.ErrInvalid):
		return nil, newStatusError(http.StatusBadRequest, err.Error())
	case errors.Is(err, gitcache.ErrNotDelivered):
		return nil, newStatusError(http.StatusConflict, err.Error())
	case err != nil:
		return nil, mapRuntimeError(err)
	}
	out := &workerapimodel.SourceRefresh{Status: result.Status, Protocol: workerapi.NewOptString(result.Protocol), Host: workerapi.NewOptString(result.Host), Path: workerapi.NewOptString(result.Path)}
	for _, update := range result.Updates {
		out.Updates = append(out.Updates, workerapimodel.SourceRefUpdate{Ref: update.Ref, Old: optString(update.Old), New: optString(update.New)})
	}
	return out, nil
}

// optString is s, or nothing when it is empty.
func optString(s string) workerapi.OptString {
	if s == "" {
		return workerapi.OptString{}
	}
	return workerapi.NewOptString(s)
}

func pullWorkingPath(source workerapimodel.GitSource) string {
	destination, ok := source.Destination.Get()
	if !ok {
		return ""
	}
	directory, working := destination.Directory.Or(""), destination.WorkingDirectory.Or("")
	if directory == "" || directory == working {
		return ""
	}
	rest, ok := strings.CutPrefix(working, directory)
	if !ok {
		return ""
	}
	return strings.TrimLeft(strings.ReplaceAll(rest, `\`, "/"), "/")
}

func (s *sandboxService) pullProgress(ctx context.Context, sandboxID, remote string) func(gitprogress.Progress) {
	var last time.Time
	source := strings.TrimPrefix(sourcecache.RemoteKey(remote), "remote/")
	s.runtime.PublishSandboxProgress(ctx, sandboxruntime.SandboxProgressObservation{SandboxID: sandboxID, Phase: sandboxruntime.PhaseFetchingSource, Clone: &sandboxruntime.CloneProgress{Source: source}})
	return func(p gitprogress.Progress) {
		if time.Since(last) < 200*time.Millisecond {
			return
		}
		last = time.Now()
		s.runtime.PublishSandboxProgress(ctx, sandboxruntime.SandboxProgressObservation{SandboxID: sandboxID, Phase: sandboxruntime.PhaseFetchingSource, Clone: &sandboxruntime.CloneProgress{Source: source, Stage: string(p.Stage), Objects: p.Objects, ObjectsTotal: p.ObjectsTotal, Bytes: p.Bytes}})
	}
}
