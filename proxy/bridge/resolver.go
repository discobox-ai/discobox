package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"
)

// ProviderResolverPath survives reboots and is outside the intake's material
// directory. Every bridge reads it afresh at each dial (ADR 26-11-12-355).
const ProviderResolverPath = "/var/lib/discobox/provider-resolver.json"

// ProviderResolver is the resolver displaced by the sandbox DNS stub. Content
// includes the provider's search/options; Symlink is empty for a regular file.
type ProviderResolver struct {
	Content string `json:"content"`
	Symlink string `json:"symlink,omitempty"`
}

// LoadProviderResolver reads a durable record. Absence means the system's
// resolver still owns DNS; a corrupt record must never fall back to the stub.
func LoadProviderResolver(path string) (ProviderResolver, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ProviderResolver{}, err
	}
	var record ProviderResolver
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("provider resolver: %w", err)
	}
	return record, nil
}

// Nameservers returns valid IP nameservers other than the sandbox's stub.
func (r ProviderResolver) Nameservers() []string {
	var servers []string
	for line := range strings.SplitSeq(r.Content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ip, err := netip.ParseAddr(fields[1])
		if err == nil && ip.String() != "169.254.53.53" {
			servers = append(servers, ip.String())
		}
	}
	return servers
}

// providerDialContext resolves the transport host directly against the saved
// provider nameservers, independently of libc and of the stub's lifetime.
func providerDialContext(recordPath string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		direct := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if _, err := netip.ParseAddr(host); err == nil {
			return direct.DialContext(ctx, network, address)
		}
		record, err := LoadProviderResolver(recordPath)
		if errors.Is(err, os.ErrNotExist) {
			return direct.DialContext(ctx, network, address)
		}
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var failures []error
		for _, server := range record.Nameservers() {
			viaProvider := direct
			viaProvider.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return direct.DialContext(ctx, network, net.JoinHostPort(server, "53"))
			}}
			// Keep net.Dialer's address deadlines and Happy Eyeballs fallback.
			// The trailing dot avoids the sandbox's search domains; TLS still
			// verifies the original pool/edge name in the caller.
			conn, err := viaProvider.DialContext(ctx, network, net.JoinHostPort(strings.TrimSuffix(host, ".")+".", port))
			if err == nil {
				return conn, nil
			}
			failures = append(failures, err)
		}
		if len(failures) == 0 {
			return nil, fmt.Errorf("provider resolver record has no nameservers for %s", host)
		}
		return nil, errors.Join(failures...)
	}
}
