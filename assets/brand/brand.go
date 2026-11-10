// Package brand is the design language's tokens (LANGUAGE.md) as Go reads
// them: tokens.json, embedded here so that every Go consumer reads the one
// file rather than a copy of it. The console core hands its colors and its
// shape (space, radii, strokes) to the desktop app (ADR 26-11-07-363 §4), and
// the TUI is held to the colors' xterm indices.
package brand

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

//go:embed tokens.json
var tokensJSON []byte

// Color is one color role: its value in each theme, and the xterm-256 index
// the TUI draws it with.
type Color struct {
	// Name is the role's token name: "gold", "text-faint".
	Name string
	// Dark is the dark theme's value, the canonical one, as #rrggbb.
	Dark string
	// Light is the light theme's value, as #rrggbb.
	Light string
	// XTerm is the TUI's index for the role, 0 where the TUI does not use the
	// role by index. No role uses an index below 16, which are whatever a
	// terminal's theme redefines them to (cli/internal/tui/theme.go).
	XTerm int
}

var colors = mustColors(tokensJSON)

// Dimension is one space, radius or stroke token: a length the desktop draws
// in points, which the web draws in pixels.
type Dimension struct {
	// Name is the token's name: "radius-frame", "stroke-focus".
	Name string
	// Points is its length.
	Points float64
}

// shapeGroups are the token groups that are lengths in px. The type scale is
// not one of them: its steps are rem on a page.
var shapeGroups = []string{"space", "radius", "stroke"}

var dimensions = mustDimensions(tokensJSON)

// Dimensions is every space, radius and stroke token, ordered by name.
func Dimensions() []Dimension { return slices.Clone(dimensions) }

func mustDimensions(data []byte) []Dimension {
	out, err := parseDimensions(data)
	if err != nil {
		panic("assets/brand/tokens.json: " + err.Error())
	}
	return out
}

func parseDimensions(data []byte) ([]Dimension, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var out []Dimension
	for _, group := range shapeGroups {
		var tokens map[string]struct {
			Type  string `json:"$type"`
			Value string `json:"$value"`
		}
		if err := json.Unmarshal(doc[group], &tokens); err != nil {
			return nil, fmt.Errorf("%s: %w", group, err)
		}
		if len(tokens) == 0 {
			return nil, fmt.Errorf("no %s tokens", group)
		}
		for name, t := range tokens {
			px, ok := strings.CutSuffix(t.Value, "px")
			v, err := strconv.ParseFloat(px, 64)
			if t.Type != "dimension" || !ok || err != nil || v < 0 {
				return nil, fmt.Errorf("%s %q needs a dimension in px, not %q", group, name, t.Value)
			}
			out = append(out, Dimension{Name: name, Points: v})
		}
	}
	slices.SortFunc(out, func(a, b Dimension) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Colors is every color role, ordered by name.
func Colors() []Color { return slices.Clone(colors) }

// LookupColor is one color role by name.
func LookupColor(name string) (Color, bool) {
	i := slices.IndexFunc(colors, func(c Color) bool { return c.Name == name })
	if i < 0 {
		return Color{}, false
	}
	return colors[i], true
}

func mustColors(data []byte) []Color {
	out, err := parseColors(data)
	if err != nil {
		panic("assets/brand/tokens.json: " + err.Error())
	}
	return out
}

func parseColors(data []byte) ([]Color, error) {
	var doc struct {
		Color map[string]struct {
			Type       string `json:"$type"`
			Value      string `json:"$value"`
			Extensions struct {
				Discobox struct {
					Light string `json:"light"`
					XTerm int    `json:"xterm"`
				} `json:"ai.discobox"`
			} `json:"$extensions"`
		} `json:"color"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := make([]Color, 0, len(doc.Color))
	for name, t := range doc.Color {
		ext := t.Extensions.Discobox
		if t.Type != "color" || !isHex(t.Value) || !isHex(ext.Light) {
			return nil, fmt.Errorf("color %q needs a #rrggbb value and light value", name)
		}
		if ext.XTerm != 0 && (ext.XTerm < 16 || ext.XTerm > 255) {
			return nil, fmt.Errorf("color %q: xterm %d is not an index from 16 to 255", name, ext.XTerm)
		}
		out = append(out, Color{Name: name, Dark: strings.ToLower(t.Value), Light: strings.ToLower(ext.Light), XTerm: ext.XTerm})
	}
	slices.SortFunc(out, func(a, b Color) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func isHex(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for _, c := range s[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}
