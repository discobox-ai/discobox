package tui

import "testing"

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
