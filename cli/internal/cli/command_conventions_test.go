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
// DISCOBOX argument, a list flag is emptied by --clear-<flag>, a secret is
// bound rather than granted, a binding's words are --as, --route, --for and
// --project-wide, no command's flag reuses a root flag's name, and help reads
// as sentences. A new command that breaks one of these is caught here rather than
// in the next review of the whole tree.
func TestCommandConventions(t *testing.T) {
	// A plural group the cleanup has not decided on yet: a root command the
	// in-box skills teach.
	pluralGroups := []string{"tools"}
	root := NewRootCommand()
	global := map[string]bool{}
	root.PersistentFlags().VisitAll(func(flag *pflag.Flag) { global[flag.Name] = true })
	walkCommands(root, func(cmd *cobra.Command) {
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
		// A secret is bound, not granted (ADR 26-10-27-880): the word is gone
		// from the tree, so it does not come back as a command or a flag.
		if strings.Contains(cmd.Name(), "grant") {
			t.Errorf("%s: a secret is bound, not granted; name it for the binding", path)
		}
		// What a person binds a secret to is a DISCOBOX argument, --harness or
		// --project-wide: the same three on every `secret` verb that names one.
		if cmd.HasParent() && cmd.Parent().Name() == "secret" && cmd.Flags().Lookup("harness") != nil {
			if cmd.Flags().Lookup("project-wide") == nil || !strings.Contains(cmd.Use, "[DISCOBOX]") {
				t.Errorf("%s: a binding's target is [DISCOBOX], --harness or --project-wide", path)
			}
		}
		// A binding is spelled one way, by a person's verb or the raw one:
		// its variable --as, its routes --route, its lifetime --for.
		if strings.Contains(path, " binding") || (cmd.HasParent() && cmd.Parent().Name() == "secret" && (cmd.Name() == "bind" || cmd.Name() == "unbind")) || strings.HasSuffix(path, "secret request approve") {
			for _, other := range []string{"to", "env", "ttl"} {
				if cmd.Flags().Lookup(other) != nil {
					t.Errorf("%s: --%s; a binding's words are --as, --route and --for", path, other)
				}
			}
		}
		// A command's own flag never takes a root flag's name: it would shadow
		// it, and `--project acme` would stop meaning which project.
		if cmd != root {
			cmd.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) {
				// `secret create --token` is the value and predates the rule;
				// the root's --token is hidden. Not a new exception.
				if global[flag.Name] && (flag.Name != "token" || cmd.Parent().Name() != "secret") {
					t.Errorf("%s: --%s shadows the root's --%s", path, flag.Name, flag.Name)
				}
			})
		}
		cmd.Flags().VisitAll(func(flag *pflag.Flag) {
			if strings.Contains(flag.Name, "grant") {
				t.Errorf("%s: --%s; a secret is bound, not granted", path, flag.Name)
			}
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
