// Command discobox-installer-logo draws the TUI's mark into the install
// scripts, as the escape sequences each language can carry (ADR 0109).
//
//	go generate ./installer
//
// The mark is cell data in cli/internal/tui/logo.json, generated from a
// terminal capture by scripts/logo-cells.mjs and rendered by the TUI through
// lipgloss, which downsamples its colors to whatever the terminal can show. A
// shell script has no lipgloss, so the downsampling happens here instead: each
// variant is written out twice, once in 24-bit color and once in the nearest
// xterm-256 indices, and the script picks by what the terminal claims.
//
// The scripts stay ASCII — Windows PowerShell 5.1 reads a file with no byte
// order mark in the ANSI code page — so the art is escaped on the way in: octal
// for sh's printf %b, base64 for PowerShell. Both decode to the same bytes.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image/color"
	"math"
	"os"
	"strconv"
	"strings"
)

// run is a piece of a row and the colors it is drawn in, as logo.json spells
// them. An absent foreground on a colored run means the glyph belongs in the
// terminal's own background: reverse video is the only way to name that, and
// the notch it carves is the mark's shape.
type run struct {
	Text string `json:"t"`
	FG   string `json:"f"`
	BG   string `json:"b"`
}

type doc struct {
	Width int     `json:"width"`
	Rows  [][]run `json:"rows"`
}

// indent is the margin the mark is drawn inside, so it does not sit flush
// against the edge of a terminal.
const indent = "  "

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-installer-logo:", err)
		os.Exit(1)
	}
}

func generate() error {
	var (
		logo = flag.String("logo", "../cli/internal/tui/logo.json", "the TUI's cell data")
		shln = flag.String("sh", "install.sh", "the sh installer to draw into")
		ps1  = flag.String("ps1", "install.ps1", "the PowerShell installer to draw into")
	)
	flag.Parse()

	raw, err := os.ReadFile(*logo)
	if err != nil {
		return err
	}
	var cells doc
	if err := json.Unmarshal(raw, &cells); err != nil {
		return fmt.Errorf("%s: %w", *logo, err)
	}
	art24, err := render(cells, true)
	if err != nil {
		return err
	}
	art256, err := render(cells, false)
	if err != nil {
		return err
	}

	if err := replace(*shln, map[string]string{
		"logo_24bit=": "logo_24bit='" + octal(art24) + "'",
		"logo_256=":   "logo_256='" + octal(art256) + "'",
	}); err != nil {
		return err
	}
	return replace(*ps1, map[string]string{
		"$logo24bit = ": "$logo24bit = '" + base64.StdEncoding.EncodeToString([]byte(art24)) + "'",
		"$logo256 = ":   "$logo256 = '" + base64.StdEncoding.EncodeToString([]byte(art256)) + "'",
	})
}

// render draws every row, dropping the blank rows the TUI's layout pads with:
// an installer gives the mark its own spacing.
func render(cells doc, trueColor bool) (string, error) {
	var rows []string
	for _, row := range cells.Rows {
		drawn, err := renderRow(row, trueColor)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(stripped(row)) == "" && len(rows) == 0 {
			continue
		}
		rows = append(rows, indent+drawn)
	}
	for len(rows) > 0 && strings.TrimSpace(rows[len(rows)-1]) == "" {
		rows = rows[:len(rows)-1]
	}
	return strings.Join(rows, "\n") + "\n", nil
}

func stripped(row []run) string {
	var b strings.Builder
	for _, r := range row {
		b.WriteString(r.Text)
	}
	return b.String()
}

func renderRow(row []run, trueColor bool) (string, error) {
	var b strings.Builder
	for _, r := range row {
		if r.FG == "" && r.BG == "" {
			b.WriteString(r.Text)
			continue
		}
		var codes []string
		switch {
		case r.FG == "" && r.BG != "":
			// The glyph is drawn in the terminal's ground: put the color on as
			// a foreground and let reverse video swap it behind the glyph.
			codes = append(codes, "7")
			code, err := sgr(r.BG, true, trueColor)
			if err != nil {
				return "", err
			}
			codes = append(codes, code)
		default:
			if r.FG != "" {
				code, err := sgr(r.FG, true, trueColor)
				if err != nil {
					return "", err
				}
				codes = append(codes, code)
			}
			if r.BG != "" {
				code, err := sgr(r.BG, false, trueColor)
				if err != nil {
					return "", err
				}
				codes = append(codes, code)
			}
		}
		fmt.Fprintf(&b, "\x1b[%sm%s\x1b[0m", strings.Join(codes, ";"), r.Text)
	}
	return b.String(), nil
}

// sgr is one color as a foreground or background, in 24-bit or as the nearest
// xterm-256 index.
func sgr(hex string, foreground, trueColor bool) (string, error) {
	rgb, err := parseHex(hex)
	if err != nil {
		return "", err
	}
	lead := "48"
	if foreground {
		lead = "38"
	}
	if trueColor {
		return fmt.Sprintf("%s;2;%d;%d;%d", lead, rgb.R, rgb.G, rgb.B), nil
	}
	return fmt.Sprintf("%s;5;%d", lead, nearest256(rgb)), nil
}

func parseHex(hex string) (color.RGBA, error) {
	if len(hex) != 7 || hex[0] != '#' {
		return color.RGBA{}, fmt.Errorf("%q is not a #rrggbb color", hex)
	}
	value, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return color.RGBA{}, fmt.Errorf("%q is not a #rrggbb color: %w", hex, err)
	}
	return color.RGBA{R: uint8(value >> 16), G: uint8(value >> 8), B: uint8(value), A: 0xff}, nil
}

// nearest256 is the xterm-256 index closest to a color, over the 6x6x6 cube and
// the greyscale ramp. The first sixteen are left out: they are exactly the
// indices every terminal theme redefines, which is what logo.json exists to
// stop the mark depending on.
func nearest256(want color.RGBA) int {
	best, bestDistance := 16, math.MaxFloat64
	for index := 16; index < 256; index++ {
		if distance := squares(want, xterm(index)); distance < bestDistance {
			best, bestDistance = index, distance
		}
	}
	return best
}

func squares(a, b color.RGBA) float64 {
	dr, dg, db := float64(a.R)-float64(b.R), float64(a.G)-float64(b.G), float64(a.B)-float64(b.B)
	return dr*dr + dg*dg + db*db
}

// xterm is the color an index stands for, by the standard 6x6x6 cube and
// 24-step grey ramp.
func xterm(index int) color.RGBA {
	if index >= 232 {
		level := uint8(8 + 10*(index-232))
		return color.RGBA{R: level, G: level, B: level, A: 0xff}
	}
	steps := []uint8{0, 95, 135, 175, 215, 255}
	index -= 16
	return color.RGBA{R: steps[index/36], G: steps[(index/6)%6], B: steps[index%6], A: 0xff}
}

// octal escapes the art for sh's printf %b, which reads \0ooo and \033 and
// nothing else it needs here. Every byte above ASCII is escaped, so the script
// itself stays ASCII whatever the art contains.
func octal(art string) string {
	var b strings.Builder
	for _, byte := range []byte(art) {
		switch {
		case byte == 0x1b:
			b.WriteString(`\033`)
		case byte == '\n':
			b.WriteString(`\n`)
		case byte == '\'':
			// Cannot occur in the art, and would end the shell string if it did.
			b.WriteString(`\047`)
		case byte < 0x20 || byte >= 0x7f:
			fmt.Fprintf(&b, `\0%03o`, byte)
		default:
			b.WriteByte(byte)
		}
	}
	return b.String()
}

// replace rewrites each marked assignment in a script. A marker is the
// assignment up to its value, so a line already carrying art is replaced as
// readily as an empty one — this has to be safe to run twice, since
// `task verify` runs it over a tree it has just run it over.
//
// Every marker has to match exactly one line: a script edited so one moved or
// doubled would otherwise ship with a stale mark, or two of them.
func replace(path string, stamps map[string]string) error {
	source, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(source), "\n")
	seen := map[string]int{}
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		for marker, replacement := range stamps {
			if trimmed == marker || strings.HasPrefix(trimmed, marker+"'") {
				indent := line[:len(line)-len(trimmed)]
				lines[i] = indent + replacement
				seen[marker]++
			}
		}
	}
	for marker := range stamps {
		if seen[marker] != 1 {
			return fmt.Errorf("%s: expected one %q line, found %d", path, marker, seen[marker])
		}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644) //nolint:gosec // G306: an install script, which is public by the time anyone runs it.
}
