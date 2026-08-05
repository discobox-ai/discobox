// Package nestedbridge discovers the address of the sandbox's nested-Docker
// bridge and publishes it for the rest of the sandbox to use.
//
// The sandbox's daemon.json deliberately sets no "bip": dockerd already picks a
// default-bridge subnet that does not overlap anything it can see, and pinning
// one would claim a fixed slice of RFC1918 space in every sandbox at every
// nesting level — colliding with whatever the surrounding environment happens
// to route, and burning one of Docker's fifteen default /16s per level.
//
// The cost of letting dockerd choose is that the bridge-facing proxy forwarder
// can no longer pre-bind a known address before docker0 exists (which is what
// docs/adr/0015 decision 7's ListenStream + FreeBind socket unit did). Instead
// the forwarder is pulled up alongside dockerd, waits for the bridge to appear,
// binds whatever address dockerd chose, and publishes it here so the runc
// wrapper injects a proxy address that is real rather than assumed.
package nestedbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	// DefaultInterface is the nested Docker daemon's default bridge.
	DefaultInterface = "docker0"
	// Port is the sandbox-local port both proxy forwarders listen on. It only
	// has to be self-consistent within a sandbox: the address containers are
	// told comes from the published file, not from a shared constant.
	Port = 17008
	// DefaultPublishPath is where the bound address is published. It lives
	// under /run because it describes this boot's runtime state, not
	// configuration: dockerd may pick a different subnet next boot.
	DefaultPublishPath = "/run/discobox/proxy/nested-forwarder.json"
	// pollInterval is how often to re-check for the bridge while waiting.
	pollInterval = 250 * time.Millisecond
)

// published is the on-disk shape, matching the bridge configs pool-agent
// writes so consumers can read either with the same struct.
type published struct {
	ListenAddress string `json:"listenAddress"`
}

// ErrNoAddress reports that the interface exists but carries no IPv4 address.
var ErrNoAddress = errors.New("bridge has no IPv4 address")

// WaitForAddress blocks until the named interface has an IPv4 address, and
// returns it as a host:port listen address.
//
// dockerd creates the bridge asynchronously after it starts, so the unit that
// runs this is pulled up alongside dockerd rather than ordered after it, and
// waits here for the interface to appear.
func WaitForAddress(ctx context.Context, iface string, timeout time.Duration) (string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		if addr, err := address(iface); err == nil {
			return net.JoinHostPort(addr, fmt.Sprint(Port)), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", fmt.Errorf("interface %s did not get an IPv4 address within %s", iface, timeout)
		case <-ticker.C:
		}
	}
}

// address returns the interface's first IPv4 address.
func address(iface string) (string, error) {
	link, err := net.InterfaceByName(iface)
	if err != nil {
		return "", err
	}
	addrs, err := link.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String(), nil
		}
	}
	return "", ErrNoAddress
}

// Publish records the bound address for other components in this sandbox.
// Writing is atomic so a reader never observes a half-written file.
func Publish(path, listenAddress string) error {
	if path == "" {
		path = DefaultPublishPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	body, err := json.Marshal(published{ListenAddress: listenAddress})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil { //nolint:gosec // the bridge address is not secret; containers are told it.
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// PublishedAddress returns the address recorded by Publish, or "" when none has
// been recorded yet. Callers treat "" as "no nested forwarder available" and
// degrade rather than substituting a guess.
func PublishedAddress(path string) string {
	if path == "" {
		path = DefaultPublishPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var p published
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	return p.ListenAddress
}

// LocalSubnets returns the CIDR of every directly-connected IPv4 network on
// this host, which is what sandboxconfig.LocalSubnetsToken stands in for.
//
// It is enumerated at use time rather than at boot because the set grows: the
// nested Docker bridge does not exist until dockerd first starts, and every
// user-created Docker network adds another. A caller resolving the token when
// it materializes a container's environment therefore sees the networks that
// actually exist by then.
//
// Loopback is skipped: it is already named literally in the lists this feeds,
// and expressing it as a /8 would exempt more than intended.
func LocalSubnets() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			// Mask the address down to its network so the entry is the subnet
			// this host sits on, not the single address it holds.
			cidr := (&net.IPNet{IP: ipnet.IP.Mask(ipnet.Mask), Mask: ipnet.Mask}).String()
			if _, dup := seen[cidr]; dup {
				continue
			}
			seen[cidr] = struct{}{}
			out = append(out, cidr)
		}
	}
	sort.Strings(out) // deterministic, so injected env is stable across runs
	return out
}
