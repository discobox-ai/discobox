// Command discobox-clipwatch reports the Windows clipboard to the discobox
// console running in WSL, one line per change, until its stdin closes. It is
// carried inside the Linux CLI and installed by it; nothing runs it by hand.
// See cli/internal/clipwatch.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/discobox-ai/discobox/cli/internal/clipwatch"
)

func main() {
	if err := clipwatch.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
