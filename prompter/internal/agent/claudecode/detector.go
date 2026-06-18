package claudecode

import "github.com/obot-platform/discobox/prompter/internal/agent"

func init() {
	agent.Register(agent.StaticDetector{
		AgentKind: agent.KindClaudeCode,
		EnvKeys:   []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_REMOTE_SESSION_ID"},
	})
	agent.Register(agent.StaticDetector{
		AgentKind:    agent.KindClaudeCode,
		ProcessNames: []string{"claude", "claude-code"},
	})
}
