package dockerworker

import (
	"strings"

	"github.com/discobox-ai/discobox/pool-agent/imagereap"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
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
	env := map[string]string{}
	if e.cfg.ImageRetention > 0 {
		env[imagereap.RetentionEnv] = e.cfg.ImageRetention.String()
	}
	if e.cfg.ProxyAuditRetention > 0 {
		env[proxyagent.EnvAuditRetention] = e.cfg.ProxyAuditRetention.String()
	}
	e.cfg.SetAuditSpoolEnv(env)
	// The image a sandbox delivered its platform runs (ADR 26-10-16-423
	// §10), which is the image the pool's own box runs too.
	if image := strings.TrimSpace(e.cfg.SandboxBaseImage); image != "" {
		env[sandboxruntime.EnvSandboxBaseImage] = image
	}
	if e.cfg.SandboxIdleTimeout > 0 {
		env[sandboxruntime.EnvSandboxIdleTimeout] = e.cfg.SandboxIdleTimeout.String()
	}
	return env
}
