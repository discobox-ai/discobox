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
			// Written by an older sandbox-agent, which reported a derived
			// harness "state" this server no longer knows. Rows like this are
			// already in every live database and a stopped sandbox's row is
			// never rewritten, so an unknown field must not cost the title.
			name:     "primary titled itself (agent-status row from an older agent)",
			sessions: `[{"terminalId":"exc_1","primary":true,"title":"fix the reaper","state":"running","lastEvent":"Stop","attacherCount":0,"execStatus":"running"}]`,
			want:     "fix the reaper",
		},
		{
			name:     "primary titled itself",
			sessions: `[{"terminalId":"exc_1","primary":true,"title":"fix the reaper","attacherCount":0,"execStatus":"running"}]`,
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

// A name set in the sandbox's meta outranks the title its harness wrote
// (ADR 26-10-12-026 §3): it is what a person or the agent chose to call the
// sandbox. It is reported in meta, and the configured name is left alone.
func TestSandboxDisplayNamePrefersTheMetaName(t *testing.T) {
	sandbox := titledSandbox(`[{"terminalId":"exc_1","primary":true,"title":"Commit files","attacherCount":0,"execStatus":"running"}]`)
	name := "  Daily GH Check In "
	sandbox.MetaName = &name
	if got := SandboxDisplayName(sandbox); got != "Daily GH Check In" {
		t.Fatalf("display name = %q, want the name the sandbox was given", got)
	}
	out, err := SandboxToAPI(sandbox, SandboxUpgradeBasis{})
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	meta, _ := out.Meta.Get()
	if out.DisplayName != "Daily GH Check In" || meta.Name.Or("") != "Daily GH Check In" || out.Config.Name != "generated-name" {
		t.Fatalf("displayName = %q, meta.name = %q, config.name = %q", out.DisplayName, meta.Name.Or(""), out.Config.Name)
	}
	// The description is left out when unset; the tags are sent empty, for a
	// client from before they were optional.
	if tags, sent := meta.Tags.Get(); !sent || len(tags) != 0 || meta.Description.Set {
		t.Fatalf("meta = %+v, want the name, empty tags and no description", meta)
	}

	// Cleared, the title names it again.
	blank := " "
	sandbox.MetaName = &blank
	if got := SandboxDisplayName(sandbox); got != "Commit files" {
		t.Fatalf("display name with no name set = %q, want the title", got)
	}
}

func TestSandboxToAPIIncludesCalculatedDisplayName(t *testing.T) {
	sandbox := titledSandbox(`[{"terminalId":"exc_1","primary":true,"title":"fix the reaper","state":"running","attacherCount":0,"execStatus":"running"}]`)
	out, err := SandboxToAPI(sandbox, SandboxUpgradeBasis{})
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
