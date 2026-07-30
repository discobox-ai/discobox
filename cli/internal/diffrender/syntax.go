package diffrender

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// span is a run of one line that a lexer identified as one token, in rune
// offsets into the line's rendered text.
type span struct {
	start int
	end   int
	token chroma.TokenType
}

// lexerFor picks a lexer from the file's name, or nil when nothing matches.
//
// Nothing matching means no highlighting rather than a fallback lexer: a
// fallback returns the whole text as one plain token, so it costs a tokenise
// pass to produce exactly what not highlighting produces.
func lexerFor(path string) chroma.Lexer {
	lexer := lexers.Match(path)
	if lexer == nil {
		return nil
	}
	// Coalescing merges adjacent tokens of the same type, which turns a line of
	// code into a handful of spans instead of dozens.
	return chroma.Coalesce(lexer)
}

// highlightHunk tokenises a hunk and returns each line's spans.
//
// The lexer sees each side of the hunk as one text — context plus removed lines
// for the old side, context plus added lines for the new one — rather than one
// line at a time. A lexer is a state machine: fed single lines it restarts in
// the initial state on every one, and everything that spans lines (block
// comments, multi-line strings, heredocs) is colored as though it began where
// it did not. Feeding it the hunk keeps that state right within the hunk, which
// is as much context as a diff has. A hunk that itself begins inside a block
// comment still starts in the wrong state; the alternative is fetching whole
// files from the sandbox, which costs a round trip per file and cannot recover
// the old side at all.
func highlightHunk(lexer chroma.Lexer, lines []Line, texts []string) [][]span {
	out := make([][]span, len(lines))
	if lexer == nil {
		return out
	}
	for _, side := range []LineKind{Removed, Added} {
		// Indexes of the lines making up this side of the hunk, in file order.
		var indexes []int
		for i, line := range lines {
			if line.Kind == Context || line.Kind == side {
				indexes = append(indexes, i)
			}
		}
		if len(indexes) == 0 {
			continue
		}
		sideTexts := make([]string, 0, len(indexes))
		for _, i := range indexes {
			sideTexts = append(sideTexts, texts[i])
		}
		spans := tokeniseLines(lexer, sideTexts)
		for offset, i := range indexes {
			// A context line belongs to both sides and is tokenised twice; the
			// two agree, so the second pass simply overwrites the first.
			out[i] = spans[offset]
		}
	}
	return out
}

// tokeniseLines lexes lines as one text and cuts the tokens back up along the
// line boundaries they came from.
func tokeniseLines(lexer chroma.Lexer, lines []string) [][]span {
	out := make([][]span, len(lines))
	iterator, err := lexer.Tokenise(nil, strings.Join(lines, "\n"))
	if err != nil {
		return out
	}
	line, column := 0, 0
	for _, token := range iterator.Tokens() {
		// A token's value can carry newlines — a block comment is one token —
		// so it is split back into the lines it covers.
		for i, part := range strings.Split(token.Value, "\n") {
			if i > 0 {
				line, column = line+1, 0
			}
			if line >= len(out) {
				return out
			}
			width := len([]rune(part))
			if width > 0 && !isPlainToken(token.Type) {
				out[line] = append(out[line], span{start: column, end: column + width, token: token.Type})
			}
			column += width
		}
	}
	return out
}

// isPlainToken reports whether a token should be left in the terminal's own
// foreground color. Ordinary text, whitespace, and punctuation are most of a
// line, and coloring them is what makes highlighting look noisy.
func isPlainToken(token chroma.TokenType) bool {
	switch token {
	case chroma.Text, chroma.TextWhitespace, chroma.None, chroma.Punctuation, chroma.Error:
		return true
	}
	return false
}

// syntaxPalette is the foreground color for each token category.
//
// It is a short, hand-picked list rather than one of chroma's own styles
// because these colors have to stay legible on top of the diff's own
// backgrounds. A style tuned for a plain black terminal will happily paint a
// string green, which then lands on the green band of an added line.
//
// Plain identifiers are deliberately absent, so they render in the terminal's
// own foreground. Coloring every name as well as every keyword, type, literal,
// and comment leaves almost nothing uncolored, and a line where everything is
// emphasized emphasizes nothing.
type syntaxPalette map[chroma.TokenType]color.Color

func newSyntaxPalette(dark bool) syntaxPalette {
	colors := map[chroma.TokenType]string{
		chroma.Keyword:          "141", // purple, and away from both bands
		chroma.NameFunction:     "111", // blue
		chroma.NameClass:        "117",
		chroma.NameBuiltin:      "117",
		chroma.KeywordType:      "117",
		chroma.LiteralString:    "180", // tan, never green
		chroma.LiteralNumber:    "116",
		chroma.Comment:          "245", // gray, deliberately quiet
		chroma.CommentPreproc:   "245",
		chroma.GenericHeading:   "111",
		chroma.GenericStrong:    "141",
		chroma.NameAttribute:    "116",
		chroma.NameTag:          "111",
		chroma.LiteralStringDoc: "180",
	}
	if !dark {
		colors = map[chroma.TokenType]string{
			chroma.Keyword:          "90",
			chroma.NameFunction:     "26",
			chroma.NameClass:        "30",
			chroma.NameBuiltin:      "30",
			chroma.KeywordType:      "30",
			chroma.LiteralString:    "94",
			chroma.LiteralNumber:    "24",
			chroma.Comment:          "244",
			chroma.CommentPreproc:   "244",
			chroma.GenericHeading:   "26",
			chroma.GenericStrong:    "90",
			chroma.NameAttribute:    "24",
			chroma.NameTag:          "26",
			chroma.LiteralStringDoc: "94",
		}
	}
	palette := make(syntaxPalette, len(colors))
	for token, value := range colors {
		palette[token] = lipgloss.Color(value)
	}
	return palette
}

// color resolves a token to its color, walking up chroma's token hierarchy so
// every subtype of a category it does not name explicitly — LiteralStringHeredoc
// under LiteralString, CommentMultiline under Comment — still gets that
// category's color.
func (p syntaxPalette) color(token chroma.TokenType) (color.Color, bool) {
	for _, candidate := range []chroma.TokenType{token, token.SubCategory(), token.Category()} {
		if value, ok := p[candidate]; ok {
			return value, true
		}
	}
	return nil, false
}
