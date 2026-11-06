package wslc

import (
	"context"
	"errors"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"
)

func (d *Driver) PoolOutbound(_ context.Context, _ string, r pooloutbound.Resolved) (pooloutbound.Delivery, error) {
	if r.HTTP != "" || r.HTTPS != "" || r.CAPEM != "" {
		return pooloutbound.Delivery{}, errors.New("WSL Containers does not expose pre-daemon outbound configuration; use a pool backend with managed proxy/trust boot delivery")
	}
	return pooloutbound.Delivery{Runtime: r, AppliedRevision: r.Revision()}, nil
}
