// Package proxyenv renders the sandbox's proxy-trust environment as a systemd
// EnvironmentFile, for units systemd starts directly rather than sandbox-agent
// spawning them.
//
// dockerd is the one that matters: it resolves and pulls images itself, before
// any container exists, so no container-level injection (the runc wrapper of
// docs/adr/0020) can reach it, and it is started by docker.socket activation
// rather than spawned by sandbox-agent, so it inherits none of the sandbox's
// proxy env either way. Without this file, every image pull inside a sandbox
// fails to resolve its registry: the sandbox has no route off-box, and all
// egress must cross the pool proxy.
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

	"github.com/obot-platform/discobox/sandbox-agent/nestedbridge"
	"github.com/obot-platform/discobox/sandboxconfig"
)

// DefaultSandboxJSON is pool-agent's read-only sandbox manifest.
const DefaultSandboxJSON = "/etc/discobox/sandbox.json"

// DefaultOutputPath is where the rendered file lands. It is under /run rather
// than beside sandbox.json: /etc/discobox is pool-agent's read-only mount, and
// this is boot-time runtime state derived from it, not configuration.
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
	if len(manifest.ProxyEnvs) == 0 {
		return nil, nil
	}

	subnets := nestedbridge.LocalSubnets()
	names := append([]string{}, manifest.ProxyEnvs...)
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		value, ok := manifest.Env[name]
		if !ok {
			continue
		}
		value = sandboxconfig.ResolveLocalSubnetsToken(value, subnets)
		fmt.Fprintf(&b, "%s=%s\n", name, strconv.Quote(value))
	}
	if b.Len() == 0 {
		return nil, nil
	}
	return []byte(b.String()), nil
}

// WriteFile renders sandboxJSONPath's proxy-trust env to outPath. When Render
// produces nothing (no proxy-trust vars declared), any stale file at outPath
// from a previous boot is removed rather than left behind, and outPath's
// absence is not an error: docker.service.d/proxy.conf's EnvironmentFile
// reference is optional (a leading `-`) for exactly this case.
func WriteFile(sandboxJSONPath, outPath string) error {
	content, err := Render(sandboxJSONPath)
	if err != nil {
		return err
	}
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
