package dockerworker

import (
	"strings"

	"github.com/discobox-ai/discobox/pool-agent/imagereap"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// poolPolicyEnv is the pool policy the agent applies to its own Docker
// daemon, its proxy, and the sandboxes it hosts, which it reads from its
// environment: written to the box beside its bootstrap
// (bundle.EnvironmentFile), which the agent's unit reads.
//
// The policy is kept out of the bootstrap deliberately. The bootstrap is
// identity and transport — "a backend is expressed entirely in the URLs it
// renders" — and image retention is neither. Policy that was never configured
// is omitted rather than defaulted.
func (e *Engine) poolPolicyEnv() map[string]string {
	env := poolruntime.PoolPolicy{
		ProxyAuditRetention: poolruntime.Duration(e.cfg.ProxyAuditRetention),
		AuditSpoolBudget:    e.cfg.AuditSpoolBudget,
		SandboxIdleTimeout:  poolruntime.Duration(e.cfg.SandboxIdleTimeout),
	}.Env()
	if e.cfg.ImageRetention > 0 {
		env[imagereap.RetentionEnv] = e.cfg.ImageRetention.String()
	}
	// The image a sandbox delivered its platform runs (ADR 26-10-16-423
	// §10), which is the image the pool's own box runs too.
	if image := strings.TrimSpace(e.cfg.SandboxBaseImage); image != "" {
		env[sandboxruntime.EnvSandboxBaseImage] = image
	}
	return env
}
