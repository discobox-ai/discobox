package server

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pkg/gitprogress"
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
	return func(p gitprogress.Progress) {
		if time.Since(last) < 200*time.Millisecond {
			return
		}
		last = time.Now()
		s.runtime.PublishSandboxProgress(ctx, sandboxruntime.SandboxProgressObservation{SandboxID: sandboxID, Phase: sandboxruntime.PhaseMaterializingSource, Clone: &sandboxruntime.CloneProgress{Source: sourcecache.RemoteKey(remote), Stage: string(p.Stage), Objects: p.Objects, ObjectsTotal: p.ObjectsTotal, Bytes: p.Bytes}})
	}
}
