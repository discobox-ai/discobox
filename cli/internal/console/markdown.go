package console

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// charRef is a character reference as a markdown renderer reads one:
// `&#27;`, `&#x1b;` or `&rlm;` — and without the semicolon too, which it
// takes as a browser does (`&#x1b[31m` is an escape and `[31m`).
var charRef = regexp.MustCompile(`&(?:#[0-9]+|#[xX][0-9a-fA-F]+|[A-Za-z][A-Za-z0-9]*);?`)

// MarkdownSource is a comment's markdown as it is handed to a renderer, the
// TUI's or the desktop app's. The data source has already escaped every
// character a terminal would act on (Issue), but a renderer decodes character
// references, so `&#x1b;[31m` is printable text there and an escape sequence
// once drawn — and `&#x202e;` a bidirectional override, `&#x200b;` a
// zero-width space. A reference to a character that would not be let through
// as itself is therefore left undecoded, by writing its `&` as `&amp;`: it is
// shown as it was typed, not obeyed. The rest — `&amp;`, `&lt;`, `&nbsp;`,
// `&#128512;` — decode as they do on the page. Line ends are the page's: a
// CRLF is one.
//
// It is done to the whole body, code included, where a renderer decodes
// nothing and such a reference is drawn with the `amp;` in it. Sparing code
// would mean knowing where a fence is as the parser knows it — one inside a
// list item ends with the item, one inside an HTML block is not one — and a
// guess that said "code" where the parser says "prose" is the escape again.
func MarkdownSource(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	shown := func(r rune) bool { return unicode.IsPrint(r) || unicode.Is(unicode.Zs, r) }
	return charRef.ReplaceAllStringFunc(body, func(ref string) string {
		typed := "&amp;" + ref[1:]
		if ref[1] == '#' {
			// The number as the character it names, as well as what html
			// makes of it below: it maps some (0x80–0x9f) as a browser would,
			// and not every reader of the markdown does.
			digits, base := strings.TrimSuffix(ref[2:], ";"), 10
			if digits[0] == 'x' || digits[0] == 'X' {
				digits, base = digits[1:], 16
			}
			if n, err := strconv.ParseUint(digits, base, 32); err != nil || n > unicode.MaxRune || !shown(rune(n)) {
				return typed
			}
		}
		// A name there is not decodes to itself, and is text already.
		for _, r := range html.UnescapeString(ref) {
			if !shown(r) {
				return typed
			}
		}
		return ref
	})
}
