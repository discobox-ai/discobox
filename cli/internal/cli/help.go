package cli

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Help is drawn the way rex draws it (ADR 26-11-04-204 §1): the root's
// commands under groups, bold section headers, cyan command and flag names,
// a flag's value as an uppercase name, and a command's own flags apart from
// the global ones it inherits. One writer draws every command's help; a
// command that sets a help function of its own (`admin provider create`)
// keeps it.

// colorWhen is --color: whether the CLI colors what it draws on its own
// output.
type colorWhen string

const (
	colorAuto   colorWhen = "auto"
	colorAlways colorWhen = "always"
	colorNever  colorWhen = "never"
)

func (c *colorWhen) String() string { return string(*c) }
func (c *colorWhen) Type() string   { return "WHEN" }

func (c *colorWhen) Set(value string) error {
	switch when := colorWhen(strings.ToLower(strings.TrimSpace(value))); when {
	case colorAuto, colorAlways, colorNever:
		*c = when
		return nil
	}
	return fmt.Errorf("must be auto, always, or never")
}

// colors reports whether out is drawn in color: always, never, or — for auto —
// when it is a terminal that shows color, NO_COLOR included (streamHasColor).
func (c colorWhen) colors(out io.Writer) bool {
	switch c {
	case colorAlways:
		return true
	case colorNever:
		return false
	}
	return isTerminalStream(out) && streamHasColor(out)
}

// writer is out drawn through the color profile --color leaves it: the
// terminal's own for auto, none for never, and at least 256 colors for always.
func (c colorWhen) writer(out io.Writer) *colorprofile.Writer {
	writer := colorprofile.NewWriter(out, os.Environ())
	switch c {
	case colorNever:
		writer.Profile = colorprofile.NoTTY
	case colorAlways:
		writer.Profile = max(writer.Profile, colorprofile.ANSI256)
	}
	return writer
}

// The root's command groups, in the order help lists them.
var rootCommandGroups = []*cobra.Group{
	{ID: "basic", Title: "Basic commands:"},
	{ID: "automation", Title: "Automation:"},
	{ID: "interface", Title: "Interface:"},
	{ID: "files", Title: "Files & ports:"},
	{ID: "resources", Title: "Resources:"},
	{ID: "setup", Title: "Setup:"},
	{ID: "other", Title: "Other:"},
}

// inGroup puts cmd in the root's group id, for help to list it under.
func inGroup(id string, cmd *cobra.Command) *cobra.Command {
	cmd.GroupID = id
	return cmd
}

// hiddenSpelling makes cmd a spelling that keeps working but that help does
// not teach: a hidden second registration under name, since cobra prints every
// alias a command has (cli/DESIGN.md "Command Naming").
func hiddenSpelling(name string, cmd *cobra.Command) *cobra.Command {
	cmd.Use = name + strings.TrimPrefix(cmd.Use, cmd.Name())
	cmd.Aliases = nil
	cmd.Hidden = true
	cmd.GroupID = ""
	return cmd
}

// helpStyle draws help's two emphases, or nothing without color.
type helpStyle bool

func (s helpStyle) header(text string) string {
	if !s {
		return text
	}
	return "\x1b[1m" + text + "\x1b[0m"
}

func (s helpStyle) name(text string) string {
	if !s {
		return text
	}
	return "\x1b[36m" + text + "\x1b[0m"
}

// installHelp makes the root draw every command's help and usage with
// writeHelp, colored as --color says for the stream it lands on.
func (a *App) installHelp(root *cobra.Command) {
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		out := cmd.OutOrStdout()
		writeHelp(out, cmd, helpStyle(a.color.colors(out)), true)
	})
	root.SetUsageFunc(func(cmd *cobra.Command) error {
		out := cmd.OutOrStderr()
		writeHelp(out, cmd, helpStyle(a.color.colors(out)), false)
		return nil
	})
}

// writeHelp writes cmd's help: its description and examples when full, then
// its usage, the commands under it, its options and the global ones.
func writeHelp(out io.Writer, cmd *cobra.Command, style helpStyle, full bool) {
	var b strings.Builder
	if full {
		if text := strings.TrimRightFunc(cmp.Or(cmd.Long, cmd.Short), unicode.IsSpace); text != "" {
			b.WriteString(text)
			b.WriteString("\n\n")
		}
		if cmd.Example != "" {
			b.WriteString(style.header("Examples:"))
			b.WriteString("\n")
			b.WriteString(strings.TrimRightFunc(cmd.Example, unicode.IsSpace))
			b.WriteString("\n\n")
		}
	}

	b.WriteString(style.header("Usage:"))
	b.WriteString("\n")
	subcommands := cmd.HasAvailableSubCommands()
	if cmd.Runnable() && (!subcommands || cmd.HasParent()) {
		b.WriteString("  ")
		b.WriteString(cmd.UseLine())
		b.WriteString("\n")
	}
	if subcommands {
		b.WriteString("  ")
		b.WriteString(cmd.CommandPath())
		b.WriteString(" [command] [flags]\n")
	}
	if len(cmd.Aliases) > 0 {
		b.WriteString("\n")
		b.WriteString(style.header("Aliases:"))
		b.WriteString("\n  ")
		b.WriteString(strings.Join(append([]string{cmd.Name()}, cmd.Aliases...), ", "))
		b.WriteString("\n")
	}

	if subcommands {
		writeCommandList(&b, cmd, style)
	}

	local, inherited := helpFlags(cmd.LocalFlags()), helpFlags(cmd.InheritedFlags())
	width := 0
	for _, row := range append(slices.Clone(local), inherited...) {
		width = max(width, len(row.name))
	}
	if len(local) > 0 {
		b.WriteString("\n")
		b.WriteString(style.header("Options:"))
		b.WriteString("\n")
		writeFlagRows(&b, local, width, style)
	}
	if len(inherited) > 0 {
		b.WriteString("\n")
		b.WriteString(style.header("Global options:"))
		b.WriteString("\n")
		writeFlagRows(&b, inherited, width, style)
	}
	if subcommands {
		fmt.Fprintf(&b, "\nUse \"%s [command] --help\" for more information about a command.\n", cmd.CommandPath())
	}
	_, _ = io.WriteString(out, b.String())
}

// writeCommandList lists the commands under cmd, by group in the order the
// groups were added, then any in none under "Commands:".
func writeCommandList(b *strings.Builder, cmd *cobra.Command, style helpStyle) {
	type row struct{ name, short string }
	byGroup := map[string][]row{}
	width := 0
	for _, sub := range cmd.Commands() {
		if !sub.IsAvailableCommand() && sub.Name() != "help" {
			continue
		}
		name := strings.Join(append([]string{sub.Name()}, sub.Aliases...), ", ")
		width = max(width, len(name))
		byGroup[sub.GroupID] = append(byGroup[sub.GroupID], row{name, sub.Short})
	}
	write := func(title string, rows []row) {
		if len(rows) == 0 {
			return
		}
		b.WriteString("\n")
		b.WriteString(style.header(title))
		b.WriteString("\n")
		for _, r := range rows {
			b.WriteString("  ")
			b.WriteString(style.name(r.name))
			b.WriteString(strings.Repeat(" ", width-len(r.name)+4))
			b.WriteString(r.short)
			b.WriteString("\n")
		}
	}
	for _, group := range cmd.Groups() {
		write(group.Title, byGroup[group.ID])
	}
	write("Commands:", byGroup[""])
}

// helpFlag is one flag's row: its names and value, and its description.
type helpFlag struct{ name, usage string }

// helpFlags is the rows of the flags help shows, sorted by name as pflag
// sorts them.
func helpFlags(flags *pflag.FlagSet) []helpFlag {
	var rows []helpFlag
	flags.VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden {
			return
		}
		name := "    "
		if flag.Shorthand != "" && flag.ShorthandDeprecated == "" {
			name = "-" + flag.Shorthand + ", "
		}
		name = "  " + name + "--" + flag.Name
		if value := flagValueName(flag); value != "" {
			if flag.NoOptDefVal != "" {
				value = "[" + value + "]"
			}
			name += " " + value
		}
		_, usage := pflag.UnquoteUsage(flag)
		usage = lowerFirst(usage)
		if shown := flagDefault(flag); shown != "" {
			usage += " (default " + shown + ")"
		}
		rows = append(rows, helpFlag{name: name, usage: usage})
	})
	return rows
}

func writeFlagRows(b *strings.Builder, rows []helpFlag, width int, style helpStyle) {
	for _, row := range rows {
		// The leading spaces are layout, not the name: only the name is colored.
		indent := len(row.name) - len(strings.TrimLeft(row.name, " "))
		b.WriteString(row.name[:indent])
		b.WriteString(style.name(row.name[indent:]))
		b.WriteString(strings.Repeat(" ", width-len(row.name)+2))
		b.WriteString(row.usage)
		b.WriteString("\n")
	}
}

// flagValueName is the uppercase name help writes after a flag that takes a
// value: the word its usage backquotes (pflag's convention), else one made
// from what it takes — DURATION, N for a number, its own name for a string —
// and none for a switch.
func flagValueName(flag *pflag.Flag) string {
	name, _ := pflag.UnquoteUsage(flag)
	if strings.Contains(flag.Usage, "`") {
		return strings.ToUpper(name)
	}
	switch flag.Value.Type() {
	case "bool":
		return ""
	case "string", "stringSlice", "stringArray":
		return strings.ToUpper(strings.ReplaceAll(flag.Name, "-", "_"))
	case "duration":
		return "DURATION"
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64", "count":
		return "N"
	}
	return strings.ToUpper(flag.Value.Type())
}

// flagDefault is what help says a flag defaults to, or empty for a value that
// says nothing: a zero, an empty list, a switch left off.
func flagDefault(flag *pflag.Flag) string {
	switch flag.DefValue {
	case "", "false", "0", "0s", "[]", "<nil>":
		return ""
	}
	return flag.DefValue
}

// lowerFirst writes a description the way rex writes a flag's, as a lowercase
// fragment, leaving a word that is all capitals (HTTP, ID) as it is.
func lowerFirst(text string) string {
	first, size := utf8.DecodeRuneInString(text)
	if size == 0 || !unicode.IsUpper(first) {
		return text
	}
	if next, _ := utf8.DecodeRuneInString(text[size:]); unicode.IsUpper(next) {
		return text
	}
	return string(unicode.ToLower(first)) + text[size:]
}
