package cli

import (
	"slices"
	"testing"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// A level is sent whole, so the part a command line leaves out is read back
// as stored rather than reset — and nothing is read when both are given.
func TestEgressFlagsKeepThePartNotGiven(t *testing.T) {
	stored := apimodel.EgressPolicy{
		Allow:         apiclientgen.NewOptNilStringArray([]string{"github.com"}),
		DenyByDefault: apiclientgen.NewOptBool(true),
		Passthrough:   apiclientgen.NewOptNilStringArray([]string{"pinned.example"}),
	}
	for _, tc := range []struct {
		name            string
		args            []string
		wantSent        bool
		wantRead        bool
		wantAllow       []string
		wantDeny        bool
		wantPassthrough []string
	}{
		{name: "neither flag", args: nil},
		{name: "allow only keeps the restriction", args: []string{"--egress-allow", "a.example,b.example"}, wantSent: true, wantRead: true, wantAllow: []string{"a.example", "b.example"}, wantDeny: true, wantPassthrough: []string{"pinned.example"}},
		{name: "deny only keeps the list", args: []string{"--egress-deny-by-default=false"}, wantSent: true, wantRead: true, wantAllow: []string{"github.com"}, wantPassthrough: []string{"pinned.example"}},
		{name: "clearing empties the list", args: []string{"--clear-egress-allow"}, wantSent: true, wantRead: true, wantAllow: []string{}, wantDeny: true, wantPassthrough: []string{"pinned.example"}},
		{name: "every part given reads nothing", args: []string{"--egress-allow", "x.example", "--egress-deny-by-default", "--clear-egress-passthrough"}, wantSent: true, wantAllow: []string{"x.example"}, wantDeny: true, wantPassthrough: []string{}},
		{name: "passthrough only keeps the rest", args: []string{"--egress-passthrough", "db.corp.example"}, wantSent: true, wantRead: true, wantAllow: []string{"github.com"}, wantDeny: true, wantPassthrough: []string{"db.corp.example"}},
		{name: "clearing passthrough empties it", args: []string{"--clear-egress-passthrough"}, wantSent: true, wantRead: true, wantAllow: []string{"github.com"}, wantDeny: true, wantPassthrough: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var flags egressFlags
			cmd := &cobra.Command{Use: "update"}
			addEgressUpdateFlags(cmd, &flags, "the project")
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			read := false
			level, sent, err := flags.update(cmd, func() (apimodel.EgressPolicy, error) {
				read = true
				return stored, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if sent != tc.wantSent || read != tc.wantRead {
				t.Fatalf("sent %v read %v, want %v %v", sent, read, tc.wantSent, tc.wantRead)
			}
			if !sent {
				return
			}
			allow, _ := level.Allow.Get()
			if !slices.Equal(allow, tc.wantAllow) || level.DenyByDefault.Or(false) != tc.wantDeny {
				t.Fatalf("level = %v deny %v, want %v deny %v", allow, level.DenyByDefault.Or(false), tc.wantAllow, tc.wantDeny)
			}
			if passthrough, _ := level.Passthrough.Get(); !slices.Equal(passthrough, tc.wantPassthrough) {
				t.Fatalf("passthrough = %v, want %v", passthrough, tc.wantPassthrough)
			}
			if level.Passthrough.Set != (tc.wantPassthrough != nil) {
				t.Fatalf("passthrough sent = %v, want %v", level.Passthrough.Set, tc.wantPassthrough != nil)
			}
		})
	}
}

// An empty entry is refused, not dropped: emptying a list is
// --clear-egress-allow, and an empty one is more likely a variable that
// expanded to nothing.
func TestEgressFlagsRefuseAnEmptyEntry(t *testing.T) {
	for _, args := range [][]string{{"--egress-allow", ""}, {"--egress-allow", "a.example,,b.example"}, {"--egress-allow", " "}, {"--egress-passthrough", ""}} {
		var flags egressFlags
		cmd := &cobra.Command{Use: "update"}
		addEgressUpdateFlags(cmd, &flags, "the project")
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		if _, _, err := flags.update(cmd, func() (apimodel.EgressPolicy, error) { return apimodel.EgressPolicy{}, nil }); err == nil {
			t.Fatalf("update %q was accepted", args)
		}
		if _, _, err := flags.create(cmd); err == nil {
			t.Fatalf("create %q was accepted", args)
		}
	}
}

// A level that never named passthrough does not send it, so an update of the
// allow list still reaches a server that predates passthrough, which refuses
// a field it does not know (ADR 26-10-31-819 §6).
func TestEgressFlagsSendPassthroughOnlyWhenItIsThere(t *testing.T) {
	stored := apimodel.EgressPolicy{Allow: apiclientgen.NewOptNilStringArray([]string{"github.com"})}
	var flags egressFlags
	cmd := &cobra.Command{Use: "update"}
	addEgressUpdateFlags(cmd, &flags, "the project")
	if err := cmd.ParseFlags([]string{"--egress-deny-by-default"}); err != nil {
		t.Fatal(err)
	}
	level, _, err := flags.update(cmd, func() (apimodel.EgressPolicy, error) { return stored, nil })
	if err != nil {
		t.Fatal(err)
	}
	if level.Passthrough.Set {
		t.Fatalf("passthrough was sent: %v", level.Passthrough)
	}
	var create egressFlags
	createCmd := &cobra.Command{Use: "create"}
	addEgressFlags(createCmd, &create, "the discobox")
	if err := createCmd.ParseFlags([]string{"--egress-allow", "github.com"}); err != nil {
		t.Fatal(err)
	}
	if level, _, err := create.create(createCmd); err != nil || level.Passthrough.Set {
		t.Fatalf("create sent passthrough %v (%v)", level.Passthrough, err)
	}
}
