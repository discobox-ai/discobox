package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
	"github.com/spf13/cobra"
)

// harnessChangeAction says what a save does to a change's file. A removal
// the harness has its own file for resets to it; a file with nothing before
// it is added, not updated.
func harnessChangeAction(f api.HarnessCaptureFileChange) string {
	switch {
	case f.Remove.Or(false) && f.File.IsSet():
		return "reset"
	case f.Remove.Or(false):
		return "remove"
	case !f.Previous.IsSet():
		return "add"
	}
	return "update"
}

// harnessDiffLines is the most lines, both sides together, compared line by
// line. The comparison's memory grows with the lines times the edits, and the
// file comes from a box; past this it is shown replaced whole, in linear time.
const harnessDiffLines = 1000

// harnessChangeDiff is a change's hunks, as a unified diff from the file a save
// replaces to the one it leaves, made safe for a terminal. The file headers
// are left off: the action line above the hunks already names the file.
func harnessChangeDiff(f api.HarnessCaptureFileChange) string {
	before := diffText(f.Previous.Or(api.HarnessCaptureFile{}).Content)
	after := diffText(f.File.Or(api.HarnessCaptureFile{}).Content)
	if before == after {
		return ""
	}
	var hunks []*gotextdiff.Hunk
	if strings.Count(before, "\n")+strings.Count(after, "\n") > harnessDiffLines {
		whole := &gotextdiff.Hunk{FromLine: 1, ToLine: 1}
		for _, l := range diffLines(before) {
			whole.Lines = append(whole.Lines, gotextdiff.Line{Kind: gotextdiff.Delete, Content: l})
		}
		for _, l := range diffLines(after) {
			whole.Lines = append(whole.Lines, gotextdiff.Line{Kind: gotextdiff.Insert, Content: l})
		}
		hunks = []*gotextdiff.Hunk{whole}
	} else {
		hunks = gotextdiff.ToUnified(f.Path, f.Path, before, myers.ComputeEdits(span.URIFromPath(f.Path), before, after)).Hunks
	}
	var out strings.Builder
	for _, hunk := range hunks {
		from, to := 0, 0
		for _, l := range hunk.Lines {
			if l.Kind != gotextdiff.Insert {
				from++
			}
			if l.Kind != gotextdiff.Delete {
				to++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", unifiedRange(hunk.FromLine, from), unifiedRange(hunk.ToLine, to))
		for _, l := range hunk.Lines {
			mark := " "
			switch l.Kind {
			case gotextdiff.Insert:
				mark = "+"
			case gotextdiff.Delete:
				mark = "-"
			}
			out.WriteString(mark + strings.TrimSuffix(l.Content, "\n") + "\n")
			if !strings.HasSuffix(l.Content, "\n") {
				out.WriteString("\\ No newline at end of file\n")
			}
		}
	}
	return terminalSafeMultiline(strings.TrimSuffix(out.String(), "\n"))
}

// harnessDiffIndentBytes is the most a side may grow to when it is
// re-indented. Indenting writes each value on a line of its own as deep as it
// is nested, so a small, deeply nested document from a box would grow by its
// depth times its values; past this it is compared as it is.
const harnessDiffIndentBytes = 4 << 20

// diffText is a side of a change as it is compared. JSON is laid out one way,
// two-space indented as the harness exporters write it: a bundle's file is
// stored as it shipped, often on one line, and the same settings exported and
// saved are indented, and a diff of the two would show every line changed for
// a change to one. Only the layout goes; a field the save drops still shows.
func diffText(content string) string {
	if !json.Valid([]byte(content)) || indentedSize(content) > harnessDiffIndentBytes {
		return content
	}
	var indented bytes.Buffer
	if json.Indent(&indented, []byte(strings.TrimSpace(content)), "", "  ") != nil {
		return content
	}
	return indented.String() + "\n"
}

// indentedSize is an upper bound on the size of valid JSON re-indented two
// spaces a level, found in one pass without writing it: the text itself plus,
// for each line break indenting adds, the break and its indent, and the space
// after each colon.
func indentedSize(content string) int {
	size, depth, inString, escaped := len(content), 0, false, false
	for i := 0; i < len(content) && size <= harnessDiffIndentBytes; i++ {
		c := content[i]
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			depth++
			size += 1 + 2*depth
		case c == '}' || c == ']':
			depth--
			size += 1 + 2*depth
		case c == ',':
			size += 1 + 2*depth
		case c == ':':
			size++
		}
	}
	return size
}

// diffLines splits text into lines that keep their line breaks.
func diffLines(text string) []string {
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// unifiedRange is one side of a hunk header. An empty side names the line
// before it, as diff(1) writes it: a new file's hunk starts -0,0.
func unifiedRange(start, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", start-1)
	case 1:
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// The candidate ID is mandatory on writes: saving never silently captures a
// different login from the immutable candidate a person reviewed.
func (a *App) newHarnessChangesCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "changes SANDBOX", Short: "Preview portable settings and logins changed in a discobox", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		project, box, client, err := a.sandboxRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.GetSandboxHarnessConfigChanges(cmd.Context(), api.GetSandboxHarnessConfigChangesParams{ProjectId: project, SandboxId: box})
		if err != nil {
			return err
		}
		response, err := expectResponse[api.HarnessConfigChanges](res)
		if err != nil {
			return err
		}
		if a.output == "json" {
			return writeJSON(cmd.OutOrStdout(), response)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s (candidate %s, revision %d)\n", terminalSafe(response.State), terminalSafe(response.CandidateId.Or("none")), response.ConfigRevision)
		for _, f := range response.Files {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", harnessChangeAction(f), terminalSafe(f.Path))
			if diff := harnessChangeDiff(f); diff != "" {
				fmt.Fprintln(cmd.OutOrStdout(), diff)
			}
		}
		for _, c := range response.Credentials {
			fmt.Fprintf(cmd.OutOrStdout(), "credential %s %s (%s)\n", terminalSafe(c.Provider), terminalSafe(c.EnvName), terminalSafe(c.Type))
		}
		if len(response.Files)+len(response.Credentials) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No saveable settings or credentials.")
		}
		for _, d := range response.Diagnostics.Or(nil) {
			label := d.Code
			if path := d.Path.Or(""); path != "" {
				label += " (" + path + ")"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", terminalSafe(label), terminalSafe(d.Message))
		}
		if candidate := response.CandidateId.Or(""); candidate != "" && response.State == "pending" {
			prefix := "discobox"
			if a.serverURL != "" {
				prefix += " --server " + shellQuote(terminalSafe(a.serverURL), runtime.GOOS == "windows")
			}
			prefix += " --project " + shellQuote(terminalSafe(project), runtime.GOOS == "windows")
			boxArg := shellQuote(terminalSafe(box), runtime.GOOS == "windows")
			candidateArg := shellQuote(terminalSafe(candidate), runtime.GOOS == "windows")
			if len(response.Files)+len(response.Credentials) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "\nSave shared defaults: %s admin harness save %s %s\n", prefix, boxArg, candidateArg)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Discard candidate: %s admin harness discard %s %s\n", prefix, boxArg, candidateArg)
		}
		return nil
	}}
	return cmd
}
func (a *App) newHarnessSaveCommand() *cobra.Command {
	return &cobra.Command{Use: "save SANDBOX CANDIDATE", Short: "Save a reviewed candidate as the harness defaults and shared credentials", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		project, box, client, err := a.sandboxRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.SaveSandboxHarnessConfigChanges(cmd.Context(), &api.HarnessConfigCandidateRequest{CandidateId: args[1]}, api.SaveSandboxHarnessConfigChangesParams{ProjectId: project, SandboxId: box})
		if err != nil {
			return err
		}
		result, err := expectResponse[api.HarnessConfigSaveResult](res)
		if err != nil {
			return err
		}
		if a.output == "json" {
			return writeJSON(cmd.OutOrStdout(), result)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s (revision %d)\n", terminalSafe(result.State), result.ConfigRevision)
		return nil
	}}
}
func (a *App) newHarnessDiscardCommand() *cobra.Command {
	return &cobra.Command{Use: "discard SANDBOX CANDIDATE", Short: "Discard a pending candidate without changing local files or shared defaults", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		project, box, client, err := a.sandboxRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.DiscardSandboxHarnessConfigChanges(cmd.Context(), &api.HarnessConfigCandidateRequest{CandidateId: args[1]}, api.DiscardSandboxHarnessConfigChangesParams{ProjectId: project, SandboxId: box})
		if err != nil {
			return err
		}
		result, err := expectResponse[api.HarnessConfigChanges](res)
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), result)
	}}
}
func (a *App) newHarnessReconsiderCommand() *cobra.Command {
	var files, credentials []string
	cmd := &cobra.Command{Use: "reconsider SANDBOX CANDIDATE", Short: "Create a new candidate against current defaults, retaining only selected local changes", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		project, box, client, err := a.sandboxRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.ReconsiderSandboxHarnessConfigChanges(cmd.Context(), &api.HarnessConfigSelection{CandidateId: args[1], KeepFiles: files, ReplaceCredentials: credentials}, api.ReconsiderSandboxHarnessConfigChangesParams{ProjectId: project, SandboxId: box})
		if err != nil {
			return err
		}
		result, err := expectResponse[api.HarnessConfigChanges](res)
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), result)
	}}
	cmd.Flags().StringArrayVar(&files, "keep-file", nil, "Portable file path whose local change should be retained (repeatable)")
	cmd.Flags().StringArrayVar(&credentials, "replace-credential", nil, "Credential environment variable to replace despite the conflict (repeatable)")
	return cmd
}
