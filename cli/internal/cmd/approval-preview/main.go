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
	request := flag.String("request", "", "discobox-access JSON request fixture")
	root := flag.String("policy-root", ".", "Directory relative policy file names are resolved against")
	screen := flag.String("screen", "credential", "Screen to explore: credential, adoption, trust or refresh")
	flag.Parse()
	if err := tui.RunApprovalPreview(os.Stdout, *capture, *current, *width, *height, *request, *root, *screen); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
