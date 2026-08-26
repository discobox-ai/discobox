// Package frontmatter reads the `.discobox/` file format: a script with a
// YAML metadata block at the top, delimited by `---`, `#---` or `//---`, and
// an id derived from its filename.
//
// It lives in the root module because it is a convention of the `.discobox/`
// directory rather than a detail of any one reader: service definitions
// (`sandbox-agent/services`) and hook definitions in `discobox-ai/hooks` are
// the same file format, and a second copy of a delimiter table is a second
// answer to what such a file looks like.
//
// What the fields *mean* is the caller's: this package normalizes key spelling
// and value shape, and nothing else. Which keys are required, which values are
// valid, and what a missing one defaults to belong to the domain that reads
// them.
package frontmatter

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Delimiters are the three opening/closing lines a metadata block may use. The
// plain form is YAML's own; the two commented forms let a block sit inside a
// script that still has to run, under the comment syntax of the language it is
// written in.
const (
	DelimiterPlain = "---"
	DelimiterHash  = "#---"
	DelimiterSlash = "//---"
)

// idExtensions are the extensions stripped when deriving an id from a
// filename. It is a list rather than "whatever filepath.Ext returns" because a
// file named `check-1.2` must not become `check-1`: only extensions that are
// plausibly a language or document suffix are removed.
var idExtensions = map[string]struct{}{
	".sh": {}, ".bash": {}, ".zsh": {}, ".fish": {},
	".py": {}, ".rb": {}, ".pl": {},
	".js": {}, ".mjs": {}, ".cjs": {}, ".ts": {},
	".md": {}, ".markdown": {}, ".txt": {},
}

var (
	idNonAlnum    = regexp.MustCompile(`[^a-z0-9]+`)
	idOrderPrefix = regexp.MustCompile(`^[0-9]+-+`)
)

// Parsed is one file split into its metadata block and what follows it.
type Parsed struct {
	// Meta is the block's YAML with any comment prefix removed, ready to
	// decode. It is bytes rather than fields so a caller that wants the raw
	// block — to hash it, to report on it — still can.
	Meta []byte
	// Body is everything after the closing delimiter, unmodified. For a script
	// it is the part that runs and the caller executes the file by path
	// instead; for a prompt-style file it is the prompt.
	Body string
	// HasShebang reports a `#!` first line, which is allowed before the
	// opening delimiter and is what makes a script file with metadata still a
	// script file.
	HasShebang bool
	// Delimiter is which of the three forms the block used.
	Delimiter string
}

// Parse splits a file into its metadata block and body.
//
// Line endings are normalized so a file written on Windows parses the same as
// one written anywhere else; the body is returned with that normalization
// applied, which is why callers that need the exact original bytes should keep
// their own copy.
func Parse(data []byte) (Parsed, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.SplitAfter(text, "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return Parsed{}, errors.New("missing front matter")
	}
	start := 0
	hasShebang := strings.HasPrefix(strings.TrimSuffix(lines[0], "\n"), "#!")
	if hasShebang {
		start = 1
	}
	if start >= len(lines) {
		return Parsed{}, errors.New("missing opening delimiter")
	}
	delim := DelimiterKind(lines[start])
	if delim == "" {
		return Parsed{}, errors.New("missing opening delimiter")
	}

	var meta bytes.Buffer
	closeIndex := -1
	for i := start + 1; i < len(lines); i++ {
		if DelimiterKind(lines[i]) == delim {
			closeIndex = i
			break
		}
		line := strings.TrimSuffix(strings.TrimSuffix(lines[i], "\n"), "\r")
		switch delim {
		case DelimiterHash:
			line = stripCommentPrefix(line, "#")
		case DelimiterSlash:
			line = stripCommentPrefix(line, "//")
		}
		meta.WriteString(line)
		meta.WriteByte('\n')
	}
	if closeIndex == -1 {
		return Parsed{}, errors.New("missing closing delimiter")
	}
	return Parsed{
		Meta:       meta.Bytes(),
		Body:       strings.Join(lines[closeIndex+1:], ""),
		HasShebang: hasShebang,
		Delimiter:  delim,
	}, nil
}

// DelimiterKind reports which delimiter a line is, or empty when it is not
// one. The closing delimiter must match the opening one, so a `#---` block is
// not closed by a bare `---` inside it.
func DelimiterKind(line string) string {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
	switch trimmed {
	case DelimiterPlain, DelimiterHash, DelimiterSlash:
		return trimmed
	default:
		return ""
	}
}

// HasShebangLine reports whether a file opens with `#!`. It reads the original
// bytes rather than a Parsed, because "is this a script" is asked of files that
// failed to parse too.
func HasShebangLine(data []byte) bool { return bytes.HasPrefix(data, []byte("#!")) }

func stripCommentPrefix(line, prefix string) string {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, prefix) {
		// Not a comment line at all: hand it back whole rather than mangling
		// it, and let the YAML decoder report what is wrong with it.
		return line
	}
	trimmed = strings.TrimPrefix(trimmed, prefix)
	return strings.TrimLeft(trimmed, " \t")
}

// Fields is a decoded metadata block, keyed by normalized key.
//
// Normalization is spelling only: keys are lowercased and hyphens become
// underscores, so `run-as`, `Run_As` and `run_as` are one key. Values keep the
// type YAML gave them and are converted on read, which is what lets `blocking:
// yes` and `blocking: true` mean the same thing without the caller caring.
type Fields map[string]any

// Decode reads a metadata block. An empty block is not an error — it decodes to
// an empty Fields — because "declares nothing" is a legitimate state for a file
// whose every field has a default.
func Decode(meta []byte) (Fields, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(meta, &raw); err != nil {
		return nil, err
	}
	fields := make(Fields, len(raw))
	for k, v := range raw {
		fields[NormalizeKey(k)] = v
	}
	return fields, nil
}

// NormalizeKey is the spelling rule: lowercase, hyphens as underscores.
func NormalizeKey(key string) string {
	return strings.ReplaceAll(strings.TrimSpace(strings.ToLower(key)), "-", "_")
}

// String is the first of the given keys that is present, trimmed. The keys are
// aliases for one field, most canonical first, so a caller writes
// `f.String("language_id", "language")` rather than checking each in turn.
func (f Fields) String(keys ...string) string {
	for _, key := range keys {
		if v, ok := f[key]; ok {
			return asString(v)
		}
	}
	return ""
}

// Bool is the first of the given keys that is present, read as a boolean. YAML
// already decodes `true`/`yes`/`on` to a bool; the string cases are for a value
// that arrived quoted.
func (f Fields) Bool(keys ...string) bool {
	for _, key := range keys {
		v, ok := f[key]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case bool:
			return t
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "true", "yes", "y", "on", "1":
				return true
			}
			return false
		case int, int64, uint64:
			return fmt.Sprint(t) != "0"
		}
		return false
	}
	return false
}

// Strings is every given key's value appended in order, each read as a list.
// A scalar counts as a one-element list, so a field that usually holds several
// values can be written with one and no brackets. Empty entries are dropped.
func (f Fields) Strings(keys ...string) []string {
	var out []string
	for _, key := range keys {
		v, ok := f[key]
		if !ok {
			continue
		}
		out = append(out, asStrings(v)...)
	}
	compacted := out[:0]
	for _, s := range out {
		if s = strings.TrimSpace(s); s != "" {
			compacted = append(compacted, s)
		}
	}
	if len(compacted) == 0 {
		return nil
	}
	return compacted
}

// Except is every field whose key is not one of the given ones — the keys this
// file declared that its reader has no meaning for.
//
// Callers keep them rather than rejecting them: a file written for a newer
// reader must still parse under an older one, and an unknown key is far more
// often a forward-compatible declaration than a typo worth failing discovery
// over.
func (f Fields) Except(keys ...string) map[string]any {
	known := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		known[key] = struct{}{}
	}
	var rest map[string]any
	for key, v := range f {
		if _, ok := known[key]; ok {
			continue
		}
		if rest == nil {
			rest = map[string]any{}
		}
		rest[key] = v
	}
	return rest
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case fmt.Stringer:
		return strings.TrimSpace(t.String())
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

func asStrings(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, asString(item))
		}
		return out
	case []string:
		return t
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []string{t}
	default:
		if s := asString(t); s != "" {
			return []string{s}
		}
		return nil
	}
}

// NormalizeID is the stable id a filename produces: lowercased, punctuation
// collapsed to hyphens, a known extension removed, and a numeric ordering
// prefix stripped — `10-discobox-api.sh` is `discobox-api`.
//
// The ordering prefix is stripped because it is a property of the directory
// listing, not of the thing declared: renaming `10-api.sh` to `20-api.sh` to
// move it down the list must not rename the thing itself.
func NormalizeID(filename string) string {
	base := filepath.Base(filename)
	if ext := strings.ToLower(filepath.Ext(base)); ext != "" {
		if _, ok := idExtensions[ext]; ok {
			base = strings.TrimSuffix(base, filepath.Ext(base))
		}
	}
	base = strings.ToLower(base)
	base = strings.Trim(idNonAlnum.ReplaceAllString(base, "-"), "-")
	base = idOrderPrefix.ReplaceAllString(base, "")
	return strings.Trim(base, "-")
}

// DefaultName is the readable name a filename produces, for a file that
// declared none: the id with its words capitalized. Purely numeric words are
// left as they are, since there is no case to raise.
func DefaultName(filename string) string {
	id := NormalizeID(filename)
	if id == "" {
		return ""
	}
	parts := strings.Split(id, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		if _, err := fmt.Sscanf(p, "%d", new(int)); err == nil {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}
