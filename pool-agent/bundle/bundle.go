// Package bundle is the pool bundle (ADR 26-10-24-953 §2): the features under
// features/ that make a box a pool's, which pool-agent/Dockerfile's bundle
// stage builds FROM scratch, and where a pool's maker places what the pool
// agent reads beside them.
//
// A pool's maker places the agent bundle, the pool's pins and these files,
// and runs `discobox-provision apply`; the pool-agent feature lays and starts
// the agent's unit, which reads them.
package bundle

const (
	// BootstrapFile is the pool's poolagent.Bootstrap as JSON, root's alone,
	// which the agent's unit names to it (poolagent.EnvBootstrapFile).
	BootstrapFile = "/etc/discobox/pool-agent/bootstrap.json"
	// EnvironmentFile is the environment the maker sets the agent beside the
	// bootstrap: the pool's policy the agent reads from its environment.
	// Optional; the agent's unit reads it when it is there.
	EnvironmentFile = "/etc/discobox/pool-agent/environment"
)
