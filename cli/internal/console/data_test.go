package console

import (
	"encoding/base64"
	"slices"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/programstatus"
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

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// A window says the stream's report while it is newer than the poll, and the
// poll's otherwise: before the stream reports, once its session ends, after
// a poll taken since, and after a prompt the poll is newer than.
func TestAgentHeardIsTheNewerOfTheStreamAndThePoll(t *testing.T) {
	t.Parallel()
	polled := time.Unix(1000, 0)
	box := Sandbox{Agent: programstatus.Status{State: programstatus.Working, Msg: "polled"}, AgentObservedAt: polled}
	say := func(heard *programstatus.Set, ended bool) string {
		st, ok := box.AgentHeard(heard, ended)
		if !ok {
			return "nothing"
		}
		return string(st.State) + " " + st.Msg
	}
	if got := say(nil, false); got != "working polled" {
		t.Fatalf("no stream: %q", got)
	}
	var heard programstatus.Set
	if got := say(&heard, false); got != "working polled" {
		t.Fatalf("a stream that said nothing: %q", got)
	}
	heard.Observe([]byte("7501;state=blocked:msg="+b64("which?")), polled.Add(time.Second), 0)
	if got := say(&heard, false); got != "blocked which?" {
		t.Fatalf("a newer report: %q", got)
	}
	if got := say(&heard, true); got != "working polled" {
		t.Fatalf("an ended session: %q", got)
	}
	heard.Observe([]byte("133;A"), polled.Add(2*time.Second), 0)
	if got := say(&heard, false); got != "nothing" {
		t.Fatalf("a prompt after a report newer than the poll: %q", got)
	}
	box.AgentObservedAt = polled.Add(3 * time.Second)
	if got := say(&heard, false); got != "working polled" {
		t.Fatalf("a poll taken since: %q", got)
	}
	box.Agent = programstatus.Status{}
	if got := say(&heard, false); got != "nothing" {
		t.Fatalf("a poll with no report: %q", got)
	}
}

// One decimal below ten and none above, so "1.2 GiB" and "15 GiB" both fit the
// same column.
func TestBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2_483_027_968, "2.3 GiB"},
		{15_264_268_288, "14 GiB"},
	} {
		if got := Bytes(tc.n); got != tc.want {
			t.Errorf("Bytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// A directory's count is said as nothing yet, climbing, and final.
func TestADirectoryCountIsSaidAsItClimbs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		total DirectoryTotal
		want  string
	}{
		{DirectoryTotal{}, "calculating…"},
		{DirectoryTotal{Bytes: 5 << 20, Files: 3}, "5.0 MiB in 3 files, still counting…"},
		{DirectoryTotal{Bytes: 7 << 20, Files: 1, Done: true}, "7.0 MiB in 1 file"},
	} {
		if got := tc.total.Said(); got != tc.want {
			t.Errorf("%+v said %q, want %q", tc.total, got, tc.want)
		}
	}
}

// A name is better than an id, and an id is better than nothing: a freshly
// created discobox may be reported before its name comes back.
func TestSandboxLabelPrefersTheName(t *testing.T) {
	t.Parallel()
	if got := (Sandbox{ID: "sbx_1", Name: "nimble_swan"}).Label(); got != "nimble_swan" {
		t.Fatalf("Label() = %q", got)
	}
	if got := (Sandbox{ID: "sbx_1"}).Label(); got != "sbx_1" {
		t.Fatalf("Label() = %q", got)
	}
}
