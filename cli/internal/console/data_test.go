package console

import (
	"slices"
	"testing"
	"time"
)

// The base column prefers the reported position and marks dirt or commits ahead.
func TestSandboxBase(t *testing.T) {
	t.Parallel()
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
		{"committed work ahead of spawn wears the up arrow",
			Sandbox{Branch: "main", Commit: "1111111", Git: GitState{Known: true, Branch: "main", Commit: "2222222"}},
			"main@2222222⇡"},
		{"clean on the spawn commit is unmarked",
			Sandbox{Branch: "main", Commit: "1111111", Git: GitState{Known: true, Branch: "main", Commit: "1111111"}},
			"main@1111111"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Base(); got != tc.want {
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
	t.Parallel()
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
		{"repairing, with the container gone", Sandbox{State: StateRepairing}, true},
		{"archived, whatever a stale report said", Sandbox{State: StateArchived, HasRuntime: true}, false},
		{"nothing has reported at all", Sandbox{State: StateStopped}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Attachable(); got != tc.want {
				t.Fatalf("attachable = %t, want %t", got, tc.want)
			}
		})
	}
}

// repair is offered on every box but an archived one: a box can be broken
// behind any state, and only unarchive brings an archived one back.
func TestRepairableIsAnythingButArchived(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		s    Sandbox
		want bool
	}{
		{"errored with a container", Sandbox{State: StateError, HasRuntime: true}, true},
		{"errored with no container", Sandbox{State: StateError}, true},
		{"never reported on", Sandbox{State: StateStopped}, true},
		{"running", Sandbox{State: StateRunning, HasRuntime: true}, true},
		{"stopped, with a container", Sandbox{State: StateStopped, HasRuntime: true}, true},
		{"suspended", Sandbox{State: StateSuspended, HasRuntime: true}, true},
		{"starting", Sandbox{State: StateStarting}, true},
		{"repairing", Sandbox{State: StateRepairing}, true},
		{"archived is unarchived instead", Sandbox{State: StateArchived}, false},
		{"archived and errored is still unarchived", Sandbox{State: StateArchived, HasRuntime: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Repairable(); got != tc.want {
				t.Fatalf("repairable = %t, want %t", got, tc.want)
			}
		})
	}
}

// A discobox created under another client identity says so; one created under
// this one says nothing, and neither does any row when this window cannot say
// what identity it is itself.
func TestSandboxElsewhere(t *testing.T) {
	t.Parallel()
	const here = "host_0123456789abcdef"
	const there = "host_zzzz456789abcdef"
	me := Session{HostID: here, Host: "wilma"}
	cases := []struct {
		name    string
		s       Sandbox
		session Session
		want    string
	}{
		{"from this identity", Sandbox{OriginHostID: here, OriginHost: "wilma"}, me, ""},
		{"no origin recorded", Sandbox{}, me, ""},
		{"this identity unknown", Sandbox{OriginHostID: there, OriginHost: "betty"}, Session{Host: "wilma"}, ""},
		{"another machine", Sandbox{OriginHostID: there, OriginHost: "betty"}, me, "from betty (host_zzzz45)"},
		{"another machine with no hostname", Sandbox{OriginHostID: there}, me, "from host_zzzz45"},
		// Another identity on this very machine: a second account, or a
		// container that did not keep the id it generated last time. Naming
		// the hostname there would read as a mistake, so the id says it alone.
		{"another identity on this machine",
			Sandbox{OriginHostID: there, OriginHost: "wilma"}, me, "from host_zzzz45"},
		{"hostname compared case-insensitively",
			Sandbox{OriginHostID: there, OriginHost: "WILMA"}, me, "from host_zzzz45"},
		{"an unknown local hostname suppresses nothing",
			Sandbox{OriginHostID: there, OriginHost: "wilma"}, Session{HostID: here}, "from wilma (host_zzzz45)"},
		{"an identity somebody set themselves is said whole",
			Sandbox{OriginHostID: "ci", OriginHost: "runner"}, me, "from runner (ci)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Elsewhere(tc.session); got != tc.want {
				t.Fatalf("elsewhere = %q, want %q", got, tc.want)
			}
		})
	}
}

// A harness's refused credential affects every box running the harness; any
// other affects the box that ran into it; neither crosses servers. Requests
// are a box's own, oldest first.
func TestRequestsAndRejectionsFor(t *testing.T) {
	session := Session{}
	a := Sandbox{ID: "a", HarnessID: "h"}
	b := Sandbox{ID: "b", HarnessID: "h"}
	elsewhere := Sandbox{ID: "a", HarnessID: "h", Server: "work"}
	rejections := []SecretRejection{
		{SecretID: "harness", SandboxID: "a", HarnessConfigID: "h"},
		{SecretID: "project", SandboxID: "a"},
	}
	ids := func(rs []SecretRejection) (out []string) {
		for _, r := range rs {
			out = append(out, r.SecretID)
		}
		return out
	}
	if got := ids(RejectionsFor(session, a, rejections)); !slices.Equal(got, []string{"harness", "project"}) {
		t.Errorf("a: %v", got)
	}
	if got := ids(RejectionsFor(session, b, rejections)); !slices.Equal(got, []string{"harness"}) {
		t.Errorf("b: %v", got)
	}
	if got := RejectionsFor(session, elsewhere, rejections); len(got) != 0 {
		t.Errorf("another server's box: %v", got)
	}
	requests := []CredentialRequest{
		{ID: "late", SandboxID: "a", Created: time.Unix(2, 0)},
		{ID: "early", SandboxID: "a", Created: time.Unix(1, 0)},
		{ID: "b's", SandboxID: "b"},
		{ID: "work", SandboxID: "a", Server: "work"},
	}
	var got []string
	for _, r := range RequestsFor(session, a, requests) {
		got = append(got, r.ID)
	}
	if !slices.Equal(got, []string{"early", "late"}) {
		t.Errorf("requests for a: %v", got)
	}
}
