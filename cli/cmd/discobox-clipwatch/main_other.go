//go:build !windows

package main

import (
	"fmt"
	"os"
)

// The helper watches the Windows clipboard, and is built for Windows alone.
func main() {
	fmt.Fprintln(os.Stderr, "discobox-clipwatch runs on Windows only")
	os.Exit(2)
}
