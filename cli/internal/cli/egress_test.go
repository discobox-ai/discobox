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
	}
	for _, tc := range []struct {
		name      string
		args      []string
		wantSent  bool
		wantRead  bool
		wantAllow []string
		wantDeny  bool
	}{
		{name: "neither flag", args: nil},
		{name: "allow only keeps the restriction", args: []string{"--egress-allow", "a.example,b.example"}, wantSent: true, wantRead: true, wantAllow: []string{"a.example", "b.example"}, wantDeny: true},
		{name: "deny only keeps the list", args: []string{"--egress-deny-by-default=false"}, wantSent: true, wantRead: true, wantAllow: []string{"github.com"}},
		{name: "an empty allow clears the list", args: []string{"--egress-allow", ""}, wantSent: true, wantRead: true, wantAllow: []string{}, wantDeny: true},
		{name: "both given reads nothing", args: []string{"--egress-allow", "x.example", "--egress-deny-by-default"}, wantSent: true, wantAllow: []string{"x.example"}, wantDeny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var flags egressFlags
			cmd := &cobra.Command{Use: "update"}
			addEgressFlags(cmd, &flags, "the project")
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
		})
	}
}
