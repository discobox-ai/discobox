package opencode

import "github.com/obot-platform/discobox/prompter/internal/agent"

func init() {
	agent.Register(agent.StaticDetector{
		AgentKind: agent.KindOpenCode,
		EnvEquals: map[string]string{
			"OPENCODE": "1",
		},
		EnvAllKeys: []string{"OPENCODE_PID"},
	})
	agent.Register(agent.StaticDetector{
		AgentKind:    agent.KindOpenCode,
		ProcessNames: []string{"opencode"},
	})
}
