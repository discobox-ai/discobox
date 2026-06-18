package forced

import (
	"strings"

	"github.com/obot-platform/discobox/prompter/internal/agent"
)

func init() {
	agent.Register(detector{})
}

type detector struct{}

func (detector) Kind() agent.Kind {
	return agent.KindUnknown
}

func (detector) NeedsEnvironment() bool {
	return true
}

func (detector) NeedsProcessAncestry() bool {
	return false
}

func (detector) Detect(ctx agent.Context) (agent.Detected, bool) {
	forced := strings.TrimSpace(ctx.Environment["DISCOBOX_PROMPTER_AGENT"])
	if forced == "" {
		return agent.Detected{}, false
	}
	return agent.Detected{Kind: agent.Kind(strings.ToLower(forced)), Source: "DISCOBOX_PROMPTER_AGENT"}, true
}
