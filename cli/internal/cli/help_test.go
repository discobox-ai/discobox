package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// rootHelp is `discobox ARGS --help` as printed into a buffer.
func rootHelp(t *testing.T, args ...string) string {
	t.Helper()
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append(args, "--help"))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v --help: %v", args, err)
	}
	return out.String()
}

// section is the lines of help under header, up to the next blank line.
func section(help, header string) []string {
	_, rest, ok := strings.Cut(help, "\n"+header+"\n")
	if !ok {
		return nil
	}
	body, _, _ := strings.Cut(rest, "\n\n")
	return strings.Split(strings.TrimRight(body, "\n"), "\n")
}

// The root lists its commands under groups, in order, each command once
// (ADR 26-11-04-204 §1).
func TestRootHelpListsCommandsByGroup(t *testing.T) {
	help := rootHelp(t)
	groups := []struct {
		title    string
		commands []string
	}{
		{"Basic commands:", []string{"run", "ls", "attach", "exec", "start", "stop", "restart", "rm", "rename", "tag"}},
		{"Automation:", []string{"send", "send-key", "capture", "wait", "events", "adopt", "api-access", "release"}},
		{"Interface:", []string{"console"}},
		{"Files & ports:", []string{"cp", "push", "proxy", "tools"}},
		{"Resources:", []string{"secret", "trust"}},
		{"Setup:", []string{"configure", "admin"}},
		{"Other:", []string{"completion", "version", "help"}},
	}
	at := 0
	for _, group := range groups {
		next := strings.Index(help[at:], "\n"+group.title+"\n")
		if next < 0 {
			t.Fatalf("help has no %q after the groups before it:\n%s", group.title, help)
		}
		at += next
		var listed []string
		for _, line := range section(help, group.title) {
			name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
			listed = append(listed, strings.TrimSuffix(name, ","))
		}
		if strings.Join(listed, " ") != strings.Join(group.commands, " ") {
			t.Errorf("%s lists %q, want %q", group.title, listed, group.commands)
		}
	}
	if strings.Contains(help, "\nCommands:\n") {
		t.Errorf("a root command is in no group:\n%s", section(help, "Commands:"))
	}
	if !strings.Contains(help, "\n  ls, list ") || !strings.Contains(help, "\nUsage:\n  discobox [command] [flags]\n") {
		t.Errorf("help does not write aliases inline, or the root's usage line:\n%s", help)
	}
}

// What help stops offering keeps working: the transport flags, ps, shell, id
// and tui are hidden, and id is `admin id` now.
func TestRootHelpHidesWhatKeepsWorking(t *testing.T) {
	help := rootHelp(t)
	for _, flag := range []string{"--auto-start-server", "--iroh-log", "--iroh-relay", "--project"} {
		if strings.Contains(help, flag) {
			t.Errorf("help offers %s", flag)
		}
	}
	for _, line := range strings.Split(help, "\n") {
		names, _, _ := strings.Cut(strings.TrimSpace(line), "  ")
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(names, "-") {
			continue
		}
		for _, name := range strings.Split(names, ", ") {
			switch name {
			case "ps", "shell", "id", "tui":
				t.Errorf("help lists %q: %q", name, line)
			}
		}
	}
	root := NewRootCommand()
	for _, path := range [][]string{{"ps"}, {"shell"}, {"id"}, {"tui"}, {"admin", "id"}} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Name() != path[len(path)-1] {
			t.Errorf("%v no longer runs: %v %v", path, cmd, err)
		}
	}
	root.SetArgs([]string{"--auto-start-server=false", "--iroh-log", "off", "--iroh-relay", "", "help"})
	root.SetOut(new(bytes.Buffer))
	if err := root.Execute(); err != nil {
		t.Errorf("the hidden flags no longer parse: %v", err)
	}
}

// A command's own flags are under Options and the global ones it inherits
// under Global options, each value an uppercase name, aligned in one column.
func TestCommandHelpSplitsOptionsFromGlobalOptions(t *testing.T) {
	help := rootHelp(t, "wait")
	options, global := section(help, "Options:"), section(help, "Global options:")
	want := map[string][]string{
		"Options:":        {"  -t, --terminal TERMINAL ", "      --until UNTIL ", "      --for DURATION ", "      --after POINT "},
		"Global options:": {"      --color WHEN ", "  -o, --output FORMAT ", "      --server SERVER ", "      --token TOKEN "},
	}
	for header, lines := range map[string][]string{"Options:": options, "Global options:": global} {
		joined := strings.Join(lines, "\n")
		for _, flag := range want[header] {
			if !strings.Contains(joined, "\n"+flag) && !strings.HasPrefix(joined, flag) {
				t.Errorf("%s does not list %q:\n%s", header, flag, joined)
			}
		}
	}
	column := -1
	for _, line := range append(options, global...) {
		rest := line[len(flagPart(line)):]
		at := len(line) - len(strings.TrimLeft(rest, " "))
		if column < 0 {
			column = at
		} else if at != column {
			t.Errorf("descriptions are not aligned at column %d: %q", column, line)
		}
	}
	if !strings.Contains(help, "\nExamples:\n") || strings.Index(help, "\nExamples:\n") > strings.Index(help, "\nUsage:\n") {
		t.Errorf("wait's examples are not ahead of its usage:\n%s", help)
	}
	if !strings.Contains(help, "\n  discobox wait DISCOBOX [flags]\n") {
		t.Errorf("wait's usage line is not its own:\n%s", help)
	}
}

// flagPart is a flag row's names and value, up to the gap before its
// description.
func flagPart(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	indent := len(line) - len(trimmed)
	name, _, _ := strings.Cut(trimmed, "  ")
	return line[:indent] + name
}

// --color decides whether help is colored: never and auto into a buffer are
// plain, always is bold headers and cyan names.
func TestHelpColorFollowsTheColorFlag(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, args := range [][]string{nil, {"--color", "never"}, {"--color", "auto"}} {
		if help := rootHelp(t, args...); strings.Contains(help, "\x1b[") {
			t.Errorf("%v --help is colored", args)
		}
	}
	help := rootHelp(t, "--color", "always")
	for _, drawn := range []string{"\x1b[1mBasic commands:\x1b[0m", "\x1b[36mrun, new, create, r, n\x1b[0m", "      \x1b[36m--color WHEN\x1b[0m"} {
		if !strings.Contains(help, drawn) {
			t.Errorf("--color always --help does not draw %q:\n%q", drawn, help)
		}
	}
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"--color", "sometimes", "--help"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "must be auto, always, or never") {
		t.Errorf("--color sometimes: err = %v", err)
	}
}

// A token given in the environment is a credential: help never prints it as
// --token's default.
func TestHelpNeverPrintsTheTokenFromTheEnvironment(t *testing.T) {
	t.Setenv("DISCOBOX_TOKEN", "secret-token-value")
	if help := rootHelp(t); strings.Contains(help, "secret-token-value") {
		t.Fatalf("help prints the environment's token:\n%s", help)
	}
}

// A value's name is the word a usage backquotes, else made from what the
// flag takes; a description is a lowercase fragment unless it opens on an
// acronym.
func TestHelpValueNamesAndDescriptions(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	cmd.Flags().String("git-user-name", "", "Name commits")
	cmd.Flags().Duration("idle", 0, "Wait this long")
	cmd.Flags().Int("count", 0, "How many")
	cmd.Flags().Bool("all", false, "Every one")
	cmd.Flags().String("body", "", "print the `ID`'s body")
	for name, want := range map[string]string{"git-user-name": "GIT_USER_NAME", "idle": "DURATION", "count": "N", "all": "", "body": "ID"} {
		if got := flagValueName(cmd.Flags().Lookup(name)); got != want {
			t.Errorf("--%s value name = %q, want %q", name, got, want)
		}
	}
	for text, want := range map[string]string{"Print it": "print it", "HTTP requests": "HTTP requests", "ID of it": "ID of it", "already": "already", "": ""} {
		if got := lowerFirst(text); got != want {
			t.Errorf("lowerFirst(%q) = %q, want %q", text, got, want)
		}
	}
}

// --color reaches what draws its own color too: never strips it, always keeps
// it into a pipe.
func TestColorWriterFollowsTheColorFlag(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	painted := "\x1b[1mbold\x1b[0m"
	for when, want := range map[colorWhen]string{colorNever: "bold", colorAlways: "\x1b[1mbold\x1b[m", colorAuto: "bold"} {
		var out bytes.Buffer
		_, _ = when.writer(&out).WriteString(painted)
		if out.String() != want {
			t.Errorf("%s: wrote %q, want %q", when, out.String(), want)
		}
	}
}
