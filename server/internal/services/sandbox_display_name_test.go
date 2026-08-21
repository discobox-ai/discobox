package services

import (
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// titledSandbox builds a sandbox named "generated-name" whose agent status
// carries the given sessions payload, empty for no report at all.
func titledSandbox(sessions string) *model.Sandbox {
	sandbox := &model.Sandbox{ID: "sbx_abc12345000000p3", ProjectID: "p1", CreatedByUserID: "u1", Name: "generated-name"}
	if sessions != "" {
		sandbox.AgentStatus = []byte(`{"sources":[],"observedAt":"2026-08-12T00:00:00Z","sessions":` + sessions + `}`)
	}
	return sandbox
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
		{
			name:     "unreadable report falls back to the configured name",
			sessions: `"not an array"`,
			want:     "generated-name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SandboxDisplayName(titledSandbox(tc.sessions)); got != tc.want {
				t.Fatalf("display name = %q, want %q", got, tc.want)
			}
		})
	}
}

// A sandbox with no name of its own is still identifiable: the ID is the last
// fallback, so a listing never shows a blank cell.
func TestSandboxDisplayNameFallsBackToID(t *testing.T) {
	sandbox := titledSandbox("")
	sandbox.Name = ""
	if got := SandboxDisplayName(sandbox); got != sandbox.ID {
		t.Fatalf("display name = %q, want the sandbox ID %q", got, sandbox.ID)
	}
}

func TestSandboxToAPIIncludesCalculatedDisplayName(t *testing.T) {
	sandbox := titledSandbox(`[{"terminalId":"exc_1","primary":true,"title":"fix the reaper","state":"running","attacherCount":0,"execStatus":"running"}]`)
	out, err := SandboxToAPI(sandbox, nil)
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	if out.DisplayName != "fix the reaper" {
		t.Fatalf("displayName = %q, want the primary terminal's title", out.DisplayName)
	}
	if out.Config.Name != "generated-name" {
		t.Fatalf("config.name = %q, want the configured name left alone", out.Config.Name)
	}
}
