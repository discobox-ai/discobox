package cli

import (
	"context"
	"slices"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// poolPlacements is what the project's pools host and run, for offering only
// the harnesses one of them can run. A pool hosts exactly one platform, which
// a harness's image must be published for (ADR 0145 §1), and runs one kind of
// image, which a harness's image must be (ADR 26-10-09-106 §4); both must hold
// of the same pool.
type poolPlacements struct {
	pools []poolPlacement
}

// poolPlacement is one pool's platform and image kind. Either is empty while
// its agent has not said — a pool still coming up, or one on a server from
// before they were recorded — and could then be anything, so it rules nothing
// out.
type poolPlacement struct {
	platform  string
	imageKind string
}

func (a *App) listPoolPlacements(ctx context.Context, client *apiclientgen.Client, projectID string) (poolPlacements, error) {
	res, err := client.ListPools(ctx, apiclientgen.ListPoolsParams{ProjectId: projectID})
	if err != nil {
		return poolPlacements{}, err
	}
	body, err := expectResponse[apimodel.ListPoolsBody](res)
	if err != nil {
		return poolPlacements{}, err
	}
	return placementsOf(body.GetPools()), nil
}

func placementsOf(pools []apimodel.Pool) poolPlacements {
	out := poolPlacements{pools: make([]poolPlacement, 0, len(pools))}
	for _, pool := range pools {
		out.pools = append(out.pools, poolPlacement{platform: pool.Platform.Or(""), imageKind: pool.ImageKind.Or("")})
	}
	return out
}

// ociImageKind is the kind a harness has when its server sends none: one from
// before image kinds, whose harnesses were all OCI images.
const ociImageKind = "oci"

// run reports whether a pool can run the harness: one hosts a platform its
// image is published for and runs the kind of image it is. A harness with no
// platforms has not been inspected since they were recorded, or comes from a
// server that records none, and rules out no pool's platform — but there has
// to be a pool for it to run on. A harness whose kind is sent empty has none
// declared — a manifest-file harness from before kinds — and runs on no pool.
func (p poolPlacements) run(harness apimodel.HarnessConfig) bool {
	kind := harness.ImageKind.Or(ociImageKind)
	if kind == "" {
		return false
	}
	for _, pool := range p.pools {
		platform := pool.platform == "" || len(harness.Platforms) == 0 || slices.Contains(harness.Platforms, pool.platform)
		if platform && (pool.imageKind == "" || pool.imageKind == kind) {
			return true
		}
	}
	return false
}
