package termpane

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

// WithClaimedLinks names the link schemes the host answers itself, rather
// than the terminal drawing the pane.
//
// It is for a link only the host can open. A pane in a multiplexer shows
// something like `discobox:///sbx_0123`, which names another of the host's
// own panes. Nothing outside the host knows the scheme, so the host has to
// answer the click. A link left as OSC 8 would be worse than no link: the
// terminal's own Ctrl-click would hand it to an OS that has nowhere to send
// it, and the host would never hear of the press.
//
// So a link whose scheme is claimed:
//
//   - is drawn without its OSC 8, whether the application emitted one or the
//     link is plain text, which nothing outside would detect anyway; its text
//     is left exactly as printed;
//   - is found in plain text too, scheme-led and stopping at the row, the same
//     way WithLinkRewrite finds a URL;
//   - is reported by Links, with where the last View drew it, so the host can
//     mark it, light it and answer a press on it.
//
// Schemes are matched without regard to case. What a claimed URI means, and
// whether it is any good, is the host's to decide when it is pressed.
func WithClaimedLinks(schemes ...string) Option {
	return func(o *options) {
		o.claimed = nil
		quoted := make([]string, 0, len(schemes))
		for _, scheme := range schemes {
			if scheme = strings.ToLower(strings.TrimSpace(scheme)); scheme != "" {
				o.claimed = append(o.claimed, scheme)
				quoted = append(quoted, regexp.QuoteMeta(scheme))
			}
		}
		o.claimPattern = nil
		if len(quoted) > 0 {
			// The same run of URL characters as urlPattern, after one of
			// the claimed schemes; where a scheme starts is startsScheme's.
			o.claimPattern = regexp.MustCompile("(?i)(?:" + strings.Join(quoted, "|") + ")://" + urlChars)
		}
	}
}

// Link is one claimed link as the last View drew it: Row is the row of the
// view, Col its first cell and Width the cells it covers, all inside the
// pane's own grid. URI is the target, as emitted or as printed.
type Link struct {
	Row, Col, Width int
	URI             string
}

// Links are the claimed links the last View drew, in row order and left to
// right within a row (see WithClaimedLinks). The slice is the pane's own and
// is reused by the next View; copy it to keep it.
func (m *Model) Links() []Link { return m.links }

// claims reports whether a link target is one of the host's own schemes.
func (o *options) claims(uri string) bool {
	scheme, _, ok := strings.Cut(uri, "://")
	if !ok {
		return false
	}
	scheme = strings.ToLower(scheme)
	return slices.Contains(o.claimed, scheme)
}

// claimLinks takes the claimed links out of one rendered row of the view: the
// OSC 8 around one the application emitted is dropped, plain text naming one
// is found, and each is recorded in m.links with the cells it covers. It runs
// on the rendered row for the reason rewriteLinks does.
func (m *Model) claimLinks(viewRow int, row string) string {
	if m.opts.claimPattern == nil || !strings.Contains(row, "://") {
		return row
	}
	tokens, text, offsets := scanRow(row)
	cols := make([]int, len(tokens)+1)
	for i, token := range tokens {
		cols[i+1] = cols[i] + token.width
	}
	add := func(from, to int, uri string) {
		// Only what is on the grid: a scrollback row can be wider than the
		// pane, and the fit cuts what is past its edge.
		to = min(to, m.cols)
		if to > from {
			m.links = append(m.links, Link{Row: viewRow, Col: from, Width: to - from, URI: uri})
		}
	}

	changed := false
	var found []Link
	claim, from := "", 0
	for i, token := range tokens {
		uri, _, _, ok := parseHyperlink(token.s)
		if !ok {
			continue
		}
		if claim != "" {
			found = append(found, Link{Col: cols[from], Width: cols[i] - cols[from], URI: claim})
			if uri == "" {
				// The close of the link taken out: the open is gone, so it
				// is too.
				tokens[i].s, changed = "", true
			}
			claim = ""
		}
		if uri != "" && m.opts.claims(uri) {
			tokens[i].s, changed = "", true
			claim, from = uri, i
		}
	}
	if claim != "" {
		found = append(found, Link{Col: cols[from], Width: cols[len(tokens)] - cols[from], URI: claim})
	}

	for _, match := range m.opts.claimPattern.FindAllStringIndex(text, -1) {
		if !startsScheme(text, match[0]) {
			continue
		}
		start, end := match[0], match[0]+len(trimURL(text[match[0]:match[1]]))
		first, last := tokenAt(offsets, start), tokenBefore(offsets, end)
		// Text under a link of the application's own is that link's, claimed
		// or not.
		if first < 0 || last < first || linkedBetween(tokens, first, last) {
			continue
		}
		found = append(found, Link{Col: cols[first], Width: cols[last+1] - cols[first], URI: text[start:end]})
	}

	// In order along the row, whichever way each was found.
	slices.SortFunc(found, func(a, b Link) int { return cmp.Compare(a.Col, b.Col) })
	for _, link := range found {
		add(link.Col, link.Col+link.Width, link.URI)
	}

	if !changed {
		return row
	}
	var out strings.Builder
	out.Grow(len(row))
	for _, token := range tokens {
		out.WriteString(token.s)
	}
	return out.String()
}
