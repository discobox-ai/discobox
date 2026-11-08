package tui

import (
	"strconv"
	"testing"

	"github.com/discobox-ai/discobox/assets/brand"
)

// The TUI draws each design-language color role by the xterm index its token
// gives (ADR 26-11-07-363 §4), and the accent in the token's own value. A
// role's value changes in tokens.json, never here, and this fails until the
// constant follows it.
func TestThemeIsTheDesignLanguage(t *testing.T) {
	for token, constant := range map[string]string{
		"line":        colInactive,
		"line-bright": colGreyedOut,
		"text-muted":  colGrey,
		"text-faint":  colDim,
		"gold":        colGold,
		"on-gold":     colBlack,
		"ok":          colOK,
		"green":       colGreen,
		"salmon":      colSalmon,
		"warn":        colWarn,
		"err":         colErr,
		"info":        colInfo,
		"selection":   colHighlightBG,
	} {
		c, ok := brand.LookupColor(token)
		if !ok {
			t.Errorf("no color token %s", token)
			continue
		}
		if want := strconv.Itoa(c.XTerm); constant != want {
			t.Errorf("the TUI draws %s as %s, the token's index is %s", token, constant, want)
		}
	}
	if c, _ := brand.LookupColor("magenta"); colMark != c.Dark {
		t.Errorf("colMark = %s, the magenta token is %s", colMark, c.Dark)
	}
}
