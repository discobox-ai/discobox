// Command discobox-ca-anchor establishes trust for a CA inside a sandbox: it
// installs the certificate into each distro's trust-anchor directory under the
// content-derived filename shared with the runc wrapper, and then materializes
// the trust store those anchors belong in.
//
// It replaces update-ca-certificates on the boot path. That tool rebuilds the
// whole store — every certificate concatenated into the bundle, the entire
// directory rehashed — at the same cost whether it is adding one CA or all of
// them, and it is ordered ahead of the sandbox agent, so a sandbox waited ~1.7s
// for an attachable terminal while 152 system certificates were re-hashed to
// add one. See the runcca package's MaterializeTrustStore.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/obot-platform/discobox/runcca"
)

func main() {
	store := flag.String("store", "", "trust store to materialize (e.g. /etc/ssl/certs); empty installs anchors only")
	prebuilt := flag.String("prebuilt", runcca.PrebuiltTrustStoreDir, "image-built system trust store to seed from")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: discobox-ca-anchor [-store DIR] [-prebuilt DIR] <ca.crt> <anchor-dir> [anchor-dir...]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 2 {
		flag.Usage()
		os.Exit(2)
	}

	src := flag.Arg(0)
	anchorDirs := flag.Args()[1:]
	ca, err := os.ReadFile(src)
	if err != nil {
		fail("read %s: %v", src, err)
	}
	name := runcca.AnchorFileNameFor(ca)

	for _, dir := range anchorDirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail("create %s: %v", dir, err)
		}
		dst := filepath.Join(dir, name)
		// Write in place rather than unlink-and-create: the target may be a
		// bind mount from an outer sandbox, and replacing a mount point fails
		// EBUSY. Writing the identical bytes to our own content-derived name
		// is idempotent, so a re-run is harmless either way.
		//nolint:gosec // A trust anchor is public and must be readable by every user in the container.
		if err := os.WriteFile(dst, ca, 0o644); err != nil {
			fail("write %s: %v", dst, err)
		}
	}

	if *store == "" {
		return
	}
	// Every anchor directory is scanned, not just the one this CA landed in: a
	// nested sandbox sees its host's CA arrive as a bind mount it never wrote,
	// and dropping it from the bundle would cut off the egress path the outer
	// proxy owns.
	if err := runcca.MaterializeTrustStore(*store, *prebuilt, anchorDirs); err != nil {
		fail("materialize trust store %s: %v", *store, err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "discobox-ca-anchor: "+format+"\n", args...)
	os.Exit(1)
}
