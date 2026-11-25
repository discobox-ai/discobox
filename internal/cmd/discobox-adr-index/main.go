// Command discobox-adr-index prints the index of docs/adr, read from the ADR
// files themselves, and checks that each one can be read.
//
// The index is never a committed file. When it was a table in
// docs/adr/README.md, every ADR pull request added a row to it, so each one
// that merged left every other open ADR pull request conflicting. Git's
// `merge=union` attribute would not help there: GitHub's mergeability check
// and its merge queue apply no merge drivers. A committed file generated from
// the ADRs would conflict the same way. So each ADR's own heading and Status
// line are the record, and this prints the index from them on demand.
//
//	discobox-adr-index                   the whole index, as a Markdown table
//	discobox-adr-index -status proposed  only ADRs whose status starts so
//	discobox-adr-index -check            fail when an ADR has no readable
//	                                     heading or Status line
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// adr is one ADR file as the index shows it.
type adr struct {
	ID     string
	File   string
	Title  string
	Status string
}

var (
	// idPattern is an ADR's ID at the start of its file name: the four-digit
	// numbers that predate the date scheme, or YY-MM-DD-RRR.
	idPattern = regexp.MustCompile(`^([0-9]{4}|[0-9]{2}-[0-9]{2}-[0-9]{2}-[0-9]{3})-`)
	// headingPrefix is the ID before an ADR's title: `0051. ` in some early
	// ADRs, `26-09-24-458 — ` in the rest.
	headingPrefix = regexp.MustCompile(`^([0-9]{4}|[0-9]{2}-[0-9]{2}-[0-9]{2}-[0-9]{3})(\.| —) `)
	// statusLine is the line that opens an ADR's status, in each of the
	// spellings ADRs have used: `- **Status**: `, `Status: ` and `**Status:** `.
	statusLine = regexp.MustCompile(`^(?:- )?(?:\*\*Status\*\*:|\*\*Status:\*\*|Status:)\s*(.*)$`)
	// datedID is the date scheme, whose heading must be `# <ID> — <Title>`.
	datedID = regexp.MustCompile(`^[0-9]{2}-[0-9]{2}-[0-9]{2}-[0-9]{3}$`)
)

func main() {
	dir := flag.String("dir", "docs/adr", "the ADR directory")
	status := flag.String("status", "", "list only ADRs whose status starts with this word (case-insensitive), e.g. proposed")
	check := flag.Bool("check", false, "fail when an ADR has no readable heading or Status line, instead of printing the index")
	flag.Parse()

	adrs, problems, err := load(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox-adr-index:", err)
		os.Exit(1)
	}
	if *check {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, p)
		}
		if len(problems) > 0 {
			fmt.Fprintf(os.Stderr, "discobox-adr-index: %d ADR(s) the index cannot read; see docs/adr/README.md, Identifiers\n", len(problems))
			os.Exit(1)
		}
		return
	}
	if err := write(os.Stdout, filter(adrs, *status)); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-adr-index:", err)
		os.Exit(1)
	}
}

// load reads every ADR in dir, in ID order: the four-digit numbers first, then
// the dated IDs in date order. A file it cannot fully read is still listed,
// with what it could read, and named in problems.
func load(dir string) ([]adr, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var adrs []adr
	var problems []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".md" || name == "README.md" {
			continue
		}
		path := filepath.Join(dir, name)
		m := idPattern.FindStringSubmatch(name)
		if m == nil {
			problems = append(problems, fmt.Sprintf("%s: the file name does not start with an ADR ID (YY-MM-DD-RRR-<slug>.md)", path))
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		a, faults := parse(m[1], name, string(body))
		for _, f := range faults {
			problems = append(problems, path+": "+f)
		}
		adrs = append(adrs, a)
	}
	sort.Slice(adrs, func(i, j int) bool { return adrs[i].ID < adrs[j].ID })
	return adrs, problems, nil
}

// parse reads an ADR's title from its first heading and its status from its
// Status line and the indented lines that continue it.
func parse(id, file, body string) (adr, []string) {
	a := adr{ID: id, File: file}
	var faults []string
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	heading := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "# ") {
			heading = i
			break
		}
	}
	if heading < 0 {
		faults = append(faults, "no `# <ID> — <Title>` heading")
	} else {
		h := strings.TrimSpace(strings.TrimPrefix(lines[heading], "# "))
		if datedID.MatchString(id) && !strings.HasPrefix(h, id+" — ") {
			faults = append(faults, fmt.Sprintf("the heading is not `# %s — <Title>`", id))
		}
		a.Title = headingPrefix.ReplaceAllString(h, "")
	}
	for i := heading + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			break
		}
		m := statusLine.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		parts := []string{strings.TrimSpace(m[1])}
		for _, l := range lines[i+1:] {
			if l == "" || (l[0] != ' ' && l[0] != '\t') {
				break
			}
			parts = append(parts, strings.TrimSpace(l))
		}
		a.Status = strings.Join(parts, " ")
		break
	}
	if a.Status == "" {
		faults = append(faults, "no `- **Status**: …` line before its first section")
	}
	return a, faults
}

// filter keeps the ADRs whose status starts with the word given, ignoring
// case; an empty word keeps every one.
func filter(adrs []adr, word string) []adr {
	if word == "" {
		return adrs
	}
	var kept []adr
	for _, a := range adrs {
		if strings.HasPrefix(strings.ToLower(a.Status), strings.ToLower(word)) {
			kept = append(kept, a)
		}
	}
	return kept
}

// write prints the index as a Markdown table whose links resolve from
// docs/adr.
func write(w io.Writer, adrs []adr) error {
	var b strings.Builder
	b.WriteString("| ADR | Title | Status |\n| --- | --- | --- |\n")
	for _, a := range adrs {
		fmt.Fprintf(&b, "| [%s](%s) | %s | %s |\n", a.ID, a.File, cell(a.Title), cell(a.Status))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// cell escapes the one character that would end a table cell.
func cell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}
