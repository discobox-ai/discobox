package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// PoolJudge binds the pool-owned judge identity to one configured harness
// revision. Its runtime is reconciled by pool-agent, not the sandbox scheduler.
type PoolJudge struct {
	PoolID          string `gorm:"primaryKey;type:text"`
	ProjectID       string `gorm:"not null;type:text;index"`
	SandboxID       string `gorm:"not null;type:text;uniqueIndex"`
	HarnessConfigID string `gorm:"not null;type:text"`
	Revision        string `gorm:"not null;type:text"`
	Pool            *Pool  `gorm:"foreignKey:PoolID;constraint:OnDelete:CASCADE"`
}

// JudgeRevision includes the configured files, image metadata and credential
// bindings that determine a dedicated harness's authority and behavior.
func JudgeRevision(hc *HarnessConfig, bindings []HarnessConfigSecretBinding) (string, error) {
	data, err := json.Marshal(struct {
		Harness  *HarnessConfig
		Bindings []HarnessConfigSecretBinding
	}{hc, bindings})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
