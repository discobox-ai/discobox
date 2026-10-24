package cli

import (
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The command tree spells the same thing the same way everywhere: removing is
// `rm`, listing is `ls`, a group is named in the singular, a discobox is a
// DISCOBOX argument, a list flag is emptied by --clear-<flag>, and help reads
// as sentences. A new command that breaks one of these is caught here rather than
// in the next review of the whole tree.
func TestCommandConventions(t *testing.T) {
	// A plural group the cleanup has not decided on yet: a root command the
	// in-box skills teach.
	pluralGroups := []string{"tools"}
	walkCommands(NewRootCommand(), func(cmd *cobra.Command) {
		path := cmd.CommandPath()
		switch cmd.Name() {
		case "delete", "remove", "revoke", "list":
			t.Errorf("%s: %q is an alias, not a command name; name it rm or ls", path, cmd.Name())
		case "rm":
			for _, alias := range []string{"delete", "remove"} {
				if !slices.Contains(cmd.Aliases, alias) {
					t.Errorf("%s: rm is missing the %q alias", path, alias)
				}
			}
		case "ls":
			if !slices.Contains(cmd.Aliases, "list") {
				t.Errorf("%s: ls is missing the \"list\" alias", path)
			}
			if cmd.Flags().Lookup("quiet") == nil {
				t.Errorf("%s: ls has no -q/--quiet", path)
			}
		}
		if singular := strings.TrimSuffix(cmd.Name(), "s"); singular != cmd.Name() && slices.Contains(cmd.Aliases, singular) && !slices.Contains(pluralGroups, cmd.Name()) {
			t.Errorf("%s: a group is named in the singular, %q, with the plural as an alias", path, singular)
		}
		cmd.Flags().VisitAll(func(flag *pflag.Flag) {
			if flag.Name == "discobox-id" {
				t.Errorf("%s: --discobox-id; a command names its discobox as a DISCOBOX argument, and a filter as --discobox", path)
			}
			cleared, ok := strings.CutPrefix(flag.Name, "clear-")
			if !ok {
				return
			}
			list := cmd.Flags().Lookup(cleared)
			if list == nil || !strings.HasSuffix(list.Value.Type(), "Slice") && !strings.HasSuffix(list.Value.Type(), "Array") {
				t.Errorf("%s: --%s clears no list flag --%s", path, flag.Name, cleared)
			}
		})
		if strings.ContainsAny(cmd.Use, "<>") {
			t.Errorf("%s: Use %q writes an argument as <x>; write it as X", path, cmd.Use)
		}
		if cmd.Hidden {
			return
		}
		r, _ := utf8.DecodeRuneInString(cmd.Short)
		if !unicode.IsUpper(r) {
			t.Errorf("%s: Short %q does not start with a capital letter", path, cmd.Short)
		}
	})
}

func walkCommands(cmd *cobra.Command, visit func(*cobra.Command)) {
	visit(cmd)
	for _, child := range cmd.Commands() {
		walkCommands(child, visit)
	}
}
