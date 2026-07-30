// Package diffrender parses a unified diff and renders it for a terminal.
//
// It exists because a raw unified diff is a wire format, not a reading format:
// the information a reader wants — which file, which line number, what changed
// within the line — is present but has to be reconstructed from `@@` headers
// and leading sigils. Parsing restores it, and rendering shows it.
package diffrender

import (
	"strconv"
	"strings"
)

// LineKind is what a diff line is: unchanged, added, or removed.
type LineKind int

const (
	Context LineKind = iota
	Added
	Removed
)

// Line is one line of a hunk, with the line numbers it has on each side. A
// number is 0 where that side does not have the line.
type Line struct {
	Kind LineKind
	Text string
	Old  int
	New  int
	// NoNewline marks a line the diff reported as lacking a trailing newline.
	NoNewline bool
}

// Hunk is one contiguous run of changes plus its surrounding context.
type Hunk struct {
	OldStart int
	NewStart int
	Lines    []Line
}

// Status is what happened to a file as a whole.
type Status int

const (
	Modified Status = iota
	AddedFile
	DeletedFile
	Renamed
)

// File is one file's changes.
type File struct {
	Path    string
	OldPath string
	Status  Status
	Binary  bool
	// Mode is a mode change ("100644 -> 100755"), empty when the mode is
	// unchanged.
	Mode    string
	Hunks   []Hunk
	Added   int
	Removed int
}

// Parse reads a unified diff, as `git diff` writes it, into per-file changes.
//
// It is deliberately tolerant: anything it does not recognize is skipped rather
// than rejected, because the alternative is a rendering command that fails on
// output a human could have read perfectly well.
func Parse(patch string) []File {
	var files []File
	var current *File
	var hunk *Hunk
	oldNo, newNo := 0, 0

	flushHunk := func() {
		if current != nil && hunk != nil {
			current.Hunks = append(current.Hunks, *hunk)
		}
		hunk = nil
	}
	flushFile := func() {
		flushHunk()
		if current != nil {
			files = append(files, *current)
		}
		current = nil
	}

	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flushFile()
			current = &File{}
			if before, after, ok := parseDiffGitPaths(line); ok {
				current.Path, current.OldPath = after, before
			}
		case current == nil:
			// Anything before the first file header is not part of a patch.
			continue
		case strings.HasPrefix(line, "@@"):
			flushHunk()
			oldStart, newStart, ok := parseHunkHeader(line)
			if !ok {
				continue
			}
			hunk = &Hunk{OldStart: oldStart, NewStart: newStart}
			oldNo, newNo = oldStart, newStart
		case hunk == nil:
			// Still in the file's header: the metadata lines that precede the
			// first hunk, and that say what happened to the file itself.
			switch {
			case strings.HasPrefix(line, "new file mode"):
				current.Status = AddedFile
			case strings.HasPrefix(line, "deleted file mode"):
				current.Status = DeletedFile
			case strings.HasPrefix(line, "rename from "):
				current.Status = Renamed
				current.OldPath = unquotePath(strings.TrimPrefix(line, "rename from "))
			case strings.HasPrefix(line, "rename to "):
				current.Status = Renamed
				current.Path = unquotePath(strings.TrimPrefix(line, "rename to "))
			case strings.HasPrefix(line, "old mode "):
				current.Mode = strings.TrimSpace(strings.TrimPrefix(line, "old mode "))
			case strings.HasPrefix(line, "new mode "):
				current.Mode = strings.TrimSpace(current.Mode + " -> " + strings.TrimSpace(strings.TrimPrefix(line, "new mode ")))
			case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
				current.Binary = true
			case strings.HasPrefix(line, "--- "):
				if path, ok := headerPath(strings.TrimPrefix(line, "--- ")); ok {
					current.OldPath = path
				}
			case strings.HasPrefix(line, "+++ "):
				if path, ok := headerPath(strings.TrimPrefix(line, "+++ ")); ok {
					current.Path = path
				}
			}
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" belongs to the line before it.
			if n := len(hunk.Lines); n > 0 {
				hunk.Lines[n-1].NoNewline = true
			}
		case strings.HasPrefix(line, "+"):
			hunk.Lines = append(hunk.Lines, Line{Kind: Added, Text: line[1:], New: newNo})
			current.Added++
			newNo++
		case strings.HasPrefix(line, "-"):
			hunk.Lines = append(hunk.Lines, Line{Kind: Removed, Text: line[1:], Old: oldNo})
			current.Removed++
			oldNo++
		case strings.HasPrefix(line, " ") || line == "":
			// An all-whitespace context line is written with its single leading
			// space, so an empty line here is a context line whose content is
			// empty — or trailing slack after the final newline, which adds an
			// empty context line nobody sees at the end of the last hunk.
			hunk.Lines = append(hunk.Lines, Line{Kind: Context, Text: strings.TrimPrefix(line, " "), Old: oldNo, New: newNo})
			oldNo++
			newNo++
		default:
			// Not a diff line: the next file's header, or trailer noise.
			flushHunk()
		}
	}
	flushFile()
	return trimTrailingBlank(files)
}

// trimTrailingBlank drops the empty context line a patch's own trailing newline
// produces, which is an artifact of splitting rather than a line of the file.
func trimTrailingBlank(files []File) []File {
	if len(files) == 0 {
		return files
	}
	file := &files[len(files)-1]
	if len(file.Hunks) == 0 {
		return files
	}
	hunk := &file.Hunks[len(file.Hunks)-1]
	if n := len(hunk.Lines); n > 0 && hunk.Lines[n-1].Kind == Context && hunk.Lines[n-1].Text == "" {
		hunk.Lines = hunk.Lines[:n-1]
	}
	return files
}

// parseHunkHeader reads the starting line numbers out of "@@ -a,b +c,d @@".
func parseHunkHeader(line string) (oldStart, newStart int, ok bool) {
	rest, _, found := strings.Cut(strings.TrimPrefix(line, "@@ "), " @@")
	if !found {
		return 0, 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return 0, 0, false
	}
	oldStart, ok = parseRangeStart(fields[0], "-")
	if !ok {
		return 0, 0, false
	}
	newStart, ok = parseRangeStart(fields[1], "+")
	if !ok {
		return 0, 0, false
	}
	return oldStart, newStart, true
}

func parseRangeStart(field, sign string) (int, bool) {
	field = strings.TrimPrefix(field, sign)
	if start, _, ok := strings.Cut(field, ","); ok {
		field = start
	}
	value, err := strconv.Atoi(field)
	if err != nil {
		return 0, false
	}
	return value, true
}

// headerPath reads a path from a "---"/"+++" line, reporting false for
// /dev/null, which names the absence of a file rather than one.
func headerPath(value string) (string, bool) {
	// git appends a tab and a timestamp to these lines for some formats.
	value, _, _ = strings.Cut(value, "\t")
	value = unquotePath(strings.TrimSpace(value))
	if value == "/dev/null" {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimPrefix(value, "a/"), "b/"), true
}

// parseDiffGitPaths reads both paths from a "diff --git a/x b/x" line.
//
// That line is ambiguous for paths containing spaces, so it is only a starting
// point: the "---"/"+++" lines that follow are unambiguous and overwrite it.
func parseDiffGitPaths(line string) (before, after string, ok bool) {
	rest := strings.TrimPrefix(line, "diff --git ")
	if strings.HasPrefix(rest, `"`) {
		// Both paths are quoted, so they split on the space between them.
		if end := strings.Index(rest[1:], `" "`); end >= 0 {
			return unquotePath(rest[:end+2]), unquotePath(rest[end+3:]), true
		}
		return "", "", false
	}
	// Unquoted: assume the common case of one path repeated, which splits at the
	// midpoint, and fall back to the first space otherwise.
	if half := len(rest) / 2; half > 0 && rest[half] == ' ' && rest[:half] != "" {
		before, after = rest[:half], rest[half+1:]
	} else {
		var found bool
		before, after, found = strings.Cut(rest, " ")
		if !found {
			return "", "", false
		}
	}
	return strings.TrimPrefix(before, "a/"), strings.TrimPrefix(after, "b/"), true
}

// unquotePath undoes git's C-style quoting of paths that need escaping.
func unquotePath(value string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, `"`) || !strings.HasSuffix(value, `"`) || len(value) < 2 {
		return value
	}
	unquoted, err := strconv.Unquote(value)
	if err != nil {
		return value
	}
	return unquoted
}
