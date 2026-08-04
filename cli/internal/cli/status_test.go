package cli

import (
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestStatusOptionsGitArgs(t *testing.T) {
	opts := statusOptions{
		short:            true,
		branch:           true,
		zero:             true,
		noRenames:        true,
		porcelain:        "v2",
		untrackedFiles:   "no",
		ignored:          "matching",
		ignoreSubmodules: "dirty",
		column:           "always",
		verbose:          2,
	}
	got := strings.Join(opts.gitArgs(), " ")
	want := "--short --branch --null --no-renames --porcelain=v2 --column=always " +
		"--untracked-files=no --ignored=matching --ignore-submodules=dirty --verbose --verbose"
	if got != want {
		t.Fatalf("git args: got %q, want %q", got, want)
	}
	if unset := (statusOptions{}).gitArgs(); len(unset) != 0 {
		t.Fatalf("untouched options should add no arguments, got %v", unset)
	}
	// The negatable pairs are passed on only as written: an unset --renames must
	// not turn into --no-renames, which would change what git does.
	if args := (statusOptions{aheadBehind: true}).gitArgs(); strings.Join(args, " ") != "--ahead-behind" {
		t.Fatalf("--ahead-behind alone: got %v", args)
	}
	if args := (statusOptions{noAheadBehind: true}).gitArgs(); strings.Join(args, " ") != "--no-ahead-behind" {
		t.Fatalf("--no-ahead-behind alone: got %v", args)
	}
}

func TestStatusOptionsMachineReadable(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts statusOptions
		want bool
	}{
		{"plain", statusOptions{}, false},
		{"short is still for reading", statusOptions{short: true}, false},
		{"porcelain", statusOptions{porcelain: "v1"}, true},
		{"nul separated", statusOptions{zero: true}, true},
	} {
		if got := tc.opts.machineReadable(); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStatusCommand(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gitArgs   []string
		color     string
		pathspecs []string
		want      string
	}{
		{
			name:  "bare",
			color: "auto",
			want:  "git --no-pager status",
		},
		{
			name:    "flags are passed through in git's spelling",
			gitArgs: []string{"--short", "--branch"},
			color:   "auto",
			want:    "git --no-pager status --short --branch",
		},
		{
			// git status has no --color flag of its own, so the setting is the
			// one git itself would set.
			name:  "explicit color is config, not a flag",
			color: "always",
			want:  "git -c color.status=always --no-pager status",
		},
		{
			name:      "pathspecs come after a separator",
			color:     "never",
			pathspecs: []string{"cli/", "server/"},
			want:      "git -c color.status=never --no-pager status -- cli/ server/",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(statusCommand(tc.gitArgs, tc.color, tc.pathspecs), " ")
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStatusCommandParsing pins the argument shape: an optional sandbox ID,
// git's flags in either order around it, and pathspecs only after --.
func TestStatusCommandParsing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantErr   string
		wantDash  int
		wantFirst string
	}{
		{name: "no arguments", args: nil, wantDash: -1},
		{name: "sandbox only", args: []string{"sbx_01hq"}, wantDash: -1, wantFirst: "sbx_01hq"},
		{name: "flags before the sandbox", args: []string{"-s", "sbx_01hq"}, wantDash: -1, wantFirst: "sbx_01hq"},
		{name: "flags after the sandbox", args: []string{"sbx_01hq", "--short"}, wantDash: -1, wantFirst: "sbx_01hq"},
		{name: "pathspecs", args: []string{"sbx_01hq", "--", "cli/"}, wantDash: 1, wantFirst: "sbx_01hq"},
		{name: "pathspecs without a sandbox", args: []string{"--", "cli/"}, wantDash: 0},
		{name: "two sandboxes", args: []string{"a", "b"}, wantErr: "at most one sandbox ID"},
		{name: "mode flags need an attached value", args: []string{"-uno"}, wantErr: "unknown shorthand"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var app App
			cmd := app.newStatusCommand()
			var gotArgs []string
			cmd.RunE = func(_ *cobra.Command, args []string) error {
				gotArgs = args
				return nil
			}
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got error %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cmd.ArgsLenAtDash(); got != tc.wantDash {
				t.Fatalf("ArgsLenAtDash: got %d, want %d", got, tc.wantDash)
			}
			var first string
			if len(gotArgs) > 0 && cmd.ArgsLenAtDash() != 0 {
				first = gotArgs[0]
			}
			if first != tc.wantFirst {
				t.Fatalf("sandbox argument: got %q, want %q", first, tc.wantFirst)
			}
		})
	}
}
