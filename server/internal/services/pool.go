package services

import (
	"time"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// PoolToAPI presents effective health without changing the persisted agent
// observations. Every pool response uses this same freshness gate as placement.
func PoolToAPI(pool *model.Pool) (apimodel.Pool, error) {
	health := pool.Health(time.Now())
	ready := health == model.PoolHealthReady
	return Convert[apimodel.Pool](struct {
		*model.Pool
		Health      string `json:"health"`
		Ready       bool   `json:"ready"`
		Schedulable bool   `json:"schedulable"`
	}{pool, health, ready, ready && pool.Schedulable})
}
