// Command discobox-pool-runc installs as `runc` on the directory prepended to
// buildkitd's PATH, and is what buildkitd is pointed at by --oci-worker-binary.
// It seeds the pool's MITM CA into every build step's trust stores, binds that
// step's egress to the sandbox that asked for the build, then execs the real
// runc.
//
// It is the pool-side half of ADR 0020's wrapper, sharing the root `runcca`
// package with the sandbox rather than forking it. Two things differ, and both
// are parameters rather than code:
//
//   - the real runc is BuildKit's own `buildkit-runc`, not `/usr/bin/runc`
//   - there is no sandbox manifest out here; a build's proxy variables arrive
//     as build-args the mediator injected and are already in the spec
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/obot-platform/discobox/pool-agent/buildkitagent"
	"github.com/obot-platform/discobox/runcca"
)

// ociState is what runc feeds a hook on stdin.
type ociState struct {
	ID  string `json:"id"`
	Pid int    `json:"pid"`
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "build-forwarder" {
		os.Exit(runForwarderHook(args[1:]))
	}

	id := runcca.ContainerID(args)
	switch {
	case runcca.IsDelete(args):
		if err := runcca.Cleanup(id, config(nil)); err != nil {
			fmt.Fprintf(os.Stderr, "discobox-pool-runc: staged trust store not reclaimed: %v\n", err)
		}
	default:
		if bundle, ok := runcca.BundleDir(args); ok {
			// Injection is best-effort, for the same reason it is in the
			// sandbox: a build step that starts without the CA fails only the
			// TLS calls it makes, whereas one that fails to start breaks the
			// user's build outright.
			if _, err := runcca.Adjust(bundle, id, config(hooksFor(bundle))); err != nil {
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

func config(hooks map[string][]string) runcca.Config {
	return runcca.Config{
		MITMCA: buildkitagent.MITMCAPath,
		// Deliberately empty: reading a sandbox manifest here would be reading
		// another tenant's configuration, and there is exactly one pool-wide CA
		// to inject regardless of which sandbox owns the build.
		SandboxJSON: "",
		Hooks:       hooks,
		RewriteEnv: func(name, value string) string {
			if strings.EqualFold(name, "HTTP_PROXY") || strings.EqualFold(name, "http_proxy") {
				return buildkitagent.StripProxyIdentity(value)
			}
			return value
		},
	}
}

// hooksFor returns the hooks that bind this build step's egress to its owning
// sandbox, or nil when the step carries no sandbox identity.
//
// The identity rides in the proxy URL the mediator injected, as userinfo. It is
// not a credential: the forwarder it names listens only inside this step's own
// network namespace, so the string is inert anywhere else.
func hooksFor(bundle string) map[string][]string {
	sandboxID := sandboxFromSpec(bundle)
	if sandboxID == "" {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	return map[string][]string{
		"createRuntime": {self, "build-forwarder", "create", sandboxID},
		"poststop":      {self, "build-forwarder", "stop"},
	}
}

// sandboxFromSpec reads the owning sandbox out of the spec's proxy variables.
func sandboxFromSpec(bundle string) string {
	raw, err := os.ReadFile(bundle + "/config.json")
	if err != nil {
		return ""
	}
	var spec struct {
		Process struct {
			Env []string `json:"env"`
		} `json:"process"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return ""
	}
	for _, entry := range spec.Process.Env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !strings.EqualFold(name, "HTTP_PROXY") {
			continue
		}
		return buildkitagent.SandboxFromProxyURL(value)
	}
	return ""
}

// runForwarderHook dispatches the three forwarder modes. Failures are reported
// and stepped over for create, since a build without egress is better than a
// build that will not start.
func runForwarderHook(args []string) int {
	if len(args) == 0 {
		return 0
	}
	ctx := context.Background()
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return 0
		}
		var state ociState
		if err := json.NewDecoder(os.Stdin).Decode(&state); err != nil {
			fmt.Fprintf(os.Stderr, "discobox-pool-runc: read hook state: %v\n", err)
			return 0
		}
		if err := buildkitagent.StartBuildForwarder(ctx, state.ID, args[1], state.Pid); err != nil {
			fmt.Fprintf(os.Stderr, "discobox-pool-runc: build egress unavailable: %v\n", err)
		}
	case "serve":
		if len(args) < 4 {
			return 1
		}
		pid, err := strconv.Atoi(args[3])
		if err != nil {
			return 1
		}
		if err := buildkitagent.ServeBuildForwarder(ctx, args[1], args[2], pid); err != nil {
			fmt.Fprintf(os.Stderr, "discobox-pool-runc: build forwarder: %v\n", err)
			return 1
		}
	case "stop":
		var state ociState
		if err := json.NewDecoder(os.Stdin).Decode(&state); err != nil {
			return 0
		}
		if err := buildkitagent.StopBuildForwarder(state.ID); err != nil {
			fmt.Fprintf(os.Stderr, "discobox-pool-runc: build forwarder not reclaimed: %v\n", err)
		}
	}
	return 0
}
