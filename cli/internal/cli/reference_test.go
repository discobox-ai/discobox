package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestReferencePublicSurface(t *testing.T) {
	var out bytes.Buffer
	if err := WriteReference(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "~~~~text\nRun a development tool against a discobox.") || !strings.Contains(out.String(), "\n  # name: tuicr\n") {
		t.Fatal("terminal help and script example formatting was lost")
	}
	for _, forbidden := range []string{"ADR ", "docs/adr/"} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("public help contains internal reference %q", forbidden)
		}
	}
	root, _ := newRootCommand(false)
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	count := 0
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Hidden {
			return
		}
		count++
		heading := "## " + cmd.CommandPath() + "\n"
		_, section, ok := strings.Cut(out.String(), heading)
		if !ok {
			t.Fatalf("missing command %s", cmd.CommandPath())
		}
		section, _, _ = strings.Cut(section, "\n## ")
		cmd.InitDefaultHelpFlag()
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		flags.AddFlagSet(cmd.LocalNonPersistentFlags())
		flags.AddFlagSet(cmd.PersistentFlags())
		flags.AddFlagSet(cmd.InheritedFlags())
		flags.VisitAll(func(f *pflag.Flag) {
			if f.Hidden {
				return
			}
			if !strings.Contains(section, "--"+f.Name+" ") && !strings.Contains(section, "--"+f.Name+",") {
				t.Errorf("%s missing flag %s", cmd.CommandPath(), f.Name)
			}
		})
		for _, alias := range cmd.Aliases {
			if !strings.Contains(section, alias) {
				t.Errorf("%s missing alias %s", cmd.CommandPath(), alias)
			}
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
	if got := strings.Count(out.String(), "\n## "); got != count {
		t.Errorf("exported %d headings, want %d public commands", got, count)
	}
	for _, hidden := range []string{"## discobox tui\n", "| --iroh-relay |"} {
		if strings.Contains(out.String(), hidden) {
			t.Errorf("unexpected hidden surface %s", hidden)
		}
	}
	for _, needle := range []string{"## discobox help\n", "## discobox completion\n"} {
		if !strings.Contains(out.String(), needle) {
			t.Errorf("missing initialized command %s", needle)
		}
	}
}
