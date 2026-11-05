//go:build prototype

// approval-preview renders design sketches with the real console components.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/discobox-ai/discobox/cli/internal/tui"
)

func main() {
	capture := flag.Bool("capture", false, "Print one ANSI frame instead of opening the interactive preview")
	current := flag.Bool("current", false, "Start with the current credential request")
	width := flag.Int("width", 100, "Capture width")
	height := flag.Int("height", 36, "Capture height")
	flag.Parse()
	if err := tui.RunApprovalPreview(os.Stdout, *capture, *current, *width, *height); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
