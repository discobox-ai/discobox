package cli

import (
	"context"
	"sort"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// Pools is one server's pools for the project, oldest first, with the project's
// default marked — the same listing `discobox pool list` reads, and the pools
// `discobox run --pool` resolves against.
func (d *apiDataSource) Pools(ctx context.Context, server string) ([]tui.Pool, error) {
	d, err := d.on(ctx, server)
	if err != nil {
		return nil, err
	}
	res, err := d.client.ListPools(ctx, apiclientgen.ListPoolsParams{ProjectId: d.projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListPoolsBody](res)
	if err != nil {
		return nil, err
	}
	// The default is a property of the project rather than of the pool, so it
	// takes a request of its own.
	defaultID, err := d.app.defaultPoolID(ctx, d.client, d.projectID)
	if err != nil {
		return nil, err
	}
	sorted := append([]apimodel.Pool(nil), body.GetPools()...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) })
	pools := make([]tui.Pool, 0, len(sorted))
	for _, pool := range sorted {
		pools = append(pools, tui.Pool{
			ID:       pool.ID,
			Name:     strings.TrimSpace(pool.Name),
			Platform: strings.TrimSpace(pool.Platform.Or("")),
			Default:  pool.ID == defaultID,
		})
	}
	return pools, nil
}
