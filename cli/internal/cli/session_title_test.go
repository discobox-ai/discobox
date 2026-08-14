package cli

import (
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

// titledSandbox builds a sandbox named "generated-name" whose agent status
// carries the given sessions payload, empty for no report at all.
func titledSandbox(sessions string) apimodel.Sandbox {
	sb := apimodel.Sandbox{}
	sb.Config.Name = "generated-name"
	if sessions != "" {
		status := apiclientgen.SandboxRuntimeAgentStatus{"sessions": []byte(sessions)}
		sb.Runtime.AgentStatus = apiclientgen.NewOptNilSandboxRuntimeAgentStatus(status)
	}
	return sb
}

func TestSandboxDisplayName(t *testing.T) {
	cases := []struct {
		name     string
		sessions string
		want     string
	}{
		{
			name: "no report yet",
			want: "generated-name",
		},
		{
			name:     "primary titled itself",
			sessions: `[{"terminalId":"exc_1","primary":true,"title":"fix the reaper","state":"running","attacherCount":0,"execStatus":"running"}]`,
			want:     "fix the reaper",
		},
		{
			name:     "primary never titled itself",
			sessions: `[{"terminalId":"exc_1","primary":true,"state":"running","attacherCount":0,"execStatus":"running"}]`,
			want:     "generated-name",
		},
		{
			// A shell's title is not the sandbox's name; only the primary's is.
			name:     "only a non-primary titled itself",
			sessions: `[{"terminalId":"exc_2","primary":false,"title":"vim","state":"running","attacherCount":0,"execStatus":"running"}]`,
			want:     "generated-name",
		},
		{
			// Sessions live and ended alike are on the record; the newest
			// primary is the one whose title the user last saw.
			name: "newest primary wins",
			sessions: `[
				{"terminalId":"exc_old","primary":true,"title":"old work","startedAt":"2026-08-10T00:00:00Z","state":"exited","attacherCount":0,"execStatus":"exited"},
				{"terminalId":"exc_new","primary":true,"title":"new work","startedAt":"2026-08-12T00:00:00Z","state":"running","attacherCount":0,"execStatus":"running"}]`,
			want: "new work",
		},
		{
			name:     "blank title is no title",
			sessions: `[{"terminalId":"exc_1","primary":true,"title":"   ","state":"running","attacherCount":0,"execStatus":"running"}]`,
			want:     "generated-name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sandboxDisplayName(titledSandbox(tc.sessions)); got != tc.want {
				t.Fatalf("display name = %q, want %q", got, tc.want)
			}
		})
	}
}

// The launcher's row carries the same name the table shows, and the flag that
// tells rename the configured name is not the one on screen.
func TestToTUISandboxPrefersPrimaryTerminalTitle(t *testing.T) {
	titled := toTUISandbox(titledSandbox(`[{"terminalId":"exc_1","primary":true,"title":"fix the reaper","state":"running","attacherCount":0,"execStatus":"running"}]`))
	if titled.Name != "fix the reaper" || !titled.NameIsTitle {
		t.Fatalf("row = %q (NameIsTitle=%t), want the title, flagged", titled.Name, titled.NameIsTitle)
	}
	untitled := toTUISandbox(titledSandbox(""))
	if untitled.Name != "generated-name" || untitled.NameIsTitle {
		t.Fatalf("row = %q (NameIsTitle=%t), want the configured name, unflagged", untitled.Name, untitled.NameIsTitle)
	}
}
