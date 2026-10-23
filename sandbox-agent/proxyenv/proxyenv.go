// Package proxyenv renders the sandbox's proxy-trust environment as a systemd
// EnvironmentFile, for units systemd starts directly rather than sandbox-agent
// spawning them.
//
// Three consumers, and the rule is the same for all of them: a unit systemd
// starts inherits none of the sandbox's proxy env, and the traffic that
// matters is its own rather than a container's, so no container-level
// injection (the runc wrapper of docs/adr/0020) can reach it either.
//
//   - dockerd resolves and pulls images itself, before any container exists.
//     Without this file every image pull inside a sandbox fails to resolve its
//     registry: the sandbox has no route off-box, and all egress must cross
//     the pool proxy.
//   - nix-daemon is the store's only writer, so the `nix` a user runs
//     evaluates locally and hands every substitution and build to it. The
//     user's shell has the proxy env; the process doing the downloading does
//     not, and `nix develop` fails with "Could not resolve host:
//     cache.nixos.org".
//   - the Xfce session (xfce4-session@.service) is started by X coming up, and
//     every program launched from the desktop inherits its environment.
//     Without the proxy env, Chromium from the desktop icon connects directly
//     and gets DNS_PROBE_FINISHED_NO_INTERNET.
//
// The MITM bundle rides the same file (SSL_CERT_FILE), which is what nix needs
// on top of the proxy URL — it reads NIX_SSL_CERT_FILE, then SSL_CERT_FILE,
// before falling back to a fixed path list.
//
// The source of truth is sandbox.json's Env/ProxyEnvs (ADR 0015 decisions 2-3:
// the env map as the single naming point). This package derives the
// EnvironmentFile from it rather than pool-agent rendering one directly,
// because sandbox-agent is the side that can resolve
// sandboxconfig.LocalSubnetsToken: pool-agent cannot know a sandbox's own
// directly-connected networks (Docker allocates them), and generating the file
// out here — where a plain read of sandbox.json already sits, unlike pool-agent
// which would have to reach back into a running sandbox to ask — keeps that
// resolution in the one place that already owns it (see runcca.proxyEnv, which
// resolves the same token for nested containers).
package proxyenv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/sandbox-agent/nestedbridge"
)

// DefaultSandboxJSON is the sandbox's static bootstrap manifest.
const DefaultSandboxJSON = "/etc/discobox/sandbox.json"

// DefaultOutputPath is where the rendered file lands. It is under /run rather
// than beside sandbox.json: this is boot-time runtime state derived from it,
// not configuration.
const DefaultOutputPath = "/run/discobox/proxy/proxy.env"

// Render reads sandboxJSONPath and returns its proxy-trust env subset (the
// keys named in ProxyEnvs) as a systemd EnvironmentFile: one KEY="value" line
// per variable, sorted so the output is byte-stable across regenerations.
// sandboxconfig.LocalSubnetsToken is resolved against this sandbox's own
// directly-connected networks, the same substitution runcca.proxyEnv applies
// for nested containers.
//
// It returns (nil, nil) when the sandbox declares no proxy-trust vars (no MITM
// proxy configured) or sandboxJSONPath does not exist, so callers can treat a
// nil result as "no file to write" rather than an error.
func Render(sandboxJSONPath string) ([]byte, error) {
	vars, err := Vars(sandboxJSONPath)
	if err != nil {
		return nil, err
	}
	return render(vars), nil
}

// render is vars as an EnvironmentFile, nil when there are none.
func render(vars map[string]string) []byte {
	if len(vars) == 0 {
		return nil
	}
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s=%s\n", name, strconv.Quote(vars[name]))
	}
	return []byte(b.String())
}

// Vars is sandboxJSONPath's proxy-trust env, the keys named in ProxyEnvs
// that Env sets, with sandboxconfig.LocalSubnetsToken resolved: what Render
// writes, and what the agent takes into its own environment so that what it
// provisions reaches the network through the pool. It is empty when the
// sandbox declares none or sandboxJSONPath does not exist.
func Vars(sandboxJSONPath string) (map[string]string, error) {
	data, err := os.ReadFile(sandboxJSONPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sandbox config %s: %w", sandboxJSONPath, err)
	}
	var manifest sandboxconfig.Config
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse sandbox config %s: %w", sandboxJSONPath, err)
	}
	return Resolve(manifest.Env, manifest.ProxyEnvs), nil
}

// Resolve is the variables of env that names names, with
// sandboxconfig.LocalSubnetsToken resolved against this box's own
// directly-connected networks; nil when names is empty.
func Resolve(env map[string]string, names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	subnets := nestedbridge.LocalSubnets()
	vars := map[string]string{}
	for _, name := range names {
		if value, ok := env[name]; ok {
			vars[name] = sandboxconfig.ResolveLocalSubnetsToken(value, subnets)
		}
	}
	return vars
}

// WriteFile renders sandboxJSONPath's proxy-trust env to outPath. When Render
// produces nothing (no proxy-trust vars declared), any stale file at outPath
// from a previous boot is removed rather than left behind, and outPath's
// absence is not an error: the EnvironmentFile in each consumer
// (docker.service.d/proxy.conf, nix-daemon.service.d/proxy.conf,
// xfce4-session@.service) is optional (a leading `-`) for exactly this case.
func WriteFile(sandboxJSONPath, outPath string) error {
	content, err := Render(sandboxJSONPath)
	if err != nil {
		return err
	}
	return write(content, outPath)
}

// WriteVars writes vars, already resolved (Resolve), to outPath as WriteFile
// does: no vars removes the file. A snapshot's build, which has no
// sandbox.json, renders its plan's env with it.
func WriteVars(vars map[string]string, outPath string) error {
	return write(render(vars), outPath)
}

func write(content []byte, outPath string) error {
	if content == nil {
		if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale %s: %w", outPath, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	tmp := outPath + ".tmp"
	//nolint:gosec // proxy URLs and bundle paths are not secret; matches proxyagent's 0644 material.
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", outPath, err)
	}
	return nil
}
