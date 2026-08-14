package cli

import (
	"encoding/json"
	"strings"
	"time"

	apimodel "github.com/obot-platform/discobox/api/model"
)

// sandboxDisplayName is the name a listing shows for a sandbox: the window
// title its primary terminal last set, when one has, and the sandbox's own
// configured name — generated at create — until then. The title is what the
// harness says the work is about, which tells two sandboxes apart better than
// two generated names do.
func sandboxDisplayName(sb apimodel.Sandbox) string {
	if title := sandboxPrimaryTerminalTitle(sb); title != "" {
		return title
	}
	return sb.Config.Name
}

// sandboxPrimaryTerminalTitle is the window title the sandbox's primary
// terminal last set (OSC 0/2), as the sandbox-agent reported it with the rest
// of its status — the same push that carries the git state, so a listing can
// show it for every row without waking anything. Empty when no primary session
// has titled itself, which is the caller's cue to fall back to the configured
// name. Sessions live and ended alike are on the record; the newest primary is
// the one whose title the user last saw.
func sandboxPrimaryTerminalTitle(sb apimodel.Sandbox) string {
	agentStatus, ok := sb.Runtime.AgentStatus.Get()
	if !ok {
		return ""
	}
	raw, ok := agentStatus["sessions"]
	if !ok {
		return ""
	}
	var sessions []apimodel.SandboxAgentSessionStatus
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return ""
	}
	title, startedAt := "", time.Time{}
	for _, session := range sessions {
		if !session.Primary {
			continue
		}
		sessionTitle := strings.TrimSpace(session.Title.Or(""))
		if sessionTitle == "" {
			continue
		}
		sessionStartedAt := session.StartedAt.Or(time.Time{})
		if title == "" || sessionStartedAt.After(startedAt) {
			title, startedAt = sessionTitle, sessionStartedAt
		}
	}
	return title
}
