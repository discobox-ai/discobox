package termpane

import (
	"regexp"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// WithLinkRewrite decides where a link drawn in the pane actually points.
//
// It is for a pane whose terminal is somewhere else. A server inside a sandbox
// prints the address it is listening on — `http://localhost:8080` — and that
// address is true where it was printed and false where it is read: the port
// reachable here is whatever the forward bound, and `0.0.0.0` is not an address
// a browser can open at all. The text on the screen belongs to the application
// and is left exactly as it printed it; only where a click goes is the host's.
//
// The function is called with a URL and returns the URL to use. Returning the
// one it was given means "this is already right", and is the answer for
// everything the host does not recognize.
//
// It governs two things:
//
//   - Every OSC 8 target an application in the pane emitted passes through it,
//     so a link the application made itself lands where it meant.
//   - Plain text that looks like a URL becomes a link when — and only when —
//     the function moves it. Text nothing moves is left as text, which is the
//     point: the terminal drawing the pane does its own URL detection, and
//     overriding it everywhere to say the same thing would take a working link
//     away to give it back. A link is added exactly where the text lies.
//
// Detection is scheme-led (`http://`, `https://`) and stops at the row, so a
// URL wrapped across the right edge is not one.
func WithLinkRewrite(rewrite func(url string) string) Option {
	return func(o *options) { o.linkRewrite = rewrite }
}

// oscHyperlink opens OSC 8, the sequence terminals have agreed on for a link:
// `OSC 8 ; params ; uri ST`.
const oscHyperlink = "\x1b]8;"

// urlPattern is a URL in the middle of whatever else a line says. Scheme-led,
// because that is the form that is unambiguous in a wall of program output, and
// running to the first character no URL contains. Brackets are in, so an IPv6
// authority survives; angle brackets and quotes are out, so a URL wearing them
// does not eat its own delimiter.
var urlPattern = regexp.MustCompile("https?://[^\\s\"'`<>\\\\^{}|\\x00-\\x20]+")

// rewriteLinks is WithLinkRewrite applied to one rendered row: the targets of
// the links it carries, and a link around each URL in its text the host moves.
//
// It works on the rendered row rather than on the emulator's cells because a
// row is the one place both are whole. Cells are written as they arrive, so a
// URL is a partial URL for as long as it takes the far end to finish printing
// it, and a line that scrolls off mid-write is in the scrollback before anyone
// could look at it. A rendered row has been assembled from the grid and says
// what it is going to say.
func rewriteLinks(row string, rewrite func(string) string) string {
	if rewrite == nil {
		return row
	}
	// The overwhelmingly common row has neither, and pays one scan for it.
	if !strings.Contains(row, oscHyperlink) && !strings.Contains(row, "://") {
		return row
	}

	tokens, text, offsets := scanRow(row)
	changed := false
	for i, token := range tokens {
		uri, params, term, ok := parseHyperlink(token.s)
		if !ok || uri == "" {
			continue
		}
		if to := rewrite(uri); to != uri && to != "" {
			tokens[i].s, changed = oscHyperlink+params+";"+to+term, true
		}
	}

	opens, closes := map[int]string{}, map[int]bool{}
	for _, match := range urlPattern.FindAllStringIndex(text, -1) {
		start, end := match[0], match[0]+len(trimURL(text[match[0]:match[1]]))
		first, last := tokenAt(offsets, start), tokenBefore(offsets, end)
		if first < 0 || last < first || linkedBetween(tokens, first, last) {
			continue
		}
		raw := text[start:end]
		to := rewrite(raw)
		if to == "" || to == raw {
			continue
		}
		opens[first], closes[last], changed = ansi.SetHyperlink(to), true, true
	}
	if !changed {
		return row
	}

	var out strings.Builder
	out.Grow(len(row) + 2*len(opens)*32)
	for i, token := range tokens {
		if open, ok := opens[i]; ok {
			out.WriteString(open)
		}
		out.WriteString(token.s)
		if closes[i] {
			out.WriteString(ansi.ResetHyperlink())
		}
	}
	return out.String()
}

// rowToken is one piece of a rendered row: a printable grapheme, or a sequence
// that is not one. linked marks a grapheme the application has already put
// under a link of its own.
type rowToken struct {
	s      string
	text   bool
	linked bool
}

// textAt is where a printable token sits in the row's plain text, and which
// token it is. The pair is what carries a match in the text back to the pieces
// of the row it was made of.
type textAt struct {
	offset int
	token  int
}

// scanRow splits a row into tokens, the text of it, and the map back.
func scanRow(row string) (tokens []rowToken, text string, offsets []textAt) {
	var plain strings.Builder
	var state byte
	linked := false
	for rest := row; len(rest) > 0; {
		seq, width, n, next := ansi.DecodeSequence(rest, state, nil)
		if n <= 0 {
			// Nothing the decoder can make sense of. It is not text and it is
			// not ours to interpret; it goes through as it came.
			tokens = append(tokens, rowToken{s: rest})
			break
		}
		state, rest = next, rest[n:]
		if uri, _, _, ok := parseHyperlink(seq); ok {
			linked = uri != ""
			tokens = append(tokens, rowToken{s: seq})
			continue
		}
		if width > 0 {
			offsets = append(offsets, textAt{offset: plain.Len(), token: len(tokens)})
			plain.WriteString(seq)
			tokens = append(tokens, rowToken{s: seq, text: true, linked: linked})
			continue
		}
		tokens = append(tokens, rowToken{s: seq})
	}
	return tokens, plain.String(), offsets
}

// parseHyperlink reads an OSC 8 sequence back into its parts.
func parseHyperlink(seq string) (uri, params, term string, ok bool) {
	if !strings.HasPrefix(seq, oscHyperlink) {
		return "", "", "", false
	}
	body := seq[len(oscHyperlink):]
	switch {
	case strings.HasSuffix(body, "\x1b\\"):
		term, body = "\x1b\\", strings.TrimSuffix(body, "\x1b\\")
	case strings.HasSuffix(body, "\x07"):
		term, body = "\x07", strings.TrimSuffix(body, "\x07")
	default:
		return "", "", "", false
	}
	// The params are colon-separated and the URI may hold semicolons of its
	// own, so the first one is the only separator.
	params, uri, found := strings.Cut(body, ";")
	if !found {
		return "", "", "", false
	}
	return uri, params, term, true
}

// tokenAt is the token a text offset starts, and tokenBefore the last token
// that starts before one. Together they bracket a match: the offsets are in
// order, so both are a search.
func tokenAt(offsets []textAt, offset int) int {
	at := sort.Search(len(offsets), func(i int) bool { return offsets[i].offset >= offset })
	if at == len(offsets) || offsets[at].offset != offset {
		return -1
	}
	return offsets[at].token
}

func tokenBefore(offsets []textAt, offset int) int {
	at := sort.Search(len(offsets), func(i int) bool { return offsets[i].offset >= offset })
	if at == 0 {
		return -1
	}
	return offsets[at-1].token
}

// linkedBetween reports whether the application already has a link over any of
// this span. Text it has linked is its own decision about where a click goes;
// the target was rewritten already, and a second link over the top of it would
// be two answers to one question.
func linkedBetween(tokens []rowToken, first, last int) bool {
	for _, token := range tokens[first : last+1] {
		if token.text && token.linked {
			return true
		}
	}
	return false
}

// trimURL drops the punctuation a URL collected from the sentence around it: a
// full stop that ends the line, a bracket that was never opened inside it. A
// closer whose opener is in the match stays — `http://[::1]:8080` is the
// address, brackets and all.
func trimURL(url string) string {
	for len(url) > 0 {
		last := url[len(url)-1]
		switch last {
		case '.', ',', ';', ':', '!', '?', '"', '\'':
		case ')', ']', '}':
			open := map[byte]byte{')': '(', ']': '[', '}': '{'}[last]
			if strings.IndexByte(url, open) >= 0 {
				return url
			}
		default:
			return url
		}
		url = url[:len(url)-1]
	}
	return url
}
