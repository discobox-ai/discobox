package filter

import (
	"net"
	"slices"

	"github.com/obot-platform/discobox/proxy/internal/rules"
)

// Config controls destination filtering.
type Config struct {
	Enabled bool
	Domains []string
	IPs     []string
	Rules   []Rule
}

// Rule scopes allowlist entries to client IDs.
type Rule struct {
	ClientIDs []string
	Domains   []string
	IPs       []string
}

// Filter manages destination allowlisting.
type Filter struct {
	enabled bool
	domains []string
	cidrs   []*net.IPNet
	ips     []net.IP
	rules   []compiledRule
}

type compiledRule struct {
	clientIDs map[string]struct{}
	domains   []string
	cidrs     []*net.IPNet
	ips       []net.IP
}

// New creates a filter from config.
func New(cfg Config) *Filter {
	f := &Filter{enabled: cfg.Enabled, domains: slices.Clone(cfg.Domains)}
	for _, value := range cfg.IPs {
		addIPRule(value, &f.cidrs, &f.ips)
	}
	for _, rule := range cfg.Rules {
		compiled := compiledRule{
			clientIDs: make(map[string]struct{}, len(rule.ClientIDs)),
			domains:   slices.Clone(rule.Domains),
		}
		for _, clientID := range rule.ClientIDs {
			compiled.clientIDs[clientID] = struct{}{}
		}
		for _, value := range rule.IPs {
			addIPRule(value, &compiled.cidrs, &compiled.ips)
		}
		f.rules = append(f.rules, compiled)
	}
	return f
}

// AllowHost reports whether host is allowed.
func (f *Filter) AllowHost(host string) bool {
	return f.AllowHostForClient(host, "")
}

// AllowHostForClient reports whether host is allowed for clientID.
func (f *Filter) AllowHostForClient(host, clientID string) bool {
	if f == nil || !f.enabled {
		return true
	}
	if len(f.domains) == 0 && len(f.cidrs) == 0 && len(f.ips) == 0 && len(f.rules) == 0 {
		return false
	}
	hostOnly, _, err := net.SplitHostPort(host)
	if err != nil {
		hostOnly = host
	}
	if ip := net.ParseIP(hostOnly); ip != nil {
		return f.allowIP(ip) || f.allowClientIP(clientID, ip)
	}
	for _, pattern := range f.domains {
		if rules.MatchDomain(pattern, hostOnly) {
			return true
		}
	}
	for _, rule := range f.rules {
		if !rule.matchesClient(clientID) {
			continue
		}
		for _, pattern := range rule.domains {
			if rules.MatchDomain(pattern, hostOnly) {
				return true
			}
		}
	}
	return false
}

func (f *Filter) allowIP(ip net.IP) bool {
	for _, allowed := range f.ips {
		if allowed.Equal(ip) {
			return true
		}
	}
	for _, cidr := range f.cidrs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func (f *Filter) allowClientIP(clientID string, ip net.IP) bool {
	for _, rule := range f.rules {
		if rule.matchesClient(clientID) && rule.allowIP(ip) {
			return true
		}
	}
	return false
}

func (r compiledRule) matchesClient(clientID string) bool {
	if len(r.clientIDs) == 0 {
		return true
	}
	_, ok := r.clientIDs[clientID]
	return ok
}

func (r compiledRule) allowIP(ip net.IP) bool {
	for _, allowed := range r.ips {
		if allowed.Equal(ip) {
			return true
		}
	}
	for _, cidr := range r.cidrs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func addIPRule(value string, cidrs *[]*net.IPNet, ips *[]net.IP) {
	if _, cidr, err := net.ParseCIDR(value); err == nil {
		*cidrs = append(*cidrs, cidr)
	} else if ip := net.ParseIP(value); ip != nil {
		*ips = append(*ips, ip)
	}
}
