package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/discobox-ai/discobox/pkg/runcca"
	"github.com/discobox-ai/discobox/sandbox-agent/nestedbridge"
)

// runRunc is the runc wrapper, the agent invoked as `runc` from a directory
// prepended to containerd's and dockerd's PATH. It seeds the sandbox's MITM CA
// into a container's trust stores and injects proxy-trust environment into
// its OCI spec, then execs the real runc. See the runcca package for why this
// replaced the NRI plugin of docs/adr/0015.
func runRunc(args []string) int {
	id := runcca.ContainerID(args)

	// Injection is best-effort by design. A container that starts without the
	// MITM CA fails only the TLS calls it happens to make; a container that
	// does not start at all breaks the sandbox outright. Every failure here is
	// reported and stepped over.
	switch {
	case runcca.IsDelete(args):
		if err := runcca.Cleanup(id, runcca.Config{LocalSubnets: nestedbridge.LocalSubnets}); err != nil {
			fmt.Fprintf(os.Stderr, "discobox-runc: staged trust store not reclaimed: %v\n", err)
		}
	default:
		if bundle, ok := runcca.BundleDir(args); ok {
			if _, err := runcca.Adjust(bundle, id, runcca.Config{LocalSubnets: nestedbridge.LocalSubnets}); err != nil {
				fmt.Fprintf(os.Stderr, "discobox-runc: proxy trust not injected: %v\n", err)
			}
		}
	}

	self, err := runcca.SelfInfo()
	if err != nil {
		fmt.Fprintf(os.Stderr, "discobox-runc: %v\n", err)
		return 127
	}
	runc, err := runcca.RealRunc(os.Getenv("PATH"), self)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discobox-runc: %v\n", err)
		return 127
	}
	argv := append([]string{runc}, args...)
	//nolint:gosec // Passing this process's own argv through to the real runc is the entire point of the shim.
	if err := syscall.Exec(runc, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "discobox-runc: exec %s: %v\n", runc, err)
	}
	return 127
}
