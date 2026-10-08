package cli

import (
	"context"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// poolPlatforms is what the project's pools host, for offering only the
// harnesses one of them can run (ADR 0145 §1). A pool hosts exactly one
// platform, and a discobox runs on its pool's, which its harness's image must
// be published for.
type poolPlatforms struct {
	hosted map[string]bool
	// undeclared is a pool whose agent has not said what it hosts yet — one
	// still coming up, or one on a server from before platforms. It could be
	// any platform, so it rules nothing out.
	undeclared bool
}

func (a *App) listPoolPlatforms(ctx context.Context, client *apiclientgen.Client, projectID string) (poolPlatforms, error) {
	res, err := client.ListPools(ctx, apiclientgen.ListPoolsParams{ProjectId: projectID})
	if err != nil {
		return poolPlatforms{}, err
	}
	body, err := expectResponse[apimodel.ListPoolsBody](res)
	if err != nil {
		return poolPlatforms{}, err
	}
	return platformsOf(body.GetPools()), nil
}

func platformsOf(pools []apimodel.Pool) poolPlatforms {
	out := poolPlatforms{hosted: map[string]bool{}}
	for _, pool := range pools {
		if platform := pool.Platform.Or(""); platform != "" {
			out.hosted[platform] = true
		} else {
			out.undeclared = true
		}
	}
	return out
}

// run reports whether a pool can run the harness: one hosts a platform its
// image is published for. A harness with no platforms has not been inspected
// since they were recorded, or comes from a server that records none, and
// rules out no pool — but there has to be a pool for it to run on.
func (p poolPlatforms) run(harness apimodel.HarnessConfig) bool {
	if len(p.hosted) == 0 && !p.undeclared {
		return false
	}
	if len(harness.Platforms) == 0 || p.undeclared {
		return true
	}
	for _, platform := range harness.Platforms {
		if p.hosted[platform] {
			return true
		}
	}
	return false
}
