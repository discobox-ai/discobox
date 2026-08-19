package tui

import (
	"strings"
	"testing"
)

// The base column prefers the reported position, and marks the state of the
// work: a star for uncommitted content, a check for a head commit an apply
// has landed, nothing for merely committed work.
func TestSandboxBase(t *testing.T) {
	spawn := Sandbox{Branch: "main", Commit: "1111111"}
	cases := []struct {
		name string
		s    Sandbox
		want string
	}{
		{"spawn position until a report", spawn, "main@1111111"},
		{"spawn snapshot star", Sandbox{Branch: "main", Commit: "1111111", Dirty: true}, "main@1111111*"},
		{"reported position displaces spawn",
			Sandbox{Branch: "main", Commit: "1111111", Git: GitState{Known: true, Branch: "feature", Commit: "1111111"}},
			"feature@1111111"},
		{"reported dirt stars even a clean spawn",
			Sandbox{Branch: "main", Commit: "1111111", Git: GitState{Known: true, Branch: "main", Commit: "2222222", Dirty: true}},
			"main@2222222*"},
		{"a report of clean clears the spawn star",
			Sandbox{Branch: "main", Commit: "1111111", Dirty: true, Git: GitState{Known: true, Branch: "main", Commit: "1111111"}},
			"main@1111111"},
		{"applied wears the check",
			Sandbox{Branch: "main", Commit: "1111111", Git: GitState{Known: true, Branch: "main", Commit: "2222222", Applied: true}},
			"main@2222222✓"},
		{"applied shows the host-side commit the apply produced",
			Sandbox{Branch: "main", Commit: "1111111", Git: GitState{Known: true, Branch: "main", Commit: "2222222", Applied: true, AppliedCommit: "3333333"}},
			"main@3333333✓"},
		{"committed unapplied work wears the up arrow",
			Sandbox{Branch: "main", Commit: "1111111", Git: GitState{Known: true, Branch: "main", Commit: "2222222"}},
			"main@2222222⇡"},
		{"clean on the spawn commit is unmarked",
			Sandbox{Branch: "main", Commit: "1111111", Git: GitState{Known: true, Branch: "main", Commit: "1111111"}},
			"main@1111111"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.base(); got != tc.want {
				t.Fatalf("base = %q, want %q", got, tc.want)
			}
		})
	}
}

// Attach asks the power axis, not the lifecycle one. The case that matters is
// the errored box with a healthy container: displayState reports `error` for a
// latched failure whatever the container is doing (ADR 0034 §5), and refusing
// on that made archive/unarchive the only way to reach work that was never
// unreachable.
func TestSandboxAttachable(t *testing.T) {
	cases := []struct {
		name string
		s    Sandbox
		want bool
	}{
		{"running", Sandbox{State: StateRunning, HasRuntime: true}, true},
		{"stopped starts on demand", Sandbox{State: StateStopped, HasRuntime: true}, true},
		{"errored with a container", Sandbox{State: StateError, HasRuntime: true}, true},
		{"errored with no container", Sandbox{State: StateError}, false},
		{"starting, before any agent has reported", Sandbox{State: StateStarting}, true},
		{"archived, whatever a stale report said", Sandbox{State: StateArchived, HasRuntime: true}, false},
		{"nothing has reported at all", Sandbox{State: StateStopped}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.attachable(); got != tc.want {
				t.Fatalf("attachable = %t, want %t", got, tc.want)
			}
		})
	}
}

// The reason names the obstacle, not the row's state: the two ways to have no
// container are undone by different things.
func TestAttachWhyNamesTheObstacle(t *testing.T) {
	cases := []struct {
		name string
		s    Sandbox
		want string
	}{
		{"archived points at unarchive", Sandbox{State: StateArchived}, "unarchive"},
		{"no container points at repair", Sandbox{State: StateError}, "repair"},
		{"attachable has nothing to say", Sandbox{State: StateError, HasRuntime: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := attachWhy(true, []Sandbox{tc.s})
			if tc.want == "" {
				if got != "" {
					t.Fatalf("why = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("why = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}
