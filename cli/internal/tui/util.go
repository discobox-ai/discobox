package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// fitCells truncates or pads s to exactly width display cells, ANSI-aware, so a
// rendered line can never wrap. Any style left open by the content is reset
// before the padding, and before the caller appends anything after it, so color
// cannot bleed across the cell boundary.
func fitCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = ansi.Truncate(s, width, "") + "\x1b[m"
	if w := ansi.StringWidth(s); w < width {
		s += strings.Repeat(" ", width-w)
	}
	return s
}

// fitVertical pads or truncates s to exactly height lines so the caller can pin
// content that follows (like a footer) to a fixed row.
func fitVertical(s string, height int) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}
