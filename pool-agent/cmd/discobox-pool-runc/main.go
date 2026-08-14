// Command discobox-pool-runc installs as `runc` on the directory prepended to
// buildkitd's PATH, and is what buildkitd is pointed at by --oci-worker-binary.
// It seeds the pool's MITM CA into every build step's trust stores, then execs
// the real runc.
//
// It is the pool-side half of ADR 0020's wrapper, sharing the root `runcca`
// package with the sandbox rather than forking it. Two things differ, and both
// are parameters rather than code:
//
//   - the real runc is BuildKit's own `buildkit-runc`, not `/usr/bin/runc`
//   - there is no sandbox manifest and no nested Docker bridge out here, so no
//     proxy environment is read from disk; a build's proxy variables arrive as
//     build-args the mediator injected, and are already in the spec
package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/obot-platform/discobox/pool-agent/buildkitagent"
	"github.com/obot-platform/discobox/runcca"
)

func config() runcca.Config {
	return runcca.Config{
		// Staged by pool-agent next to the rest of the pool's proxy material.
		MITMCA: buildkitagent.MITMCAPath,
		// Deliberately empty: reading a sandbox manifest here would be reading
		// another tenant's configuration, and there is exactly one pool-wide CA
		// to inject regardless of which sandbox owns the build.
		SandboxJSON: "",
	}
}

func main() {
	args := os.Args[1:]
	id := runcca.ContainerID(args)

	// Injection is best-effort, for the same reason it is in the sandbox: a
	// build step that starts without the CA fails only the TLS calls it makes,
	// whereas a step that fails to start breaks the user's build outright.
	switch {
	case runcca.IsDelete(args):
		if err := runcca.Cleanup(id, config()); err != nil {
			fmt.Fprintf(os.Stderr, "discobox-pool-runc: staged trust store not reclaimed: %v\n", err)
		}
	default:
		if bundle, ok := runcca.BundleDir(args); ok {
			if _, err := runcca.Adjust(bundle, id, config()); err != nil {
				fmt.Fprintf(os.Stderr, "discobox-pool-runc: proxy trust not injected: %v\n", err)
			}
		}
	}

	argv := append([]string{buildkitagent.RealRunc}, args...)
	//nolint:gosec // Passing this process's own argv through to the real runc is the entire point of the shim.
	if err := syscall.Exec(buildkitagent.RealRunc, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "discobox-pool-runc: exec %s: %v\n", buildkitagent.RealRunc, err)
		os.Exit(127)
	}
}
