package model

import "time"

const PoolHeartbeatTimeout = 90 * time.Second

const (
	PoolHealthUnknown  = "unknown"
	PoolHealthReady    = "ready"
	PoolHealthNotReady = "not_ready"
	PoolHealthOffline  = "offline"
)

// Health derives liveness at the point of use, even while a runtime reconcile
// is blocked building images. Only a status heartbeat can establish readiness;
// registration and provisioning progress cannot renew it.
func (p *Pool) Health(now time.Time) string {
	baseline := p.CreatedAt
	if p.HealthCheckStartedAt != nil {
		baseline = *p.HealthCheckStartedAt
	}
	if p.StatusReportedAt == nil || (p.HealthCheckStartedAt != nil && p.StatusReportedAt.Before(*p.HealthCheckStartedAt)) {
		if !baseline.IsZero() && now.Sub(baseline) > PoolHeartbeatTimeout {
			return PoolHealthOffline
		}
		return PoolHealthUnknown
	}
	if now.Sub(*p.StatusReportedAt) > PoolHeartbeatTimeout {
		return PoolHealthOffline
	}
	if !p.Ready {
		return PoolHealthNotReady
	}
	return PoolHealthReady
}

func (p *Pool) IsReady() bool {
	return p.Health(time.Now()) == PoolHealthReady
}

var PoolHealthStatuses = []string{PoolHealthUnknown, PoolHealthReady, PoolHealthNotReady, PoolHealthOffline}
