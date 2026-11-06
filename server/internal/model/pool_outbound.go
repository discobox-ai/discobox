package model

import "github.com/discobox-ai/discobox/pkg/pooloutbound"

// PoolOutboundStatus exposes routing without credentials or server-local paths.
type PoolOutboundStatus struct {
	Probes          map[string]pooloutbound.Probes `json:"probes"`
	Source          string                         `json:"source"`
	Mode            string                         `json:"mode"`
	HTTP            string                         `json:"http"`
	HTTPS           string                         `json:"https"`
	CAFingerprints  []string                       `json:"caFingerprints"`
	DesiredRevision string                         `json:"desiredRevision"`
	AppliedRevision string                         `json:"appliedRevision"`
	RestartRequired bool                           `json:"restartRequired"`
	ShadowsYAML     bool                           `json:"shadowsYaml"`
}
