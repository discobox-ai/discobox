// Command discobox-ca-anchor installs a CA into trust-anchor directories under
// the content-derived filename shared with the runc wrapper, so a sandbox's own
// CA and any CA its host injects can coexist in one filesystem. See the runcca
// package's AnchorFileName for why the name is derived rather than fixed.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/obot-platform/discobox/sandbox-agent/runcca"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: discobox-ca-anchor <ca.crt> <anchor-dir> [anchor-dir...]")
		os.Exit(2)
	}
	src := os.Args[1]
	ca, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discobox-ca-anchor: read %s: %v\n", src, err)
		os.Exit(1)
	}
	name := runcca.AnchorFileNameFor(ca)

	for _, dir := range os.Args[2:] {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "discobox-ca-anchor: create %s: %v\n", dir, err)
			os.Exit(1)
		}
		dst := filepath.Join(dir, name)
		// Write in place rather than unlink-and-create: the target may be a
		// bind mount from an outer sandbox, and replacing a mount point fails
		// EBUSY. Writing the identical bytes to our own content-derived name
		// is idempotent, so a re-run is harmless either way.
		//nolint:gosec // A trust anchor is public and must be readable by every user in the container.
		if err := os.WriteFile(dst, ca, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "discobox-ca-anchor: write %s: %v\n", dst, err)
			os.Exit(1)
		}
	}
}
